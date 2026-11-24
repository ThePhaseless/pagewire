# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub: on the repository's Security tab, choose "Report a vulnerability". Please do not open a public issue for a security problem.

Include the version or image digest, what an attacker can do, and the steps to reproduce. The discussion stays in the private advisory. A fix goes into `main` and the next release, and the advisory is published once that release is out.

Vulnerabilities in the upstream daemons (ipp-usb, PAPPL and the printer applications, AirSane, Avahi, CUPS, SANE, HPLIP) belong to their projects. If printscan's configuration or packaging makes one of them worse, report it here too.

## Supported versions

Fixes go into the latest release and the `main` branch. Older releases do not get fixes; upgrade to the latest release.

## Threat model

printscan exposes a printer and a scanner on a home or small-office LAN. These are the boundaries it relies on:

- Web UI and API (port 8080): There is no login. The UI expects an authenticating reverse proxy in front of it and the ingress as its only route. Anyone who reaches port 8080 can print, scan, read scans and change the config. State-changing requests from other origins are refused, and pages are served with a strict Content-Security-Policy.
- Agent endpoint (port 8081): it accepts reports only with the bearer token from the `printscan-agent-token` Secret, compared in constant time. Keep it off the ingress; the example NetworkPolicy allows it only from the node addresses the agent's traffic comes from.
- Device pod on the LAN: It uses the host network, so the node's LAN address carries mDNS, IPP (ipp-usb, the printer application, `ippeveprinter`) and eSCL (ipp-usb, AirSane). Anyone on the LAN can print and scan, as with a network printer. ipp-usb also proxies the printer's own web console, and AirSane's web page on port 8090 can start scans. PAPPL's admin pages refuse remote clients. printscan's own code opens no listening port in the device pod: the agent dials out.
- Kubernetes permissions: The control plane's Role is limited to its namespace: the device Deployment and ConfigMap, the agent token Secret, and pods (get, list, watch, delete). It cannot touch other namespaces or cluster-wide objects. The namespace needs the `privileged` Pod Security level because the device pod uses the host network and mounts the host's `/sys`.
- Containers: Every container drops all capabilities and runs with a read-only root file system and no privilege escalation. Containers that open the USB device node run as uid 0, because the device plugin creates device nodes owned by root. All others run as non-root users.
- Never shelling out: printscan's Go code never starts processes. It talks IPP, eSCL, D-Bus and the Kubernetes API, and a lint rule enforces this.
- Vendor files: printscan downloads proprietary files (today only HP's plug-in) only after the matching licence is accepted in the config, only from the URL in the embedded catalog, with a size limit. Each download must verify against the vendor's OpenPGP signature and a public key committed to this repository. Archives are unpacked without following paths outside the target directory. They land on a separate volume that the driver containers mount read-only.
- Config file: parsing is strict. Unknown keys and invalid values are rejected with their position, and an invalid file never replaces the running config. Writes are atomic and guarded by an ETag.
- Supply chain: Images are built in CI with SBOM and provenance attestations. Every base image is pinned by digest, and every GitHub Action by commit SHA.

Known limits:

- An attacker on the LAN can use the printer and scanner, and reach the printer's web console through ipp-usb. Restrict these ports on the node if the LAN is not trusted.
- HP signs its plug-in with a 1024-bit DSA key over SHA-1. printscan checks that signature because it is the only one HP publishes.
