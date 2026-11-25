# printscan

printscan turns one USB multifunction printer into a network printer and scanner that phones, tablets and computers find by themselves, plus a web page for printing and scanning from any browser. It runs on Kubernetes or on a single Docker or Podman host, and drives the OpenPrinting daemons (ipp-usb, PAPPL printer applications, AirSane, Avahi) for you. You describe the printer in one YAML file, or let the setup wizard write it.

Clients see an IPP printer that driverless (IPP Everywhere) clients can use, and an eSCL scanner. These are the protocols that AirPrint and Mopria clients speak. printscan is not certified by Apple, the Mopria Alliance or the Printer Working Group, and it is not affiliated with them. AirPrint, Mopria and IPP Everywhere are trademarks of their owners (see `NOTICE`).

## Status

printscan has no release yet. It has been tested with the simulated backend and, in CI, against real daemons with no printer attached: Avahi, the PostScript PAPPL printer application, AirSane with the SANE `test` backend, and `ippeveprinter`. It has not yet been run with a physical printer, so expect rough edges on real hardware, and please report what you find.

The catalog targets USB printers and multifunction devices of these kinds: anything that speaks IPP over USB, HP devices supported by HPLIP, PostScript printers, Epson and Canon devices supported by Gutenprint, and PCL printers through Ghostscript. Scanners work through ipp-usb or through AirSane and the SANE backends Ubuntu ships.

## How it works

| Part | Runs as | What it does |
|---|---|---|
| Control plane | Deployment `printscan`, image `printscan` | Web UI and REST API on `:8080`, the agent endpoint on `:8081`. Reads `/data/printscan.yaml`, picks drivers from its catalog and renders the device pod through the Kubernetes API |
| Device pod | Deployment `printscan-device`, owned by the control plane | Host network, so mDNS and the IPP and eSCL ports sit on the node's LAN address. Holds dbus, Avahi, the agent, and then ipp-usb, or a PAPPL printer application plus AirSane |
| Agent | Sidecar in the device pod, same image as the control plane | Probes USB, sets up the PAPPL print queue over loopback IPP, browses DNS-SD, downloads and checks vendor archives so their licence can be shown, and reports to the control plane every 5 s. It opens no port |
| Device containers (docker backend) | `printscan-device-*` containers created by the control plane through the Engine API | Same daemons as the device pod, on the host network |
| Simulated backend | Inside the control plane, outside Kubernetes | In-process virtual printer and scanner, for trying printscan and for tests |

printscan never runs commands itself: it talks IPP, eSCL, D-Bus and the Kubernetes API or the Docker Engine API.

Driver choice is data in `internal/catalog/catalog.yaml`. The first matching rule wins:

1. A printer that speaks IPP over USB gets ipp-usb, which also handles its scanner. No driver is needed.
2. HP devices get the HPLIP printer application and AirSane with the `hpaio` SANE backend.
3. PostScript printers get the PostScript printer application, Epson and Canon get Gutenprint, PCL printers get the Ghostscript application, and anything else falls back to Ghostscript. Scanners go through AirSane and SANE.

You can override the printer driver and the scanner in the config.

## Requirements

