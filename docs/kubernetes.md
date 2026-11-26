# Running pagewire on Kubernetes

This guide covers what a cluster needs before `deploy/base` works, and the values in it that you have to change. Names in angle brackets, such as `<node>`, are placeholders.

## USB passthrough to the node

Each printer and scanner has to be visible to the node it is plugged into. On bare metal, plug it in. On a virtual machine, pass the USB device through to the VM in your hypervisor, preferably by vendor and product ID so it survives a port change. Then check that the node sees it, for example in `/sys/bus/usb/devices/*/idVendor` or with `lsusb` on the node.

Note each device's USB vendor and product IDs (4 hex digits each). The device plugin below needs them.

## Device plugin

pagewire does not mount `/dev`. It asks for the USB devices as an extended resource, which [generic-device-plugin](https://github.com/squat/generic-device-plugin) provides. `deploy/examples/generic-device-plugin.yaml` is a DaemonSet you can start from:

- Use one group that lists every printer and scanner pagewire may use, one `usb` entry each. Replace `VVVV:PPPP` and `WWWW:QQQQ` in its `--device` argument with the vendor and product IDs, and add or remove entries to match. Add `serial` to an entry if you have two identical devices.
- Use `"count": 8`. Every USB container of a device pod requests one unit: the probe, ipp-usb, one printer application per driver family in use (up to five) and AirSane, so 8 is always enough. A node with one printer on a printer application and its scanner on AirSane needs 3.
- The resource name is `<domain>/<device name>`, `devic.es/printer` in the example. Set `PAGEWIRE_USB_RESOURCE` on the control plane to the same string. If they differ, the device pod stays Pending, and pagewire cannot tell that from an unplugged device.

generic-device-plugin 0.2.0 hands out a group like this: on each node where at least one of its `usb` entries matches, it advertises `count` units of the resource, and every unit carries all of that node's matched devices; entries that match nothing on a node are skipped there. Nodes without a match advertise 0. So the resource picks the node but cannot pick one device among several: every USB container of a device pod sees every device of the group on its node. That is why pagewire runs one device pod per node, and why a device you tell pagewire not to use still reaches the daemons:

- "Don't use" in the wizard or Settings only stops pagewire configuring the device. ipp-usb serves every IPP-over-USB device it can open, and AirSane exports every device whose SANE backend is loaded, so such a device may still show up on the LAN. Leave a device you do not want served out of the device plugin group.
- Per-container device isolation exists only with the docker backend.

The device plugin creates the device nodes owned by root, so the containers that open them run as uid 0. A udev rule on the node that changes the nodes' owner is optional.

### After a replug or power cycle

A device that is plugged in again gets a new device node, and the device plugin hands each pod the nodes that existed when the pod started. pagewire tracks this per printer and scanner, and restarts the node's device pod once, about 30 s after a device shows up at a new node:

- the device moved to another bus or address;
- the pod's allocation does not hold the device's node;
- a device that is in use appeared on the node after its pod started, for example when it was switched off at the time.

pagewire restarts at most once per change and device. The restart covers the whole device pod of that node, so a print or scan running on another device of the same node is interrupted. Switching a device off never restarts anything: pagewire keeps its entry in the pod until it comes back, so the other devices keep working. If the new pod still does not hold the device's node, because the device plugin's group does not list it or the plugin has not noticed the change, that device's status shows "The printer's USB connection changed and the device pod still has the old one. pagewire restarted it once; check the device plugin" instead of restarting again.

## Devices on several nodes

pagewire runs one device workload per node with USB devices: a Deployment and a ConfigMap named `pagewire-device`, `pagewire-device-1`, `pagewire-device-2` and `pagewire-device-3`. That is at most 4 nodes with devices; with more, the extra nodes are not served and the Status page names them under reconcile. Each workload holds that node's printers and scanners, because each node can run only one Avahi.

To find those nodes, pagewire lists the cluster's nodes and takes those whose allocatable `PAGEWIRE_USB_RESOURCE` is above 0. Listing nodes needs the ClusterRole `pagewire-nodes` in `deploy/base/clusterrole.yaml` and its binding in `deploy/base/clusterrolebinding.yaml`. `deploy/base` applies both; they grant only `list` and `watch` on nodes, and you need them only for devices on more than one node:

- Without the ClusterRole, the node list is refused and pagewire runs one device workload, `pagewire-device`, not pinned to any node, so Kubernetes schedules it on the node that advertises the resource. It logs "listing nodes failed; running one device workload (single-node mode)" once, and tries the node list again every 5 minutes. To run this way, remove both files from `deploy/base/kustomization.yaml`.
- Any other node-list error, such as a timeout or an API server restart, counts as temporary: pagewire keeps its device workloads as they are and changes none of them until the list works again.
- With the ClusterRole, each workload is pinned to its node by node name, through `nodeAffinity` on `metadata.name`. The Deployment carries the node name in its annotation `pagewire.thephaseless.github.io/node`, which a restarted control plane reads to find its slots again.

Apply the ClusterRole and the updated Role (`deploy/base/role.yaml`, which names all four slots) together. Turning on multi-node mode on a running install pins `pagewire-device` to its node, which rolls its pod once.

If you remove the ClusterRole after running on several nodes, pagewire goes back to one workload and no longer touches `pagewire-device-1` to `-3`, which keep running. Delete them and their ConfigMaps by hand:

```sh
kubectl -n printing delete deployment pagewire-device-1 pagewire-device-2 pagewire-device-3 --ignore-not-found
kubectl -n printing delete configmap pagewire-device-1 pagewire-device-2 pagewire-device-3 --ignore-not-found
```

The vendor-file volume is ReadWriteOnce, so only one device workload mounts it: the first, in node name order, whose printers need or offer a vendor file, else the first node's. With a node-local volume, make sure it can be mounted on that node. If printers on two nodes need one, the second node gets no vendor files and Status shows an error under reconcile.

## RBAC

`deploy/base/role.yaml` is limited to the namespace, and every rule names its objects except pods. It lets the control plane read its own Deployment (`pagewire`) and update its finalizers; read, create, patch and delete the device Deployments and ConfigMaps (`pagewire-device` and `pagewire-device-1` to `-3`); read, create and patch the agent token Secret (`pagewire-agent-token`); and list, watch and delete pods, whose names are generated. `resourceNames` match exactly, which is why the slot names are a fixed set. If you change `PAGEWIRE_K8S_OWNER`, change the `pagewire` resource name in the Role too. The optional ClusterRole adds `list` and `watch` on nodes, cluster-wide, and nothing else. pagewire watches the device pods and nodes to react to changes at once. With an older Role or ClusterRole that lacks `watch`, it falls back to re-checking every 30 s and says so under reconcile on the Status page.

## Namespace and admission policy

`deploy/base/namespace.yaml` creates the namespace `printing` with the `privileged` Pod Security level. The device pod needs that level only for `hostNetwork`, which puts mDNS and the IPP and eSCL ports on the node's LAN address. It mounts no host paths and runs no privileged container.

The `privileged` level allows much more than `hostNetwork`. `deploy/examples/admission-policy.yaml` takes the rest back: a ValidatingAdmissionPolicy, bound to the namespace `printing`, that allows `hostNetwork` and refuses privileged containers, `hostPath` volumes, `hostPID`, `hostIPC`, added capabilities, `allowPrivilegeEscalation: true`, containers that do not drop `ALL` capabilities, and `Unconfined` seccomp or AppArmor profiles on the pod or a container. It covers containers, init containers and ephemeral containers, so `kubectl debug` into the namespace needs `--profile=restricted`. It is cluster-scoped, so it is not part of `deploy/base`. Apply it yourself (Kubernetes 1.30 or newer):

```sh
kubectl apply -f deploy/examples/admission-policy.yaml
```

If you rename the namespace, change the binding's `namespaceSelector` too.

## Volumes

`deploy/base/pvc.yaml` has two ReadWriteOnce claims:

- `pagewire-data` (2Gi) holds the config file and stored scans, and is mounted by the control plane. Use a volume that delivers inotify events. Network RWX file systems do not; on a network or FUSE volume pagewire notices it and re-reads the config file every 30 s instead.
- `pagewire-blobs` (1Gi) holds vendor files, such as HP's plug-in, and is mounted by one device pod (see [Devices on several nodes](#devices-on-several-nodes)). The name is the default of `--blob-pvc` (`PAGEWIRE_BLOB_PVC`).

In probe and usb modes that device pod always mounts the blob volume, even for printers that need no vendor file. Its agent container uses it to download and check a vendor archive, so the wizard can show the vendor's licence before you accept it. The volume therefore has to be mountable on the node that runs that pod: with one node, the node with the USB devices; with several, see [Devices on several nodes](#devices-on-several-nodes). A node-local ReadWriteOnce volume that was provisioned on another node leaves the device pod Pending. The device Deployment runs one replica with the Recreate strategy, so the old and new pod never hold the volume at the same time.

To run without a blob volume, pass the flag with an empty value in the control plane's container:

```yaml
args: ["serve", "--blob-pvc="]
```

Remove every `licenses.acceptedEulas` entry from the config first. With an accepted entry the device Deployment still names the blob volume, with an empty claim name, and Kubernetes rejects it. With no accepted entries, the device pod mounts no blob volume, so it cannot download licence texts or vendor files. The HP licence shows as failed in the wizard and Settings, and the plug-in cannot be accepted or installed. An empty `PAGEWIRE_BLOB_PVC` environment variable does not do this: pagewire treats an empty variable as unset and uses `pagewire-blobs`.

## Ingress, forward auth and allowed hosts

pagewire has no login. Route only port 8080 (the Service's `http` port) through an ingress that authenticates every request, for example Traefik with Authentik forward auth. When the proxy sets `X-Authentik-Username`, print jobs carry that user name. Never route port 8081.

Two optional settings close the gaps a misconfigured proxy would leave. `deploy/base/deployment.yaml` has both as commented-out examples:

- `PAGEWIRE_ALLOWED_HOSTS` (flag `--allowed-hosts`) is a comma-separated list of host names the UI and API answer to, for example `pagewire.example.com`. A request with any other `Host` header gets 421 with problem type `misdirected-request`, which stops DNS-rebinding pages from reaching the API. The port is ignored and case does not matter. `/healthz` and `/readyz` are exempt. On Kubernetes the default is no check. `*` turns the check off on any backend.
- `PAGEWIRE_PROXY_SECRET` is a shared secret that your proxy adds to every request, in the header `X-Pagewire-Proxy-Secret` (change it with `PAGEWIRE_PROXY_SECRET_HEADER` or `--proxy-secret-header`). When it is set, an `/api/` request without the right header gets 401 with problem type `unauthorized`, so the API refuses requests that bypassed the proxy. The UI files, `/healthz`, `/readyz` and port 8081 are exempt. Take the secret from a Secret through `secretKeyRef`, or mount it as a file and set `--proxy-secret-file` (`PAGEWIRE_PROXY_SECRET_FILE`). There is no flag that takes the secret itself. Setting both the variable and the file, or giving an empty file, stops pagewire at startup.

## NetworkPolicy

`deploy/base/networkpolicy.yaml` allows two kinds of traffic to the control plane. Two values in it are marked `# CHANGE ME`:

- `kubernetes.io/metadata.name: ingress-controller` is the namespace your ingress controller runs in. Port 8080 is allowed only from there.
- `192.0.2.0/24` is a documentation range that matches nothing. Replace it with your nodes' LAN subnet, or narrow it to the address of every node with USB devices, one `/32` each. Each of those nodes runs a device pod whose agent reports to port 8081, so leave none of them out.

Port 8081 takes reports from the agent in the device pod. That pod uses the host network, so its traffic does not come from a pod IP. Depending on the CNI it arrives with the node's LAN address or with the node's address inside the pod network (`10.244.0.0/16` in the example). The policy lists both. Find out which one your CNI uses, with a flow log or a temporary packet capture, and drop the other.

## The agent token on port 8081

The agent sends its reports to `http://pagewire.<namespace>.svc:8081` (`--agent-url`) with a bearer token from the Secret `pagewire-agent-token`, which the control plane creates. The connection is plain HTTP inside the cluster, so anyone who can capture traffic between the node and the control-plane pod can read the token and send false reports. The NetworkPolicy limits who can reach port 8081. Use a CNI with transparent encryption if your cluster network is shared with machines you do not trust.

## LAN-only IPP and eSCL

The device pod serves IPP, eSCL and mDNS on the node's LAN address, the way a network printer does. Keep these ports on the LAN: do not forward them to the internet. mDNS does not cross routers or VPNs, so clients on another subnet need the manual addresses from the Status page. See the Security section of the README for what these daemons expose.

## Image tag

Until the first release, `deploy/base/deployment.yaml` and `deploy/base/kustomization.yaml` both use the tag `main`, and the control-plane container has `imagePullPolicy: Always`. `main` moves with every push to the main branch, and the device pod already pulls `main` images Always, so the control plane and the agent speak the same protocol after a restart.

After the first release, set `newTag: <version>@sha256:<digest>` in `kustomization.yaml` and drop `imagePullPolicy: Always`. The device pod's probe, agent and fetch-blobs containers reuse the control-plane image string, digest included. Pin the device and driver images with `PAGEWIRE_IMAGE_OVERRIDES` (see Images in the README).

## Memory limit and GOMEMLIMIT

The control-plane container has a 256Mi memory limit. At start, pagewire reads that limit from the cgroup (`/sys/fs/cgroup/memory.max`, cgroup v2) and sets the Go memory limit to 80% of it, about 205 MiB by default. When pagewire converts a PNG for printing, it decodes up to 128 MiB of pixels at a time. The Go memory limit makes the garbage collector work harder as the heap nears it, and the remaining 20% covers memory the cgroup counts but Go does not, such as kernel buffers and page tables. To change it, change only `resources.limits.memory`. Setting `GOMEMLIMIT` on the container overrides the 80% rule, and on a node that still uses cgroup v1 pagewire sets no limit unless you set `GOMEMLIMIT`.
