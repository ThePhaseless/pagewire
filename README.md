# printscan

printscan turns one USB multifunction printer into a network printer and scanner that phones, tablets and computers find by themselves, plus a web page for printing and scanning from any browser. It runs on Kubernetes and drives the OpenPrinting daemons (ipp-usb, PAPPL printer applications, AirSane, Avahi) for you. You describe the printer in one YAML file, or let the setup wizard write it.

Clients see a driverless IPP Everywhere printer and an eSCL scanner, the protocols that AirPrint and Mopria clients speak. printscan is not certified by Apple, the Mopria Alliance or the Printer Working Group, and it is not affiliated with them. AirPrint, Mopria and IPP Everywhere are trademarks of their owners (see `NOTICE`).

## How it works

| Part | Runs as | What it does |
|---|---|---|
| Control plane | Deployment `printscan`, image `printscan` | Web UI and REST API on `:8080`, the agent endpoint on `:8081`. Reads `/data/printscan.yaml`, picks drivers from its catalog and renders the device pod through the Kubernetes API |
| Device pod | Deployment `printscan-device`, owned by the control plane | Host network, so mDNS and the IPP and eSCL ports sit on the node's LAN address. Holds dbus, Avahi, the agent, and then ipp-usb, or a PAPPL printer application plus AirSane |
| Agent | Sidecar in the device pod, same image as the control plane | Probes USB, sets up the PAPPL print queue over loopback IPP, browses DNS-SD, fetches vendor files you accepted, and reports to the control plane every 5 s. It opens no port |
| Simulated backend | Inside the control plane, outside Kubernetes | In-process virtual printer and scanner, for trying printscan and for tests |

printscan never runs commands itself: it talks IPP, eSCL, D-Bus and the Kubernetes API.

Driver choice is data in `internal/catalog/catalog.yaml`. The first matching rule wins:

1. A printer that speaks IPP over USB gets ipp-usb, which also handles its scanner. No driver is needed.
2. HP devices get the HPLIP printer application and AirSane with the `hpaio` SANE backend.
3. PostScript printers get the PostScript printer application, Epson and Canon get Gutenprint, PCL printers get the Ghostscript application, and anything else falls back to Ghostscript. Scanners go through AirSane and SANE.

You can override the printer driver and the scanner in the config.

## Requirements