- Kubernetes 1.33 or newer (the device pod uses native sidecars).
- A namespace with the `privileged` Pod Security level, because the device pod uses the host network. It mounts no host paths; `deploy/examples/admission-policy.yaml` enforces that.
- [generic-device-plugin](https://github.com/squat/generic-device-plugin) exposing the printer as an extended resource. Its `--domain` and device name must match `PRINTSCAN_USB_RESOURCE`: `--domain=devic.es` with a device named `printer` gives `devic.es/printer`. If they differ, the device pod stays Pending and looks like an unplugged printer. Use `count: 3`, because up to three containers of the device pod request the printer at once. See `deploy/examples/generic-device-plugin.yaml`.
- Two ReadWriteOnce volumes: one for the config and scans, one for vendor files. The config watcher relies on inotify, which network RWX file systems do not deliver. The device pod for a USB printer mounts the vendor-file volume, so it must be mountable on the node with the printer.
- An auth proxy in front of the web UI (see Security).

This list is for Kubernetes. For a single Docker or Podman host, see [Deploying with Docker or Podman](#deploying-with-docker-or-podman).

`docs/kubernetes.md` walks through each of these: USB passthrough, the device plugin, the namespace and admission policy, volumes, ingress, the NetworkPolicy, the image tag and the memory limit.

## Quick start (simulated printer)

```sh
docker compose up
```

Compose builds the image from source, which takes several minutes the first time. The container's health check turns healthy about 30 s after it starts.

Open http://127.0.0.1:8080. The setup wizard detects a simulated IPP-over-USB printer, writes the config, and brings up a virtual printer and scanner you can print to and scan from in the browser. Nothing touches real hardware.

Compose is a demo. It runs only the simulated backend. For a real printer, use Kubernetes with generic-device-plugin (see Requirements) or `compose.docker.yaml` (see [Deploying with Docker or Podman](#deploying-with-docker-or-podman)).

It publishes port 8080 on 127.0.0.1 only. If you change that to reach it from another machine, also set `PRINTSCAN_ALLOWED_HOSTS` to the host name or IP address you open it with, or to `*`. With the simulated backend printscan answers only `localhost`, `127.0.0.1` and `[::1]` by default, and any other host gets 421.

## Deploying on Kubernetes

`deploy/base` is a kustomize example: namespace `printing`, a service account with a namespaced Role, the control-plane Deployment, a ClusterIP Service, two PVCs and a NetworkPolicy. Adjust it, then:

```sh
kustomize build deploy/base | kubectl apply -f -
```

Values to adjust before applying:

- `deploy/base/networkpolicy.yaml`: the ingress controller's namespace (`ingress-controller`) and the node CIDR (`192.0.2.0/24`, a placeholder that matches nothing). Both are marked `# CHANGE ME`.
- `deploy/base/deployment.yaml`: `PRINTSCAN_USB_RESOURCE` to match your device plugin, and `PRINTSCAN_ALLOWED_HOSTS` with the host name of your ingress (commented out; without it Kubernetes deployments do no Host check).
- The image tag. Until the first release, `deploy/base/deployment.yaml` and `deploy/base/kustomization.yaml` both use `main`, and the container has `imagePullPolicy: Always` so it pulls each new `main`. After a release, set `newTag: <version>@sha256:<digest>` in `kustomization.yaml` and remove `imagePullPolicy: Always`.

Also:

- GHCR packages may be private after CI first pushes them. Make each `printscan*` package public in its package settings, or give the cluster a pull secret.
- Keep the container in `deploy/base/deployment.yaml` named `printscan` (see Images).
- Route only port 8080 through your ingress. Port 8081 takes agent reports from the device pod, which runs on the host network: allow it from the address the agent's traffic arrives with. Depending on the CNI that is the node's LAN address or its pod-network address; `deploy/base/networkpolicy.yaml` lists both.
- To lock down the namespace beyond the `privileged` Pod Security level, apply `deploy/examples/admission-policy.yaml` (see `docs/kubernetes.md`).

Open the UI. With no config file the wizard starts, shows what the agent found on USB, and writes the file.

## Deploying with Docker or Podman

`compose.docker.yaml` runs printscan on one Linux host with a USB printer, without Kubernetes. The control plane runs with the docker backend and creates the device containers itself through the Docker Engine API: `printscan-device-probe`, `-dbus`, `-avahi`, `-agent`, then `-ipp-usb`, or a printer application and `-airsane` (plus `-fetch-blobs` for a vendor file). These are the same containers as the Kubernetes device pod, on the host network, with read-only root file systems, all capabilities dropped and no privilege escalation. None of them is privileged. The containers that use the printer get `/dev/bus/usb` and a device-cgroup rule for USB devices, so a printer that is plugged in again stays reachable. Scratch space for print spools sits on disk (anonymous volumes), not in memory. Config files and the agent token go into named volumes that are recreated on every rollout.

Requirements:

- A Linux host with the printer on USB.
- Docker Engine 20.10 or newer, or rootful Podman 4 or newer with its API socket enabled.
- Docker Compose 2.24.4 or newer (the socket proxy override below needs it).
- No avahi-daemon on the host answering on port 5353. Two mDNS responders on one host make discovery unreliable.
- Free host ports: 8080 and 8081 (bound to 127.0.0.1 by default), and for the device 8001 to 8004, 8090, 8631 and 60000 to 60009, as on Kubernetes.

Start it from a checkout of this repository:

```sh
DOCKER_GID=$(stat -c %g /var/run/docker.sock) docker compose -f compose.docker.yaml up -d
```

`DOCKER_GID` is the group that owns the Engine socket. The control plane joins that group so it can use the socket. Then open http://127.0.0.1:8080. With no config file the wizard starts, shows what the agent found on USB, and writes the file. The stack is the Compose project `printscan-docker`, apart from the demo `compose.yaml`, so neither one's `up` or `down -v` touches the other's containers or data volume.

The UI listens on 127.0.0.1:8080 only. Put an authenticating proxy in front of it (see Security), or set `PRINTSCAN_LISTEN=:8080` on a trusted LAN. By default printscan answers only `localhost`, `127.0.0.1`, `[::1]` and the host's IP address, and any other host name gets 421. When you reach it by a host name, set `PRINTSCAN_ALLOWED_HOSTS` to that name. The host IP in that list is read once when the control plane starts; restart it after the address changes, or set `PRINTSCAN_HOST_IP`.

On Podman, enable the API socket and point Docker Compose at it:

```sh
sudo systemctl enable --now podman.socket
DOCKER_HOST=unix:///run/podman/podman.sock DOCKER_SOCKET=/run/podman/podman.sock DOCKER_GID=0 \
  docker compose -f compose.docker.yaml up -d
```

CI runs this path: the Docker Compose CLI against rootful Podman's socket. `podman compose` also works when its provider is docker-compose. podman-compose (the Python tool) is untested. It does not support `!reset` or `!override`, which the socket proxy override needs, and it may handle `userns_mode` and `group_add` differently.

Also:

- Image pinning: set `PRINTSCAN_IMAGE=ghcr.io/thephaseless/printscan:<version>@sha256:<digest>` and, for the device and driver images, `PRINTSCAN_IMAGE_OVERRIDES` (see Images). The probe, agent and fetch-blobs containers run the image of the control-plane container (named `printscan`), so they always match its build. Upgrading the control-plane image, or any change to the device images, their config or the agent URL, recreates the device containers once. A control-plane restart with the same image and config keeps them running.
- The status message "Printer not plugged in or not visible to the device plugin" means, on Docker, that the host has no USB printer. The control plane starts nothing until a printer appears, and checks again every 5 s.
- Connect URIs and the printer address use the source address of the host's default route, read again each time the control plane checks the device. On a host with several networks, or IPv6 only, set `PRINTSCAN_HOST_IP` to the address clients should use.
- Vendor files, such as HP's plug-in, go into the `printscan-blobs` volume, never into an image. The control plane creates the volume when it is missing and never removes it, and a volume of that name you create yourself is used as it is.
- Memory: the control plane sets its Go memory limit to 80% of its container memory limit (256Mi on Kubernetes), read from the cgroup; `GOMEMLIMIT` overrides it. `compose.docker.yaml` sets no memory limit for the control plane by default, so there is none; add `mem_limit` to get one. The device containers get the same limits as on Kubernetes.
- Logs: the device containers use the engine's default log driver. Configure log rotation in the daemon (`/etc/docker/daemon.json`).

Stopping and cleaning up: `docker compose down` stops only the control plane. The device containers keep serving the LAN, because the control plane created them, not Compose. To remove everything:

```sh
DOCKER_GID=0 docker compose -f compose.docker.yaml down -v
docker ps -aq --filter label=dev.printscan.managed-by=printscan | xargs -r docker rm -fv
docker volume ls -q --filter label=dev.printscan.managed-by=printscan | xargs -r docker volume rm
```

The last command also removes the `printscan-blobs` volume when printscan created it. `down -v` removes the config and stored scans with the data volume; leave out `-v` to keep them.

### Socket proxy

The Engine socket is root-equivalent: whoever can use it can start a privileged container. A socket proxy such as [docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) narrows what the control plane can reach to the calls it makes: `/_ping`; list, inspect, create, start, stop, wait for and remove containers, and copy files into them; inspect and pull images; inspect, create and remove volumes. Run the proxy with `CONTAINERS=1 IMAGES=1 VOLUMES=1 POST=1` (check its README for the current variable names), listening on `127.0.0.1:2375`. Then write `compose.override.yaml` next to `compose.docker.yaml`, which drops the socket mount and the extra group:

```yaml
services:
  printscan:
    group_add: !reset []
    volumes: !override
      - data:/data
```

Start the stack with both files, because Compose reads `compose.override.yaml` by itself only when no `-f` is given:

```sh
PRINTSCAN_DOCKER_HOST=tcp://127.0.0.1:2375 DOCKER_GID=0 \
  docker compose -f compose.docker.yaml -f compose.override.yaml up -d
```

`DOCKER_GID` must still be set, to any value, because `compose.docker.yaml` requires it. Creating containers through the proxy is still root-equivalent. Because the control plane uses the host network, the proxy has to listen on the host's loopback address, where every local user can reach it. On a host with other users, keep the socket mount instead.

### Limits

- Rootless Docker and rootless Podman are not supported: they cannot open USB device nodes or apply device-cgroup rules.
- A daemon with `userns-remap` works: the control plane and every device container set `userns_mode: host`.
- SELinux hosts need `setsebool -P container_use_devices on` for the USB devices, and `label=disable` (or a policy) for the socket mount.
- One printscan installation per host. The container names are fixed, and two installations would want the same host ports.

## Configuration

One file, by default `/data/printscan.yaml`. `examples/printscan.yaml` shows every key with comments, and `schema/config.schema.json` is its JSON Schema (editors with the YAML language server pick it up from the first line). Only `apiVersion` and `name` are required.

```yaml
apiVersion: printscan/v1
name: Living room printer
device:
  mode: usb            # or simulated
```

`name`: Required, 1 to 63 characters, no control characters, not starting with `-`. UI title, and the queue and DNS-SD name of PAPPL and simulated printers. ipp-usb and AirSane advertise the device's own names.

So with ipp-usb and AirSane, `name` changes the UI title only, and the name clients see on the network is the one the printer reports. The Status page always shows the advertised name as it was actually browsed.

Edit the file through Settings in the UI or by hand. The UI changes only the keys you touched and keeps your comments and blank lines. It sends `If-Match` with the file's ETag, so an edit made on disk in the meantime is never overwritten silently. Hand edits are picked up within seconds.

What happens when a value changes:

| Change | Effect |
|---|---|
| `print.defaults`, `location` | Reach the PAPPL queue on the agent's next cycle, within about 5 s. Nothing restarts |
| `print.maxUploadMiB` | Applies to the next upload |
| `scan.retentionDays` | Applies on the next hourly cleanup; `0` keeps scans forever |
| `name` with a PAPPL driver | The queue is deleted and recreated under the new name, which drops jobs still waiting in it |
| `device.mode`, `device.driver`, `device.scanner`, `device.usb`, accepted EULAs; `name` and `location` in simulated mode | The device pod rolls once if the change alters what runs in it |

The control plane itself never restarts for a config change. An invalid file keeps the last valid config running. The UI shows each error with its line and column and offers to restore the last working settings. While the file is invalid, the upload limit and scan retention follow the last valid config. While it is missing, they follow the defaults.

`printscan validate <file>...` checks files the same way and prints `file:line:col: message`.

### Server flags

Each flag has an environment variable:

| Flag | Environment | Default |
|---|---|---|
| `--listen` | `PRINTSCAN_LISTEN` | `:8080` |
| `--internal-listen` | `PRINTSCAN_INTERNAL_LISTEN` | `:8081`; ignored with the sim backend, which opens no agent port |
| `--config` | `PRINTSCAN_CONFIG` | `/data/printscan.yaml` |
| `--data-dir` | `PRINTSCAN_DATA_DIR` | `/data` (scans in `/data/scans`) |
| `--backend` | `PRINTSCAN_BACKEND` | `k8s` inside Kubernetes; `docker` inside a container that has `DOCKER_HOST` set or the Engine socket mounted; else `sim` |
| `--sim-device` | `PRINTSCAN_SIM_DEVICE` | `ippusb-mfp`; any fixture in `internal/catalog/fixtures`, or `none` |
| `--escl-client` | `PRINTSCAN_ESCL_CLIENT` | `mfp`; `xml` selects the fallback client |
| `--log-level` | `PRINTSCAN_LOG_LEVEL` | `info` |
| `--k8s-namespace` | `PRINTSCAN_K8S_NAMESPACE` | the service account's namespace |
| `--k8s-owner` | `PRINTSCAN_K8S_OWNER` | `printscan` |
| `--usb-resource` | `PRINTSCAN_USB_RESOURCE` | empty, meaning no USB (simulated mode only) |
| `--blob-pvc` | `PRINTSCAN_BLOB_PVC` | `printscan-blobs`; `--blob-pvc=` (empty) runs without a vendor-file volume |
| `--agent-url` | `PRINTSCAN_AGENT_URL` | `http://printscan.<namespace>.svc:8081`; docker: `http://<internal host or 127.0.0.1>:<internal port>` |
| `--docker-host` | `DOCKER_HOST` | the Engine default, `unix:///var/run/docker.sock` |
| `--docker-container` | `PRINTSCAN_DOCKER_CONTAINER` | `printscan` |
| `--host-ip` | `PRINTSCAN_HOST_IP` | docker: the default route's source address |
| `--image-registry` | `PRINTSCAN_IMAGE_REGISTRY` | `ghcr.io/thephaseless` |
| `--image-tag` | `PRINTSCAN_IMAGE_TAG` | derived from the version (see Images) |
| | `PRINTSCAN_IMAGE_OVERRIDES` | empty |
| `--allowed-hosts` | `PRINTSCAN_ALLOWED_HOSTS` | sim: `localhost,127.0.0.1,[::1]`; docker: those plus the host IP; k8s: no check. `*` turns the check off |
| `--proxy-secret-header` | `PRINTSCAN_PROXY_SECRET_HEADER` | `X-Printscan-Proxy-Secret` |
| `--proxy-secret-file` | `PRINTSCAN_PROXY_SECRET_FILE` | empty |
| | `PRINTSCAN_PROXY_SECRET` | empty; set this or `--proxy-secret-file`, not both |

An empty environment variable counts as unset, so it falls back to the default. To pass an empty value, such as `--blob-pvc=`, use the flag.

## Drivers and vendor files (EULAs)

Everything printscan needs for printing and scanning is free software and ships in the images. Some HP devices also need HP's proprietary plug-in, which printscan never ships. HP's licence text is inside the plug-in archive, so printscan shows it like HP's own installer does:

1. When the device pod starts, the agent downloads the plug-in archive from OpenPrinting's mirror into the vendor-file volume, before you accept anything. It checks HP's detached OpenPGP signature against the key committed in `internal/catalog/keys/` and the sha256 pinned in `internal/catalog/catalog.yaml`. Both must match.
2. It reads HP's licence text from inside the verified archive. The wizard and Settings show that exact text.
3. When you accept, the config records the licence id, its version and the sha256 of the text you saw:

   ```yaml
   licenses:
     acceptedEulas:
       - id: hplip-plugin
         version: "3.24.4"
         textSha256: <sha256 shown in Settings>
   ```

4. The `fetch-blobs` init container checks the archive again and installs the plug-in only if the licence text inside it still has that sha256. It unpacks it into the vendor-file volume, which the driver and scanner containers mount read-only.

If the text in the archive ever changes, printscan does not install or use the plug-in, and Settings asks you to read and accept the new text. HP's text is never stored in this repository.

Without internet access from the cluster, the download fails, the licence shows as failed, and the plug-in cannot be accepted. The same happens when printscan runs without a vendor-file volume (`--blob-pvc=`, see `docs/kubernetes.md`). Status and Settings show each file's state: installed, missing, failed or not available.

For the HP rule the plug-in is optional, and an optional file never blocks the device pod: if the download fails, the pod starts without it and Status reports the failure. A required file that keeps failing would hold the pod in its init phase; no catalog entry is required today.

## Printing from the web page

The web page sends a file as it is when the printer accepts its format. Otherwise printscan converts a few image formats:

- PNG becomes JPEG when the printer takes JPEG.
- PNG and JPEG become a one-page PDF when the printer takes PDF. A JPEG is embedded unchanged. A CMYK JPEG cannot be embedded and is refused with 415: save it as an RGB JPEG or a PDF.
- Anything else the printer does not take, such as a Word document or plain text, is refused with 415 (`unsupported-format`). Print it to PDF first.

Limits:

| Limit | Answer |
|---|---|
| An upload larger than `print.maxUploadMiB` (default 50 MiB) | 413 `too-large` |
| A PNG whose decoded pixels would take more than 128 MiB | 413 `image-too-large` |
| A damaged image | 400 `invalid-image` |
| A second PNG conversion while one is running | 429 `too-many-jobs` |
| More than 32 print jobs from the web page still being followed | 429 `too-many-jobs` |

## Scanning

A scanned page may be at most 256 MiB, and stored scans may take 1 GiB in total. When stored scans reach 1 GiB, new scans are refused with 507 (`storage-full`) until you delete old scans or they expire after `scan.retentionDays`. A running scan can be cancelled; it leaves no file.

## Security

- The web UI has no login of its own. Put it behind an authenticating reverse proxy (for example Authentik forward auth) and expose it only through that. When the proxy sets `X-Authentik-Username`, print jobs carry that name.
- `PRINTSCAN_ALLOWED_HOSTS` limits the host names printscan answers to (421 for others), which blocks DNS rebinding. `PRINTSCAN_PROXY_SECRET` makes the API require a header that only your proxy adds (401 without it), so requests that bypass the proxy fail. `docs/kubernetes.md` explains both.
- State-changing API calls are refused across origins (Go's `http.CrossOriginProtection`). Pages are served with a strict Content-Security-Policy, `nosniff`, `no-referrer` and same-origin opener and resource policies. API responses and scan downloads are sent with `Cache-Control: no-store`.
- The agent dials out to the control plane's internal port with a bearer token from a Secret the control plane creates. That connection is plain HTTP inside the cluster. The device pod opens no port of printscan's own.
- The control plane's Role names every object it may change, except device pods, which it can only list and delete.
- On the LAN the device pod serves IPP, eSCL and mDNS on the node's address, like a network printer would. ipp-usb also proxies the printer's own web console, and AirSane's web page on port 8090 can start scans. A web page open in a browser on the LAN can send requests to these ports too. PAPPL's admin pages answer only local clients. Treat the LAN as trusted, or restrict these ports on the node.
- With the docker backend, the control plane holds the Engine socket, which is root-equivalent on the host. It only touches containers and volumes labelled `dev.printscan.managed-by=printscan`. See `SECURITY.md`.
- Containers drop all capabilities, run with read-only root file systems and no privilege escalation. The containers that open the USB device (probe, ipp-usb, the printer application, AirSane) run as uid 0, because device nodes from the device plugin are owned by root. Everything else runs as non-root users.

Report vulnerabilities as described in `SECURITY.md`.

## Images

| Image | Contents |
|---|---|
| `ghcr.io/thephaseless/printscan` | The static Go binary with the UI, on distroless. Control plane and agent |
| `ghcr.io/thephaseless/printscan-device` | dbus, Avahi, ipp-usb, AirSane, the SANE backends, `ippeveprinter` |
| `ghcr.io/thephaseless/printscan-driver-ps`, `-hplip`, `-gutenprint`, `-ghostscript` | PAPPL with one retrofit printer application and its Ubuntu driver packages |

All images are built for linux/amd64 and linux/arm64, with SBOM and provenance attestations. Tags:

- `main` follows the main branch. Use it until the first release.
- `1.2.3`, `1.2` and `1` come from release tags, and `latest` points at the newest release.
- `sha-<commit>` names the images built from one commit. It is a tag, so pin by digest when you need an exact image.
- `<tag>-sources` holds the corresponding source of the device and driver images for every published branch and version tag: `main-sources`, `1.2.3-sources`, `1.2-sources`, `1-sources`. The source for `latest` is the newest `<version>-sources`. Each device and driver image names its source image in `/usr/share/printscan/SOURCES` and in the label `dev.printscan.sources`.

The control plane picks the device and driver image tags from its own version: a release runs the same release's images, and any other build (`dev`, `main`) runs `main`. `--image-tag` sets the tag directly. Images tagged `main` without a digest get `imagePullPolicy: Always`, so a restarted device pod always pulls the newest `main`. Everything else gets `IfNotPresent`.

The device pod's `probe`, `agent` and `fetch-blobs` containers run exactly the image of the container named `printscan` in the control plane's Deployment, digest included. Only when no container has that name do they fall back to `<registry>/printscan:<tag>`.

To pin digests, set `PRINTSCAN_IMAGE_OVERRIDES` to a comma-separated list of `key=image` pairs. Keys: `device`, `driver-ps`, `driver-hplip`, `driver-gutenprint`, `driver-ghostscript`, and `printscan` (used only when the Deployment has no `printscan` container). An unknown key stops the server at startup.

```sh
PRINTSCAN_IMAGE_OVERRIDES=device=ghcr.io/thephaseless/printscan-device:1.2.3@sha256:...,driver-hplip=ghcr.io/thephaseless/printscan-driver-hplip:1.2.3@sha256:...
```

## Connecting devices

Phones, tablets and computers on the same network find the printer by themselves. The Status page has these steps too, with the printer's actual name and addresses.

- Windows: Settings, Bluetooth & devices, Printers & scanners, Add device, then pick the printer. Windows 11 adds the scanner the same way. If it does not appear, choose Add manually, then "Add a printer using an IP address or hostname", set the device type to IPP Device and paste the printer address from Status.
  - Windows protected print mode: with it on, Windows supports only Mopria-certified printers. printscan is not certified, so Windows may refuse to add it. To use printscan from that PC, turn the mode off under Settings, Bluetooth & devices, Printers & scanners, Printer preferences, or print from the web page instead. On a managed PC, Group Policy may keep the mode on.
- macOS and iOS: on iPhone and iPad, choose Print from the share menu and pick the printer; there is nothing to install. On a Mac, add it in System Settings, Printers & Scanners. Scan from the same pane or from Image Capture.
- Android: open a document or photo, choose Print, Select a printer, All printers, then pick it. The built-in print service handles it. To scan, use the web page or an app that supports eSCL.
- Linux: most desktops list the printer by themselves. Otherwise run `lpadmin -p printscan -E -m everywhere -v <printer address>`. Scanning works through sane-airscan, for example in Document Scanner or Skanlite.

The web page shows each job's state live, from sent through printing to completed, for jobs sent from the web page. Jobs sent from other devices appear in the list, and it refreshes when the printer's state changes.

### Tailscale and other VPNs

mDNS does not cross a tailnet, so other devices on the VPN do not discover the printer. Add it with the manual IPP and eSCL addresses from the Status page, using the node's VPN address.

## Development

Install the toolchain with [mise](https://mise.jdx.dev) (`mise install`, pins in `mise.toml`), plus the C headers the Go build links against:

```sh
sudo apt-get install -y --no-install-recommends pkg-config libjpeg-turbo8-dev libpng-dev
```

```sh
mise x -- go test -race -count=1 ./...
mise x -- golangci-lint run
(cd ui && bun install --frozen-lockfile && bun run gen && bun run typecheck && bun run lint && bun run test && bun run build)
(cd ui && bunx playwright-core install chromium-headless-shell && bun run e2e)    # browser tests against the real binary
```

Run it locally with the simulated backend and a live UI build:

```sh
(cd ui && bun run dev) &
mise x -- go run ./cmd/printscan serve --backend=sim --data-dir .cache/data --config .cache/data/printscan.yaml --ui-dir ui/dist
```

Images: the device and driver Dockerfiles end with a `sources` stage, so always name the target.

```sh
docker buildx build --load -t printscan:dev .
docker buildx build --load -f images/device/Dockerfile --target device -t printscan-device:dev .
docker buildx build --load -f images/driver/Dockerfile --target ps -t printscan-driver-ps:dev .   # or hplip, gutenprint, ghostscript
```

`test/integration/README.md` covers the real-daemon integration test. `CONTRIBUTING.md` explains how to send a change, and `AGENTS.md` lists the project's conventions.

## Releasing

- A release is a `v*.*.*` tag on `main`, for example `v1.2.3`. CI then publishes every image as `1.2.3`, `1.2`, `1` and `latest`, plus `sha-<commit>`. Every push to `main` publishes `main` and `sha-<commit>`.
- For every pushed branch and tag, CI also publishes `-sources` images of the device and driver images (`main-sources`, `1.2.3-sources`, `1.2-sources`, `1-sources`). Their package lists come from the amd64 and arm64 images that the same run pushed. The source for `latest` is the newest `<version>-sources`.
- Runs triggered by a push are never cancelled, so a run cannot stop between pushing images and pushing their sources. Only pull request runs cancel older runs. If one image fails to build, the other images still get their `-sources` image, and the run fails (`ci-ok`).
- CI cannot hold `latest` back until the sources job has finished. A failed sources job fails `ci-ok`; fix it and re-run before announcing the release.
- `sha-<commit>` tags are tags, not digests. Pin digests for exact images.
- Before tagging, refresh `THIRD_PARTY_NOTICES.md` from the built images (its last section lists the commands).
- Bumping hplip in `images/driver/Dockerfile` also means bumping the `hplip-plugin` EULA version, the blob version, the blob URL and the blob's `sha256` in `internal/catalog/catalog.yaml`. Check the new plug-in against its `.asc` signature before pinning its sha256.
- Repository settings: a branch ruleset on `main` that requires the `ci-ok` check, and a tag ruleset that restricts who can create `v*` tags.

## Licence

printscan is licensed under the Apache License 2.0 (`LICENSE`, `NOTICE`). The images also contain third-party software under its own licences, listed in `THIRD_PARTY_NOTICES.md`, including how to get the corresponding source.
