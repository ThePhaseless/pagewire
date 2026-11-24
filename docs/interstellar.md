# Deploying to Interstellar

This repository ships only the example manifests in `deploy/`. Wiring printscan into the Interstellar cluster is a separate change in the Interstellar repository. This is its checklist.

## USB passthrough to the node

- [ ] Terraform: a `proxmox_hardware_mapping_usb` resource for the printer (VID:PID and, if needed, the port), and a `usb { mapping = ..., usb3 = true }` block on the VM of talos-2 or talos-3.
- [ ] Grant the Terraform API token `Mapping.Use` on the mapping.
- [ ] After apply, check that the node sees the device, for example with `talosctl -n <node> read /sys/bus/usb/devices/<bus-port>/idVendor`.

## Device plugin

- [ ] Deploy squat/generic-device-plugin as a DaemonSet, based on `deploy/examples/generic-device-plugin.yaml`, with the image pinned as `tag@sha256`.
- [ ] Pass `--domain=devic.es` and a device named `printer` with `"count": 3` and the printer's vendor and product IDs, so the resource is `devic.es/printer`.
- [ ] Set `PRINTSCAN_USB_RESOURCE=devic.es/printer` on the control plane. A mismatch leaves the device pod Pending, and printscan cannot tell that apart from an unplugged printer.
- [ ] Optional: a Talos `UdevRulesConfig` for the printer's device node. Without it the node is `root:root 0664`, which works because the USB containers run as uid 0.

## The app

- [ ] Namespace `printing` with the label `pod-security.kubernetes.io/enforce: privileged`.
- [ ] `Kubernetes/apps/printscan/` built from `deploy/base`. Pin the image as `tag@digest` in the kustomization.
- [ ] Keep the control-plane container named `printscan`. The device pod's probe, agent and fetch-blobs containers reuse that container's image.
- [ ] Set `PRINTSCAN_IMAGE_OVERRIDES` with digest-pinned refs for `device` and the driver images in use (`driver-ps`, `driver-hplip`, `driver-gutenprint`, `driver-ghostscript`).
- [ ] Keep the Reloader opt-out annotation (`reloader.stakater.com/auto: "false"`). printscan watches its config file itself, and a restart only interrupts jobs.
- [ ] PVCs `printscan-data` and `printscan-blobs` on Longhorn, ReadWriteOnce.

## Ingress and network

- [ ] IngressRoute for `druk.nerine.dev` to the Service's `http` port (8080), with the `public-chain` and `authentik` middlewares. printscan has no login of its own.
- [ ] Never route port 8081 through the ingress.
- [ ] NetworkPolicy: allow 8080 from Traefik's namespace, and 8081 from the address the agent's traffic actually arrives with. The agent runs on the host network, so depending on the CNI that is the node's LAN address or its pod-network address. Check which one with a temporary log or a flow capture, then drop the other range from the example.
- [ ] Add the dashboard tile in `local.private_service_apps`.

## Docs

- [ ] Add a row for printscan to the services table in the Interstellar README.
- [ ] Add a gotcha to the Interstellar AGENTS.md: the printer's VID:PID lives in two places, the Terraform USB mapping and the device plugin's `--device` argument. Replacing the printer means changing both.

## After deploying

- [ ] Start in simulated mode (`device.mode: simulated`) and check discovery and printing from the LAN before plugging in the real printer.
- [ ] Then switch to `device.mode: usb`, and check that the wizard or Status shows the printer and the matched driver.
- [ ] From the LAN, add the printer and the scanner and print a test page on each of Windows, macOS or iOS, Android and Linux.
- [ ] Print and scan through `druk.nerine.dev` behind Authentik.
