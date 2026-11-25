# Running printscan on Kubernetes

This guide covers what a cluster needs before `deploy/base` works, and the values in it that you have to change. Names in angle brackets, such as `<node>`, are placeholders.

## USB passthrough to the node

The printer has to be visible to the node that runs the device pod. On bare metal, plug it in. On a virtual machine, pass the USB device through to the VM in your hypervisor, preferably by vendor and product ID so it survives a port change. Then check that the node sees it, for example in `/sys/bus/usb/devices/*/idVendor` or with `lsusb` on the node.

Note the printer's USB vendor and product IDs (4 hex digits each). The device plugin below needs them.

## Device plugin

printscan does not mount `/dev`. It asks for the printer as an extended resource, which [generic-device-plugin](https://github.com/squat/generic-device-plugin) provides. `deploy/examples/generic-device-plugin.yaml` is a DaemonSet you can start from:

- Replace `VVVV` and `PPPP` in its `--device` argument with the printer's vendor and product IDs. Add `serial` if you have two identical printers.
- Keep `"count": 3`: up to three containers of the device pod request the printer at the same time.
- The resource name is `<domain>/<device name>`, `devic.es/printer` in the example. Set `PRINTSCAN_USB_RESOURCE` on the control plane to the same string. If they differ, the device pod stays Pending, and printscan cannot tell that from an unplugged printer.

The device plugin creates the device node owned by root, so the containers that open it run as uid 0. A udev rule on the node that changes the node's owner is optional.

## Namespace and admission policy

`deploy/base/namespace.yaml` creates the namespace `printing` with the `privileged` Pod Security level. The device pod needs that level only for `hostNetwork`, which puts mDNS and the IPP and eSCL ports on the node's LAN address. It mounts no host paths and runs no privileged container.

The `privileged` level allows much more than `hostNetwork`. `deploy/examples/admission-policy.yaml` takes the rest back: a ValidatingAdmissionPolicy, bound to the namespace `printing`, that allows `hostNetwork` and refuses privileged containers, `hostPath` volumes, `hostPID`, `hostIPC`, added capabilities and `allowPrivilegeEscalation: true`, in containers, init containers and ephemeral containers. It is cluster-scoped, so it is not part of `deploy/base`. Apply it yourself (Kubernetes 1.30 or newer):

```sh
kubectl apply -f deploy/examples/admission-policy.yaml
```

If you rename the namespace, change the binding's `namespaceSelector` too.

## Volumes

`deploy/base/pvc.yaml` has two ReadWriteOnce claims:

- `printscan-data` (2Gi) holds the config file and stored scans, and is mounted by the control plane. Use a volume that delivers inotify events: network RWX file systems do not, and the config watcher would miss edits.
- `printscan-blobs` (1Gi) holds vendor files, such as HP's plug-in, and is mounted by the device pod. The name is the default of `--blob-pvc` (`PRINTSCAN_BLOB_PVC`).

In probe and usb modes the device pod always mounts the blob volume, even for printers that need no vendor file. Its agent container uses it to download and check a vendor archive, so the wizard can show the vendor's licence before you accept it. The volume therefore has to be mountable on the node with the USB printer. A node-local ReadWriteOnce volume that was provisioned on another node leaves the device pod Pending. The device Deployment runs one replica with the Recreate strategy, so the old and new pod never hold the volume at the same time.

To run without a blob volume, pass the flag with an empty value in the control plane's container:

```yaml
args: ["serve", "--blob-pvc="]
```

Remove every `licenses.acceptedEulas` entry from the config first. With an accepted entry the device Deployment still names the blob volume, with an empty claim name, and Kubernetes rejects it. With no accepted entries, the device pod mounts no blob volume, so it cannot download licence texts or vendor files. The HP licence shows as failed in the wizard and Settings, and the plug-in cannot be accepted or installed. An empty `PRINTSCAN_BLOB_PVC` environment variable does not do this: printscan treats an empty variable as unset and uses `printscan-blobs`.

## Ingress, forward auth and allowed hosts

printscan has no login. Route only port 8080 (the Service's `http` port) through an ingress that authenticates every request, for example Traefik with Authentik forward auth. When the proxy sets `X-Authentik-Username`, print jobs carry that user name. Never route port 8081.

Two optional settings close the gaps a misconfigured proxy would leave. `deploy/base/deployment.yaml` has both as commented-out examples:

- `PRINTSCAN_ALLOWED_HOSTS` (flag `--allowed-hosts`) is a comma-separated list of host names the UI and API answer to, for example `printscan.example.com`. A request with any other `Host` header gets 421 with problem type `misdirected-request`, which stops DNS-rebinding pages from reaching the API. The port is ignored and case does not matter. `/healthz` and `/readyz` are exempt. On Kubernetes the default is no check. `*` turns the check off on any backend.
- `PRINTSCAN_PROXY_SECRET` is a shared secret that your proxy adds to every request, in the header `X-Printscan-Proxy-Secret` (change it with `PRINTSCAN_PROXY_SECRET_HEADER` or `--proxy-secret-header`). When it is set, an `/api/` request without the right header gets 401 with problem type `unauthorized`, so the API refuses requests that bypassed the proxy. The UI files, `/healthz`, `/readyz` and port 8081 are exempt. Take the secret from a Secret through `secretKeyRef`, or mount it as a file and set `--proxy-secret-file` (`PRINTSCAN_PROXY_SECRET_FILE`). There is no flag that takes the secret itself. Setting both the variable and the file, or giving an empty file, stops printscan at startup.

## NetworkPolicy

`deploy/base/networkpolicy.yaml` allows two kinds of traffic to the control plane. Two values in it are marked `# CHANGE ME`:

- `kubernetes.io/metadata.name: ingress-controller` is the namespace your ingress controller runs in. Port 8080 is allowed only from there.
- `192.0.2.0/24` is a documentation range that matches nothing. Replace it with your nodes' LAN subnet, or narrow it to the USB node's address as a `/32`.

Port 8081 takes reports from the agent in the device pod. That pod uses the host network, so its traffic does not come from a pod IP. Depending on the CNI it arrives with the node's LAN address or with the node's address inside the pod network (`10.244.0.0/16` in the example). The policy lists both. Find out which one your CNI uses, with a flow log or a temporary packet capture, and drop the other.

## The agent token on port 8081

The agent sends its reports to `http://printscan.<namespace>.svc:8081` (`--agent-url`) with a bearer token from the Secret `printscan-agent-token`, which the control plane creates. The connection is plain HTTP inside the cluster, so anyone who can capture traffic between the node and the control-plane pod can read the token and send false reports. The NetworkPolicy limits who can reach port 8081. Use a CNI with transparent encryption if your cluster network is shared with machines you do not trust.

## LAN-only IPP and eSCL

The device pod serves IPP, eSCL and mDNS on the node's LAN address, the way a network printer does. Keep these ports on the LAN: do not forward them to the internet. mDNS does not cross routers or VPNs, so clients on another subnet need the manual addresses from the Status page. See the Security section of the README for what these daemons expose.

## Image tag

Until the first release, `deploy/base/deployment.yaml` and `deploy/base/kustomization.yaml` both use the tag `main`, and the control-plane container has `imagePullPolicy: Always`. `main` moves with every push to the main branch, and the device pod already pulls `main` images Always, so the control plane and the agent speak the same protocol after a restart.

After the first release, set `newTag: <version>@sha256:<digest>` in `kustomization.yaml` and drop `imagePullPolicy: Always`. The device pod's probe, agent and fetch-blobs containers reuse the control-plane image string, digest included. Pin the device and driver images with `PRINTSCAN_IMAGE_OVERRIDES` (see Images in the README).

## Memory limit and GOMEMLIMIT

The control-plane container has a 256Mi memory limit and `GOMEMLIMIT: 200MiB`. When printscan converts a PNG for printing, it decodes up to 128 MiB of pixels at a time. GOMEMLIMIT makes the Go garbage collector work harder as the heap nears 200 MiB, which keeps the process under the limit instead of being OOM-killed. Keep GOMEMLIMIT about 50 MiB below `resources.limits.memory`, and change both together. If you lower the limit without lowering GOMEMLIMIT, large PNG prints can get the container killed.