- Kubernetes 1.33 or newer (the device pod uses native sidecars).
- A namespace with the `privileged` Pod Security level. The device pod needs the host network and the host's `/sys`.
- [generic-device-plugin](https://github.com/squat/generic-device-plugin) exposing the printer as an extended resource. Its `--domain` and device name must match `PRINTSCAN_USB_RESOURCE`: `--domain=devic.es` with a device named `printer` gives `devic.es/printer`. If they differ, the device pod stays Pending and looks like an unplugged printer. Use `count: 3`, because up to three containers of the device pod request the printer at once. See `deploy/examples/generic-device-plugin.yaml`.
- Two ReadWriteOnce volumes: one for the config and scans, one for vendor files. The config watcher relies on inotify, which network RWX file systems do not deliver.
- An auth proxy in front of the web UI (see Security).

## Quick start (simulated printer)

```sh
docker compose up
```

Open http://127.0.0.1:8080. The setup wizard detects a simulated IPP-over-USB printer, writes the config, and brings up a virtual printer and scanner you can print to and scan from in the browser. Nothing touches real hardware.

## Deploying on Kubernetes

`deploy/base` is a kustomize example: namespace `printing`, a service account with a namespaced Role, the control-plane Deployment, a ClusterIP Service, two PVCs and a NetworkPolicy. Adjust it, then:

```sh
kustomize build deploy/base | kubectl apply -f -
```

- Pin the image as `tag@sha256:<digest>` in `kustomization.yaml`.
- Keep the container in `deploy/base/deployment.yaml` named `printscan` (see Images).
- Route only port 8080 through your ingress. Port 8081 takes agent reports from the device pod, which runs on the host network: allow it from the address the agent's traffic arrives with. Depending on the CNI that is the node's LAN address or its pod-network address; `deploy/base/networkpolicy.yaml` lists both.

Open the UI. With no config file the wizard starts, shows what the agent found on USB, and writes the file.

## Configuration

One file, by default `/data/printscan.yaml`. `examples/printscan.yaml` shows every key with comments, and `schema/config.schema.json` is its JSON Schema (editors with the YAML language server pick it up from the first line). Only `apiVersion` and `name` are required.

```yaml
apiVersion: printscan/v1
name: Living room printer
device:
  mode: usb            # or simulated
```

`name`: Required, 1 to 63 characters, no control characters. UI title, and the queue and DNS-SD name of PAPPL and simulated printers. ipp-usb and AirSane advertise the device's own names.

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
| `--internal-listen` | `PRINTSCAN_INTERNAL_LISTEN` | `:8081` |
| `--config` | `PRINTSCAN_CONFIG` | `/data/printscan.yaml` |
| `--data-dir` | `PRINTSCAN_DATA_DIR` | `/data` (scans in `/data/scans`) |
| `--backend` | `PRINTSCAN_BACKEND` | `k8s` inside Kubernetes, else `sim` |
| `--sim-device` | `PRINTSCAN_SIM_DEVICE` | `ippusb-mfp`; any fixture in `internal/catalog/fixtures`, or `none` |
| `--escl-client` | `PRINTSCAN_ESCL_CLIENT` | `mfp`; `xml` selects the fallback client |
| `--log-level` | `PRINTSCAN_LOG_LEVEL` | `info` |
| `--k8s-namespace` | `PRINTSCAN_K8S_NAMESPACE` | the service account's namespace |
| `--k8s-owner` | `PRINTSCAN_K8S_OWNER` | `printscan` |
| `--usb-resource` | `PRINTSCAN_USB_RESOURCE` | empty, meaning no USB (simulated mode only) |
| `--blob-pvc` | `PRINTSCAN_BLOB_PVC` | `printscan-blobs` |
| `--agent-url` | `PRINTSCAN_AGENT_URL` | `http://printscan.<namespace>.svc:8081` |
| `--image-registry` | `PRINTSCAN_IMAGE_REGISTRY` | `ghcr.io/thephaseless` |
| `--image-tag` | `PRINTSCAN_IMAGE_TAG` | derived from the version (see Images) |
| | `PRINTSCAN_IMAGE_OVERRIDES` | empty |

## Drivers and vendor files (EULAs)

Everything printscan needs for printing and scanning is free software and ships in the images. Some HP devices also need HP's proprietary plug-in, which printscan never ships. It downloads the plug-in on your cluster only after you accept HP's licence, either in the wizard or Settings, or by listing it under `licenses.acceptedEulas`:

```yaml
licenses:
  acceptedEulas:
    - id: hplip-plugin
      version: "3.24.4"
```

The `fetch-blobs` init container then downloads the plug-in from OpenPrinting's mirror, checks HP's detached OpenPGP signature against the key committed in `internal/catalog/keys/`, and unpacks it into the vendor-file volume. The driver and scanner containers mount that volume read-only. Status and Settings show each file's state: installed, missing, failed or not available.

For the HP rule the plug-in is optional, and an optional file never blocks the device pod: if the download fails, the pod starts without it and Status reports the failure. A required file that keeps failing would hold the pod in its init phase; no catalog entry is required today.

## Security

- The web UI has no login of its own. Put it behind an authenticating reverse proxy (for example Authentik forward auth) and expose it only through that. When the proxy sets `X-Authentik-Username`, print jobs carry that name.
- State-changing API calls are refused across origins (Go's `http.CrossOriginProtection`). Pages are served with a strict Content-Security-Policy, `nosniff` and `no-referrer`.
- The agent dials out to the control plane's internal port with a bearer token from a Secret the control plane creates. The device pod opens no port of printscan's own.
- On the LAN the device pod serves IPP, eSCL and mDNS on the node's address, like a network printer would. ipp-usb also proxies the printer's own web console, and AirSane's web page on port 8090 can start scans. PAPPL's admin pages answer only local clients. Treat the LAN as trusted, or restrict these ports on the node.
- Containers drop all capabilities, run with read-only root file systems and no privilege escalation. The containers that open the USB device (probe, ipp-usb, the printer application, AirSane) run as uid 0, because device nodes from the device plugin are owned by root. Everything else runs as non-root users.

Report vulnerabilities as described in `SECURITY.md`.

## Images

| Image | Contents |
|---|---|
| `ghcr.io/thephaseless/printscan` | The static Go binary with the UI, on distroless. Control plane and agent |
| `ghcr.io/thephaseless/printscan-device` | dbus, Avahi, ipp-usb, AirSane, the SANE backends, `ippeveprinter` |
| `ghcr.io/thephaseless/printscan-driver-ps`, `-hplip`, `-gutenprint`, `-ghostscript` | PAPPL with one retrofit printer application and its Ubuntu driver packages |

All images are built for linux/amd64 and linux/arm64, with SBOM and provenance attestations. Tags:

- `main` follows the main branch.
- `1.2.3`, `1.2` and `1` come from release tags, and `latest` points at the newest release.
- `<version>-sources` holds the corresponding source of the device and driver images for each release.

The control plane picks the device and driver image tags from its own version: a release runs the same release's images, and any other build (`dev`, `main`) runs `main`. `--image-tag` sets the tag directly. Images tagged `main` without a digest get `imagePullPolicy: Always`, so a restarted device pod always pulls the newest `main`. Everything else gets `IfNotPresent`.

The device pod's `probe`, `agent` and `fetch-blobs` containers run exactly the image of the container named `printscan` in the control plane's Deployment, digest included. Only when no container has that name do they fall back to `<registry>/printscan:<tag>`.

To pin digests, set `PRINTSCAN_IMAGE_OVERRIDES` to a comma-separated list of `key=image` pairs. Keys: `device`, `driver-ps`, `driver-hplip`, `driver-gutenprint`, `driver-ghostscript`, and `printscan` (used only when the Deployment has no `printscan` container). An unknown key stops the server at startup.

```sh
PRINTSCAN_IMAGE_OVERRIDES=device=ghcr.io/thephaseless/printscan-device:1.2.3@sha256:...,driver-hplip=ghcr.io/thephaseless/printscan-driver-hplip:1.2.3@sha256:...
```

## Connecting devices

Phones, tablets and computers on the same network find the printer by themselves. The Status page has these steps too, with the printer's actual name and addresses.

- Windows: Settings, Bluetooth & devices, Printers & scanners, Add device, then pick the printer. Windows 11 adds the scanner the same way. If it does not appear, choose Add manually, then "Add a printer using an IP address or hostname", set the device type to IPP Device and paste the printer address from Status.
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

`test/integration/README.md` covers the real-daemon integration test. `AGENTS.md` lists the project's conventions.

## Licence

printscan is licensed under the Apache License 2.0 (`LICENSE`, `NOTICE`). The images also contain third-party software under its own licences, listed in `THIRD_PARTY_NOTICES.md`, including how to get the corresponding source.
