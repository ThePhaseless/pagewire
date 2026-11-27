# Third-party notices

pagewire's own code is licensed under Apache-2.0 (`LICENSE`, `NOTICE`). The container images also contain third-party software, listed here per image: what it is, its licence, how it gets into the image, where its source is, and whether pagewire changes it.

Versions were read from images built from this repository on 2026-11-24 (linux/arm64); those of `pagewire-driver-legacy` and of the driver packages added to or moved between driver images since then, on 2026-11-25. Ubuntu and Debian package versions move with security updates, because every image build runs `apt-get upgrade`. For a given image the authoritative lists are inside it and attached to it:

- the SBOM attestation published with every image (Go modules, npm packages, Alpine, Debian and Ubuntu packages);
- `/licenses` in the control-plane image (`go/` for Go modules and the Go standard library, `c/` for the statically linked C libraries, `pagewire/` for this project's `LICENSE`, `NOTICE` and this file);
- `/usr/share/pagewire/source-packages.txt` in the device and driver images (every Ubuntu source package, as `name=version`), with each package's licence in `/usr/share/doc/<package>/copyright` and the licences of software built from source in `/usr/share/licenses/`;
- `/usr/share/pagewire/` in the device and driver images also holds `NOTICE`, this file and `SOURCES`, which names the image with the corresponding source (`ghcr.io/thephaseless/<image>:<tag>-sources`). The image label `io.github.thephaseless.pagewire.sources` carries the same name.

Licence cells give the licence of the component's own files. Debian packaging files (`debian/*`) are often GPL-2.0-or-later and are not repeated here; the copyright file in the image has every stanza.

"Linked" below means compiled into the pagewire binary. "Separate program" means a program installed unmodified next to others in the image and run as its own process (aggregation). pagewire never links against GPL, LGPL or AGPL code.

## Control-plane image (`pagewire`)

A single statically linked Go binary with the web UI embedded, on Google's distroless base.

The binary also embeds two indexes that the build generates from the device and driver images: `internal/catalog/drivers.tsv.gz` holds the name, description and IEEE-1284 device ID of every driver the printer applications list, as they print them, and `internal/catalog/sane-usb.tsv` maps USB vendor and product IDs to SANE backend names, read from the device image's `/etc/sane.d` and from sane-backends' `.desc` files. Both are lists of model names and IDs; the software they describe is listed in the sections below.

### Go modules (linked)

Licences as classified by `go tool go-licenses`, which skips the standard library. The full texts ship in the image under `/licenses/go/`, the standard library's as `/licenses/go/LICENSE-go-stdlib`. Each module's source is at `https://pkg.go.dev/<module>@<version>`, and the standard library's at https://go.dev/dl/. None is modified.

| Module | Version | Licence |
|---|---|---|
| Go standard library (not listed by go-licenses) | 1.27.1 | BSD-3-Clause |
| `github.com/bahlo/generic-list-go` | v0.2.0 | BSD-3-Clause |
| `github.com/buger/jsonparser` | v1.1.2 | MIT |
| `github.com/cloudflare/circl` | v1.6.3 | BSD-3-Clause |
| `github.com/containerd/errdefs` | v1.0.0 | Apache-2.0 |
| `github.com/containerd/errdefs/pkg` | v0.3.0 | Apache-2.0 |
| `github.com/davecgh/go-spew` | v1.1.2-0.20180830191138-d8f796af33cc | ISC |
| `github.com/distribution/reference` | v0.6.0 | Apache-2.0 |
| `github.com/docker/go-connections` | v0.8.2 | Apache-2.0 |
| `github.com/docker/go-units` | v0.5.0 | Apache-2.0 |
| `github.com/emicklei/go-restful/v3` | v3.13.0 | MIT |
| `github.com/felixge/httpsnoop` | v1.0.4 | MIT |
| `github.com/fsnotify/fsnotify` | v1.10.1 | BSD-3-Clause |
| `github.com/fxamacker/cbor/v2` | v2.9.1 | MIT |
| `github.com/gabriel-vasile/mimetype` | v1.4.16 | MIT |
| `github.com/go-logr/logr` | v1.4.3 | Apache-2.0 |
| `github.com/go-logr/stdr` | v1.2.2 | Apache-2.0 |
| `github.com/go-openapi/jsonpointer` | v1.0.0 | Apache-2.0 |
| `github.com/go-openapi/jsonreference` | v1.0.0 | Apache-2.0 |
| `github.com/go-openapi/swag` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/cmdutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/conv` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/fileutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/jsonutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/loading` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/mangling` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/netutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/pools` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/stringutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/typeutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-openapi/swag/yamlutils` | v0.27.1 | Apache-2.0 |
| `github.com/go-playground/locales` | v0.14.2 | MIT |
| `github.com/go-playground/universal-translator` | v0.18.2 | MIT |
| `github.com/go-playground/validator/v10` | v10.30.6 | MIT |
| `github.com/goccy/go-yaml` | v1.19.2 | MIT |
| `github.com/godbus/dbus/v5` | v5.2.4 | BSD-2-Clause |
| `github.com/google/gnostic-models` | v0.7.0 | Apache-2.0 |
| `github.com/google/renameio/v2` | v2.0.2 | Apache-2.0 |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause |
| `github.com/invopop/jsonschema` | v0.14.0 | MIT |
| `github.com/json-iterator/go` | v1.1.12 | MIT |
| `github.com/leodido/go-urn` | v1.5.0 | MIT |
| `github.com/moby/docker-image-spec` | v1.3.1 | Apache-2.0 |
| `github.com/moby/moby/api` | v1.57.0 | Apache-2.0 |
| `github.com/moby/moby/client` | v0.6.3 | Apache-2.0 |
| `github.com/modern-go/concurrent` | v0.0.0-20180306012644-bacd9c7ef1dd | Apache-2.0 |
| `github.com/modern-go/reflect2` | v1.0.3-0.20250322232337-35a7c28c31ee | Apache-2.0 |
| `github.com/munnerz/goautoneg` | v0.0.0-20191010083416-a7dc8b61c822 | BSD-3-Clause |
| `github.com/opencontainers/go-digest` | v1.0.0 | Apache-2.0 |
| `github.com/opencontainers/image-spec` | v1.1.1 | Apache-2.0 |
| `github.com/OpenPrinting/go-mfp` | v0.0.0-20261123173030-b3cc636803df | BSD-2-Clause |
| `github.com/OpenPrinting/goipp` | v1.2.1-0.20260630192304-61012e2ae9bf | BSD-2-Clause |
| `github.com/pb33f/ordered-map/v2` | v2.3.1 | Apache-2.0 |
| `github.com/ProtonMail/go-crypto` | v1.5.2 | BSD-3-Clause |
| `github.com/x448/float16` | v0.8.4 | MIT |
| `go.opentelemetry.io/auto/sdk` | v1.1.0 | Apache-2.0 |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.60.0 | Apache-2.0 |
| `go.opentelemetry.io/otel` | v1.35.0 | Apache-2.0 |
| `go.opentelemetry.io/otel/metric` | v1.35.0 | Apache-2.0 |
| `go.opentelemetry.io/otel/trace` | v1.35.0 | Apache-2.0 |
| `go.yaml.in/yaml/v2` | v2.4.4 | Apache-2.0 |
| `go.yaml.in/yaml/v3` | v3.0.4 | MIT |
| `go.yaml.in/yaml/v4` | v4.0.0-rc.2 | MIT |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause |
| `golang.org/x/image` | v0.24.0 | BSD-3-Clause |
| `golang.org/x/net` | v0.60.0 | BSD-3-Clause |
| `golang.org/x/oauth2` | v0.36.0 | BSD-3-Clause |
| `golang.org/x/sys` | v0.50.0 | BSD-3-Clause |
| `golang.org/x/term` | v0.47.0 | BSD-3-Clause |
| `golang.org/x/text` | v0.43.0 | BSD-3-Clause |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause |
| `google.golang.org/protobuf` | v1.36.12-0.20260120151049-f2248ac996af | BSD-3-Clause |
| `gopkg.in/evanphx/json-patch.v4` | v4.13.0 | BSD-3-Clause |
| `gopkg.in/inf.v0` | v0.9.1 | BSD-3-Clause |
| `k8s.io/api` | v0.37.2 | Apache-2.0 |
| `k8s.io/apimachinery` | v0.37.2 | Apache-2.0 AND BSD-3-Clause |
| `k8s.io/client-go` | v0.37.2 | Apache-2.0 |
| `k8s.io/klog/v2` | v2.140.0 | Apache-2.0 |
| `k8s.io/kube-openapi` | v0.0.0-20260721132016-d427ff9ee9ad | Apache-2.0 AND BSD-3-Clause |
| `k8s.io/utils` | v0.0.0-20260626114624-be93311217bd | Apache-2.0 AND BSD-3-Clause |
| `sigs.k8s.io/json` | v0.0.0-20250730193827-2d320260d730 | Apache-2.0 AND BSD-3-Clause |
| `sigs.k8s.io/randfill` | v1.0.0 | Apache-2.0 |
| `sigs.k8s.io/structured-merge-diff/v6` | v6.4.2 | Apache-2.0 |
| `sigs.k8s.io/yaml` | v1.6.0 | Apache-2.0 AND BSD-3-Clause AND MIT |

### C libraries (linked statically)

go-mfp's image conversion code uses libjpeg and libpng through cgo. The binary is linked statically against musl. The packages come from Alpine (the `golang:1.27.1-alpine` build stage), and their licence texts ship under `/licenses/c/`.

| Component | Version | Licence | Source |
|---|---|---|---|
| musl | 1.2.6 (Alpine 1.2.6-r3) | MIT | https://musl.libc.org/releases/ |
| libjpeg-turbo | 3.1.3 (Alpine 3.1.3-r0) | IJG AND BSD-3-Clause AND Zlib | https://github.com/libjpeg-turbo/libjpeg-turbo |
| libpng | 1.6.59 (Alpine 1.6.59-r0) | Libpng-2.0 | https://github.com/pnggroup/libpng |
| zlib | Alpine 1.4-r0 | Zlib | https://zlib.net |

Alpine's package sources, including its patches, are in https://gitlab.alpinelinux.org/alpine/aports. None is modified by pagewire.

### Web UI (npm packages, bundled)

The production dependencies bundled into the UI. Their full licence texts ship inside the binary as `THIRD_PARTY_LICENSES.txt`, served next to the UI at `/THIRD_PARTY_LICENSES.txt`. Sources are at `https://www.npmjs.com/package/<name>/v/<version>`. None is modified.

| Package | Version | Licence |
|---|---|---|
| `@fontsource-variable/figtree` | 5.3.0 | OFL-1.1 |
| `@internationalized/date` | 3.12.4 | Apache-2.0 |
| `@internationalized/number` | 3.6.9 | Apache-2.0 |
| `@internationalized/string` | 3.2.10 | Apache-2.0 |
| `@react-types/shared` | 3.37.0 | Apache-2.0 |
| `@swc/helpers` | 0.5.24 | Apache-2.0 |
| `aria-hidden` | 1.2.6 | MIT |
| `client-only` | 0.0.1 | MIT |
| `clsx` | 2.1.1 | MIT |
| `react-aria-components` | 1.22.1 | Apache-2.0 |
| `react-aria` | 3.53.1 | Apache-2.0 |
| `react-dom` | 19.3.1 | MIT |
| `react-stately` | 3.51.0 | Apache-2.0 |
| `react` | 19.3.1 | MIT |
| `scheduler` | 0.28.0 | MIT |
| `tslib` | 2.9.1 | 0BSD |
| `use-sync-external-store` | 1.7.0 | MIT |
| `zod` | 4.6.7 | MIT |

The Figtree font files are under the SIL Open Font License 1.1 and are served unmodified.

### Distroless base (Debian packages)

`gcr.io/distroless/static-debian13:nonroot`, pinned by digest in `Dockerfile`. The list below was read from that pinned image (its `/var/lib/dpkg/status.d/` and each package's `/usr/share/doc/<package>/copyright`) for linux/arm64, and it matches the SBOM of the locally built image. Every package except `base-files` is architecture-independent (`Architecture: all`), so the amd64 variant carries the same files; its `base-files` build may differ.

| Package | Version | Licence | How included |
|---|---|---|---|
| base-files | 13.8+deb13u8 | GPL-2.0-or-later | Base image: `/etc/os-release` and similar files |
| ca-certificates | 20250419+deb13u1 | MPL-2.0 (certificate data) AND GPL-2.0-or-later (scripts) | Base image: the CA bundle |
| media-types | 13.0.0 | public information, no licence claimed (`ad-hoc` in its copyright file) | Base image: `/etc/mime.types` |
| netbase | 6.5 | GPL-2.0-only | Base image: `/etc/protocols`, `/etc/services` |
| tzdata | 2026f-0+deb13u1 | public domain | Base image: time zone data |
| tzdata-legacy | 2026f-0+deb13u1 | public domain | Base image: legacy time zone names |

The packages are unmodified Debian packages. Their sources are at `https://sources.debian.org/src/<package>/<version>/`, and every published version stays available at https://snapshot.debian.org/. pagewire builds no `-sources` image for the control plane.

## Device image (`pagewire-device`)

dbus, Avahi, ipp-usb, AirSane, the SANE backends and `ippeveprinter`, on `ubuntu:26.04`. Every component is a separate program, or a library linked by one. None is modified, except the SANE xerox_mfp backend (see "Rebuilt from Ubuntu's source" below).

Built from upstream source:

| Component | Version | Licence | How included | Source |
|---|---|---|---|---|
| ipp-usb | 0.9.34 | BSD-2-Clause | Separate program `/usr/local/sbin/ipp-usb`, with its quirks files in `/usr/share/ipp-usb/quirks/` | https://github.com/OpenPrinting/ipp-usb/tree/0.9.34 |
| goipp (inside ipp-usb) | v1.2.0 | BSD-2-Clause | Linked into ipp-usb, vendored in its source tree; licence in `/usr/share/licenses/ipp-usb/LICENSE.goipp` | https://github.com/OpenPrinting/goipp |
| Go standard library (inside ipp-usb) | 1.27.1 | BSD-3-Clause | Linked into ipp-usb; licence in `/usr/share/licenses/ipp-usb/LICENSE.go` | https://go.dev/dl/ |
| AirSane | v0.4.16 | GPL-3.0-or-later | Separate program `/usr/local/bin/airsaned` | https://github.com/SimulPiscator/AirSane/tree/v0.4.16 |

The main Ubuntu packages (`apt-get install --no-install-recommends`; the full list, with dependencies and the base image, is `/usr/share/pagewire/source-packages.txt`):

| Package (source) | Version | Licence | How included |
|---|---|---|---|
| dbus (dbus) | 1.16.2-2ubuntu4 | AFL-2.1 OR GPL-2.0-or-later | Separate program: the pod's system bus |
| avahi-daemon, libavahi-client3 (avahi) | 0.8-18ubuntu1.1 | LGPL-2.1-or-later | Separate program: the pod's mDNS responder; client library for ipp-usb and AirSane |
| libsane1 (sane-backends) | 1.4.0-1ubuntu1 | GPL-2.0-or-later WITH SANE-exception | Library loaded by AirSane: the SANE backends and `test` (its xerox_mfp backend is replaced by the rebuilt one below) |
| libsane-hpaio (hplip) | 3.24.4+dfsg0-0ubuntu8.4 | GPL-2.0-or-later | SANE backend for HP scanners, loaded by AirSane |
| cups-ipp-utils (cups) | 2.4.16-1ubuntu1.3 | Apache-2.0 (with CUPS's GPL2/LGPL2 exception) | Separate program `ippeveprinter`, for simulated mode |
| libusb-1.0-0 (libusb-1.0) | 2:1.0.29-2build1 | LGPL-2.1-or-later | Library used by ipp-usb and the SANE backends |
| libjpeg-turbo8 (libjpeg-turbo) | 2.1.5-4ubuntu4 | IJG AND BSD-3-Clause AND Zlib | Library |
| libpng16-16t64 (libpng1.6) | 1.6.57-1ubuntu0.1 | Libpng-2.0 | Library |
| ca-certificates | 20260816ubuntu0.26.04.2 | MPL-2.0 AND GPL-2.0-or-later | CA bundle |

Rebuilt from Ubuntu's source: the SANE xerox_mfp backend, `/usr/lib/<arch>-linux-gnu/sane/libsane-xerox_mfp.so.1`. It is compiled from the sane-backends source package of the installed libsane1 (1.4.0-1ubuntu1) with `images/device/patches/sane-backends-xerox_mfp-adf.patch`, and replaces libsane1's copy. With the patch, the backend offers the ADF and Auto scan sources only on devices that report a document feeder; unpatched, it offers them on every device. The backend and the patch are GPL-2.0-or-later WITH SANE-exception. sane-backends' LICENSE and COPYING and the patch ship in `/usr/share/licenses/sane-backends/`; the source package and the patch are in the `-sources` image.

The image's 139 source packages at the time of writing include the `ubuntu:26.04` base (glibc, bash, coreutils and so on).

The HP plug-in paths (`/usr/share/hplip/{prnt,scan,fax}/plugins`, `/usr/share/hplip/data/firmware`, `/var/lib/hp/hplip.state`) are symlinks into the vendor-file volume. The image contains no plug-in files.

## Driver images (`pagewire-driver-ps`, `-hplip`, `-gutenprint`, `-ghostscript`, `-legacy`)

Each image holds one PAPPL retrofit printer application on `ubuntu:26.04`, built the way the upstream rock recipes build it (`-legacy`: pappl-retrofit's own `legacy-printer-app`, for classic CUPS drivers, as pappl-retrofit's `make install` builds it), plus CUPS (for its `usb`, `socket` and `ipp` backends; `cupsd` is never started) and that family's Ubuntu driver packages.

Built from upstream source (identical in all five images, except the application):

| Component | Version | Licence | How included | Modifications | Source |
|---|---|---|---|---|---|
| PAPPL | v1.4.12 | Apache-2.0 (with its optional GPL2/LGPL2 exception) | Library linked into the application | Yes: `images/driver/patches/pappl-max-vendor.patch`, the changes the upstream rock recipes make. It raises `PAPPL_MAX_VENDOR` from 32 to 256, turns off log rotation, and makes the web interface's manual network printer use a `cups:socket://` URI. Also `images/driver/patches/pappl-web-admin-closed.patch`, pagewire's own change: the web interface's admin pages and printer actions answer 403 when no authentication service is set. And `images/driver/patches/pappl-ipp-admin-local.patch`, pagewire's own change: Disable-Printer, Enable-Printer and Cancel-Current-Job answer only local clients (loopback or the domain socket) when no authentication service is set | https://github.com/michaelrsweet/pappl/tree/v1.4.12 |
| pappl-retrofit | 1.0b3 | Apache-2.0 (with the CUPS exceptions) | Library linked into the application | Yes: `images/driver/patches/pappl-retrofit-fd-leak.patch`, upstream commit 9e8cfd03c76f9fb26d88914ad66afedab7ab283b unchanged. It closes the file descriptors 1.0b3 leaks on every device listing, backend launch, raster job and driver setup. Also `images/driver/patches/pappl-retrofit-fork-gate.patch`, pagewire's own change: it adds a fork gate to the library. Before the application forks to run a filter, it waits until no other thread holds a pthread lock or is inside a glibc time function (at most 5 seconds; then it forks anyway and logs a line), and a forked child that would wait forever for a lock exits instead, so a print job no longer hangs in processing (pappl-retrofit issue 35). Also `images/driver/patches/pappl-retrofit-filter-reap.patch`, pagewire's own change: it runs each job's filter chain in a child process of its own. That child waits only for its own filters and, as a child subreaper, for the processes they leave behind, so two jobs printing at once on two printers of one application no longer hang in processing, and a finished job leaves no zombie processes. Jobs look up their pre-filter in the shared conversion list one at a time (pappl-retrofit issue 36) | https://github.com/OpenPrinting/pappl-retrofit/tree/1.0b3 |
| ps-printer-app | commit 5ce06c52f8a3a00420587f885e7eb4a238d586ee | Apache-2.0 | Separate program, `pagewire-driver-ps` only | None | https://github.com/OpenPrinting/ps-printer-app |
| hplip-printer-app | commit 90478d9edd063df2756e1a1045aaf1bc4012e3f5 | Apache-2.0 | Separate program, `pagewire-driver-hplip` only | None | https://github.com/OpenPrinting/hplip-printer-app |
| gutenprint-printer-app | commit fbfce9086b3e612447eec729240ab68986608857 | Apache-2.0 | Separate program, `pagewire-driver-gutenprint` only | None | https://github.com/OpenPrinting/gutenprint-printer-app |
| ghostscript-printer-app | commit acf18a476bee525b88526ff8e0154f004b361914 | Apache-2.0 | Separate program, `pagewire-driver-ghostscript` only | None | https://github.com/OpenPrinting/ghostscript-printer-app |
| legacy-printer-app (part of pappl-retrofit) | 1.0b3 | Apache-2.0 (with the CUPS exceptions) | Separate program, `pagewire-driver-legacy` only; its licence is pappl-retrofit's | None | https://github.com/OpenPrinting/pappl-retrofit/tree/1.0b3/legacy |

Their LICENSE and NOTICE files are in `/usr/share/licenses/<component>/`. Each patch carries the licence of the component it changes; they ship in `/usr/share/licenses/pappl/pappl-max-vendor.patch`, in `/usr/share/licenses/pappl/pappl-web-admin-closed.patch`, in `/usr/share/licenses/pappl/pappl-ipp-admin-local.patch`, in `/usr/share/licenses/pappl-retrofit/pappl-retrofit-fd-leak.patch`, in `/usr/share/licenses/pappl-retrofit/pappl-retrofit-fork-gate.patch`, in `/usr/share/licenses/pappl-retrofit/pappl-retrofit-filter-reap.patch` and in the `-sources` images.

The main Ubuntu packages (all unmodified):

| Package (source) | Version | Licence | Images | How included |
|---|---|---|---|---|
| cups, libcups2t64 (cups) | 2.4.16-1ubuntu1.3 | Apache-2.0 (with CUPS's GPL2/LGPL2 exception) | all | Library, plus the CUPS backends run as separate programs |
| libcupsfilters2t64 (libcupsfilters) | 2.1.1-0ubuntu5 | Apache-2.0 (with CUPS's GPL2/LGPL2 exception) | all | Library linked into the application |
| libppd2 (libppd) | 2:2.1.1-0ubuntu2 | Apache-2.0 (with CUPS's GPL2/LGPL2 exception) | all | Library linked into the application |
| cups-filters-core-drivers (cups-filters) | 2.0.1-0ubuntu4.1 | Apache-2.0 (with CUPS's GPL2/LGPL2 exception) | all | Separate programs (filters) |
| libavahi-client3 (avahi) | 0.8-18ubuntu1.1 | LGPL-2.1-or-later | all | Library |
| libgnutls30t64 (gnutls28) | 3.8.12-2ubuntu1.1 | LGPL-2.1-or-later (its bundled libunistring code: LGPL-3.0-or-later OR GPL-2.0-or-later) | all | Library |
| libusb-1.0-0 (libusb-1.0) | 2:1.0.29-2build1 | LGPL-2.1-or-later | all | Library |
| ghostscript (ghostscript) | 10.06.0~dfsg-3ubuntu1.2 | AGPL-3.0-or-later | all five (a dependency in hplip and gutenprint) | Separate program, run by the filters and, in legacy, by pappl-retrofit's PDF and PostScript conversions |
| foomatic-db-compressed-ppds (foomatic-db) | 20250819-1build1 | GPL-2.0-only AND GPL-2.0-or-later AND MIT | ps, ghostscript | PPD data files |
| hplip, printer-driver-hpcups, hplip-data (hplip) | 3.24.4+dfsg0-0ubuntu8.4 | GPL-2.0-or-later AND BSD-3-Clause AND MIT | hplip | Separate programs (the `hp` backend and `hpcups` filter) and data |
| libcurl3t64-gnutls (curl) | 8.18.0-1ubuntu2.8 | curl | all | Library, a dependency of poppler and libproxy |
| libcurl4t64 (curl) | 8.18.0-1ubuntu2.8 | curl | hplip | Library linked into hplip-printer-app |
| libssl3t64 (openssl) | 3.5.5-1ubuntu3.7 | Apache-2.0 | all | Library |
| printer-driver-gutenprint, libgutenprint9 (gutenprint) | 5.3.4.20220624T01008808d602-4ubuntu2 | GPL-2.0-or-later | gutenprint | Separate program (filter) and its library |
| printer-driver-foo2zjs (foo2zjs) | 20200505dfsg0-3ubuntu1 | GPL-2.0-or-later | ghostscript | Separate programs (filters) |
| printer-driver-min12xxw (min12xxw) | 0.0.9-11build4 | GPL-2.0-or-later | ps, ghostscript | Separate program (filter for the foomatic min12xxw drivers both images list) |
| printer-driver-splix (splix) | 2.0.1-2 | GPL-2.0-only | legacy | Separate programs (filters) and `.drv` driver files |
| printer-driver-brlaser (brlaser) | 6.2.8-1 | GPL-2.0-or-later | legacy | Separate program (filter) and a `.drv` driver file |
| printer-driver-escpr (epson-inkjet-printer-escpr) | 1.8.7-1 | GPL-2.0-or-later | legacy | Separate programs (filters and a driver program that lists the PPDs) |
| printer-driver-ptouch (ptouch-driver) | 1.7.1-1build1 | GPL-2.0-or-later | legacy | Separate programs (filter and a driver program that lists the PPDs) |
| printer-driver-c2esp (c2esp) | 27-11ubuntu8 | GPL-2.0-or-later | legacy | Separate programs (filters) and `.drv` driver files |
| printer-driver-sag-gdi (rastertosag-gdi) | 0.1-8build1 | GPL (its copyright file names no version) | legacy | Separate program (filter) and a `.drv` driver file |

In `pagewire-driver-legacy`, `/usr/share/pagewire/legacy-drivers` holds links to exactly these six packages' driver files, so the application lists no other drivers.

The number of source packages per image at the time of writing: ps 152, hplip 182, gutenprint 146, ghostscript 154, legacy 156, including the `ubuntu:26.04` base.

In `pagewire-driver-hplip` the HP plug-in paths are symlinks into the vendor-file volume, as in the device image.

## Corresponding source

For every published image tag, CI publishes a matching `-sources` image for the device image and each driver image: `ghcr.io/thephaseless/<image>:<tag>-sources`, for example `main-sources`, `1.2.3-sources`, `1.2-sources` and `1-sources`. The source for `latest` is the newest `<version>-sources`. The `SOURCES` file and the `io.github.thephaseless.pagewire.sources` label in each image name its `-sources` image.

Each is built from the `sources` stage of the image's Dockerfile and holds under `/sources`:

- `ubuntu/`: the Ubuntu source packages (`.dsc`, orig and Debian tarballs) for every line of the image's `source-packages.txt`, fetched with `apt-get source`. A version the Ubuntu archive has replaced since the image was built comes from the Ubuntu snapshot service (https://snapshot.ubuntu.com) at one of the times in the image's `/usr/share/pagewire/apt-snapshots.txt`, taken before and after each package installation. In CI the list is the union of the amd64 and arm64 lists read from the images that the same run pushed (`sha-<commit>`), so it matches the published binaries. A local build of the `sources` target uses the lists of the targets it builds, the union of all five for the driver Dockerfile;
- `upstream/`: `git archive` tarballs of everything built from upstream git, at the pinned commit (ipp-usb and AirSane; PAPPL, pappl-retrofit, which also holds legacy-printer-app, and the four other applications). The device image's rebuilt xerox_mfp backend comes from the sane-backends package in `ubuntu/` with its patch in `patches/`;
- `patches/`: the driver images' PAPPL and pappl-retrofit patches and the device image's sane-backends patch;
- `build/`: the Dockerfile, `apt-source-fetch.sh` (which downloads the Ubuntu source packages) and, for the device image, its config files.

To extract one: `docker buildx build -f images/driver/Dockerfile --target sources -o type=local,dest=out .` from a checkout of the same commit, or copy `/sources` out of the published `-sources` image. Use the `-sources` image with the same tag as the image it covers.

The control-plane image has no `-sources` image. Its Go and npm dependencies are permissively licensed and listed above. For the distroless Debian packages, see the pointers in that section.

## Explicitly not included

These are never in an image, in this repository or in CI:

- HP's HPLIP proprietary plug-in (the `hplip-<version>-plugin.run` files) and its licence text: pagewire downloads the archive at runtime on your cluster, from OpenPrinting's mirror listed in `internal/catalog/catalog.yaml`. The device pod's agent downloads it into the vendor-file volume before acceptance, checks it against HP's detached OpenPGP signature (key in `internal/catalog/keys/hplip.asc`) and the sha256 pinned in the catalog, and reads HP's licence text from inside it, so the wizard and Settings can show that text. The `agent fetch` step in the `fetch-blobs` init container installs the plug-in only when the config accepts HP's licence (`licenses.acceptedEulas` with `id: hplip-plugin`, the matching version and the sha256 of the text shown), and only if the text in the archive still has that sha256. It unpacks the plug-in into the vendor-file volume.
- hplip-printer-app also has its own plug-in installer, a page of its web interface that pagewire's PAPPL patch (`images/driver/patches/pappl-web-admin-closed.patch`) refuses to every client. pagewire never opens that page. Its plug-in paths lead into `/blobs`. The driver container mounts the vendor-file volume there only after you accepted HP's licence, and then read-only; before that, `/blobs` does not exist on the read-only root file system. So hplip-printer-app could install the plug-in itself only into a mounted `/blobs`, which happens after acceptance, and the read-only mount stops even that.
- foo2zjs printer firmware: Ubuntu's `printer-driver-foo2zjs` ships an empty `/var/lib/foo2zjs/firmware`, and pagewire adds nothing there. Printers that need a firmware upload from the host are not supported through foo2zjs.
- Brother, Epson and Canon proprietary drivers: The images contain only free drivers (Gutenprint, Ghostscript and foomatic, foo2zjs, min12xxw, SpliX, brlaser, Epson's GPL ESC/P-R driver, ptouch, c2esp, SAG-GDI, HPLIP's free parts) and the SANE backends Ubuntu ships.
- socket-proxy (`ghcr.io/wollomatic/socket-proxy`, MIT AND Apache-2.0): `compose.docker-proxy.yaml` names its image by digest, and Docker pulls it from its publisher. pagewire does not ship or rebuild it; its licence and source are at https://github.com/wollomatic/socket-proxy
- The Mopria eSCL specification: pagewire's eSCL code follows the open implementations go-mfp and sane-airscan, never the specification text.

## Regenerating these lists

- Go modules: `mise x -- go tool go-licenses report ./cmd/pagewire`.
- npm packages: `bun run build` in `ui/` writes `dist/THIRD_PARTY_LICENSES.txt`.
- Everything in an image, from a build: `docker buildx build --sbom=true -o type=local,dest=out .` (add `-f images/... --target ...` for the other images), then read `out/sbom*.spdx.json`. For a published image: `docker buildx imagetools inspect ghcr.io/thephaseless/<image>:<tag> --format '{{ json .SBOM }}'`.
- Ubuntu packages in a device or driver image: `docker run --rm --entrypoint cat <image> /usr/share/pagewire/source-packages.txt`.
- Distroless packages: `docker create` the pinned digest from `Dockerfile`, then `docker cp <container>:/var/lib/dpkg/status.d/ .` and read each package's `/usr/share/doc/<package>/copyright` the same way.
