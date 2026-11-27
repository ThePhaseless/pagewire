# pagewire device image: dbus, avahi, ipp-usb, AirSane, the SANE backends (xerox_mfp
# rebuilt with a patch from images/device/patches/) and ippeveprinter. Always build
# with an explicit target:
#   docker buildx build -f images/device/Dockerfile --target device .
#   docker buildx build -f images/device/Dockerfile --target sources -o type=local,dest=out .
# The sane-index target builds internal/catalog/sane-usb.tsv from the device stage's backends:
#   docker buildx build -f images/device/Dockerfile --target sane-index -o type=local,dest=.cache/idx .
# The sources target lists the packages of the local device stage by default. CI passes
# SOURCES_LISTS=pushed and PUSHED_IMAGE=<the image it pushed for this commit>, so the
# list is the union of that image's amd64 and arm64 package lists instead.

# Upstreams are built by commit; the Renovate comment keeps the release tag.
ARG IPP_USB_COMMIT=057c7ac8739e557d12796378f3c180136cb3e274 # renovate: datasource=github-tags depName=OpenPrinting/ipp-usb currentValue=0.9.34
ARG AIRSANE_COMMIT=e96627ae7a85e700fbb353b23deacfba868869ad # renovate: datasource=github-tags depName=SimulPiscator/AirSane currentValue=v0.4.16
ARG VERSION=dev
ARG SOURCES_LISTS=local
# A sentinel: the pushed-* stages that read it are never built in local mode.
ARG PUSHED_IMAGE=scratch
# Fixed system uids, so the renderer can set runAsUser (also recorded in LABELs below).
ARG MESSAGEBUS_UID=101
ARG AVAHI_UID=102
ARG NONROOT_UID=65532

FROM golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS ipp-usb
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG IPP_USB_COMMIT
RUN apt-get update \
 && apt-get install -y --no-install-recommends libusb-1.0-0-dev libavahi-client-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init -q . \
 && git fetch -q --depth 1 https://github.com/OpenPrinting/ipp-usb.git "${IPP_USB_COMMIT}" \
 && git checkout -q FETCH_HEAD \
 && mkdir -p /out/upstream /out/quirks \
 && git archive --prefix="ipp-usb-${IPP_USB_COMMIT}/" -o "/out/upstream/ipp-usb-${IPP_USB_COMMIT}.tar.gz" HEAD \
 && go build -trimpath -mod=vendor -tags nethttpomithttp2 -ldflags "-s -w" -o /out/ipp-usb . \
 && cp ipp-usb-quirks/*.conf /out/quirks/ \
 # The binary links the Go standard library and the vendored goipp, each under its own licence.
 && cp "$(go env GOROOT)/LICENSE" /out/LICENSE.go \
 && cp vendor/github.com/OpenPrinting/goipp/LICENSE /out/LICENSE.goipp \
 # The quirks directory this image installs to is one ipp-usb searches by default.
 && grep -q 'DefaultPathGlobalQuirksDir = "/usr/share/ipp-usb/quirks"' paths.go

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS airsane
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG AIRSANE_COMMIT
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git make cmake g++ \
      libsane-dev libavahi-client-dev libjpeg-dev libpng-dev libusb-1.0-0-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init -q . \
 && git fetch -q --depth 1 https://github.com/SimulPiscator/AirSane.git "${AIRSANE_COMMIT}" \
 && git checkout -q FETCH_HEAD \
 && mkdir -p /out/upstream \
 && git archive --prefix="AirSane-${AIRSANE_COMMIT}/" -o "/out/upstream/AirSane-${AIRSANE_COMMIT}.tar.gz" HEAD \
 && cmake -S . -B build -DCMAKE_BUILD_TYPE=MinSizeRel \
 && cmake --build build -j"$(nproc)" \
 && install -D -m 0755 build/airsaned /out/airsaned

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS device-base
ARG MESSAGEBUS_UID
ARG AVAHI_UID
ARG NONROOT_UID
# Users first, so the dbus and avahi-daemon packages keep these uids.
RUN groupadd --system --gid "${MESSAGEBUS_UID}" messagebus \
 && useradd --system --uid "${MESSAGEBUS_UID}" --gid messagebus --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin messagebus \
 && groupadd --system --gid "${AVAHI_UID}" avahi \
 && useradd --system --uid "${AVAHI_UID}" --gid avahi --home-dir /run/avahi-daemon --no-create-home --shell /usr/sbin/nologin avahi \
 && groupadd --gid "${NONROOT_UID}" nonroot \
 && useradd --uid "${NONROOT_UID}" --gid nonroot --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin nonroot
# The UTC times before and after the apt run name the Ubuntu snapshots
# (snapshot.ubuntu.com) that hold the image's package versions, for sources-fetch.
# shortcut: a mirror lagging the publisher can make both times name a newer state; record each suite's InRelease Date if sources-fetch ever names such a line.
RUN mkdir -p /usr/share/pagewire \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      dbus avahi-daemon libavahi-client3 libusb-1.0-0 libsane1 libsane-hpaio \
      cups-ipp-utils libjpeg8 libpng16-16t64 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && test "$(id -u messagebus)" = "${MESSAGEBUS_UID}" \
 && test "$(id -u avahi)" = "${AVAHI_UID}"
# The source packages the sources target downloads as the corresponding source of
# this image. No later step installs or removes packages (the device stage only
# replaces libsane1's xerox_mfp file with one built from the same source).
RUN mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt

# The sane-backends .desc files of the installed version: pixma, epson2 and others
# hard-code their USB IDs, which only these files list (the image has no /usr/share/doc).
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS sane-desc
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends bzip2 xz-utils
COPY images/apt-source-fetch.sh /usr/local/bin/
COPY --from=device-base /usr/share/pagewire/source-packages.txt /usr/share/pagewire/apt-snapshots.txt /src/
WORKDIR /src
RUN grep '^sane-backends=' source-packages.txt > sane-backends.txt \
 && chown _apt /src \
 && sh /usr/local/bin/apt-source-fetch.sh sane-backends.txt apt-snapshots.txt \
 && mkdir -p /out \
 && tar -xf "$(ls sane-backends_*.orig.tar.* | grep -v '\.asc$')" -C /out --strip-components=2 \
      --wildcards '*/doc/descriptions/*.desc' '*/doc/descriptions-external/*.desc' \
 && ls /out/descriptions/*.desc /out/descriptions-external/*.desc > /dev/null

# libsane1's xerox_mfp backend rebuilt from the same Ubuntu source package with
# images/device/patches/sane-backends-xerox_mfp-adf.patch, which offers the ADF
# sources only on devices that report an ADF. Only this backend is built.
FROM sane-desc AS xerox-mfp
ARG BUILDKIT_SBOM_SCAN_STAGE=true
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential autoconf automake libtool \
      autoconf-archive autopoint gettext pkg-config dpkg-dev python3-minimal \
      libusb-1.0-0-dev libjpeg-dev libxml2-dev
COPY --from=device-base /usr/lib/*-linux-gnu/sane/ /distro-sane/
COPY images/device/patches/sane-backends-xerox_mfp-adf.patch /src/
WORKDIR /src
RUN dpkg-source -x "$(ls sane-backends_*.dsc)" /src/sane \
 && cd /src/sane \
 # The patch's assumptions: one source list for every device, has_adf read before init_options().
 && grep -qF 'dev->opt[OPT_SOURCE].constraint.string_list = doc_sources;' backend/xerox_mfp.c \
 && grep -qF 'dev->has_adf = ((dev->res[0x26] & 0x03) != 0);' backend/xerox_mfp.c \
 && patch -p1 --fuzz=0 < /src/sane-backends-xerox_mfp-adf.patch \
 && export DEB_BUILD_MAINT_OPTIONS=hardening=+all && eval "$(dpkg-buildflags --export=sh)" \
 && autoreconf -fi \
 && ma="$(dpkg-architecture -qDEB_HOST_MULTIARCH)" \
 # debian/rules' configure flags that reach this backend; BACKENDS limits the build to it.
 && ./configure --prefix=/usr --libdir="/usr/lib/${ma}" --sysconfdir=/etc --localstatedir=/var \
      --enable-pthread --with-usb --disable-locking BACKENDS=xerox_mfp \
 && make -C lib -j"$(nproc)" && make -C sanei -j"$(nproc)" \
 && make -C backend -j"$(nproc)" libsane-xerox_mfp.la \
 && so="$(readlink backend/.libs/libsane-xerox_mfp.so.1)" \
 # It replaces libsane1's file of the same name: same libraries and exported symbols.
 && test -f "/distro-sane/${so}" \
 && readelf -d "/distro-sane/${so}" | grep NEEDED | sort > /tmp/needed.distro \
 && readelf -d "backend/.libs/${so}" | grep NEEDED | sort > /tmp/needed.new \
 && cmp /tmp/needed.distro /tmp/needed.new \
 && nm -D --defined-only "/distro-sane/${so}" | awk '{print $3}' | sort > /tmp/syms.distro \
 && nm -D --defined-only "backend/.libs/${so}" | awk '{print $3}' | sort > /tmp/syms.new \
 && cmp /tmp/syms.distro /tmp/syms.new \
 && mkdir -p "/xerox-mfp/usr/lib/${ma}/sane" \
 && strip --strip-unneeded -o "/xerox-mfp/usr/lib/${ma}/sane/${so}" "backend/.libs/${so}" \
 && chmod 0644 "/xerox-mfp/usr/lib/${ma}/sane/${so}" \
 && install -D -m 0644 -t /xerox-mfp/usr/share/licenses/sane-backends LICENSE COPYING \
      /src/sane-backends-xerox_mfp-adf.patch

FROM device-base AS device
ARG MESSAGEBUS_UID
ARG AVAHI_UID
ARG NONROOT_UID
COPY --from=ipp-usb /out/ipp-usb /usr/local/sbin/ipp-usb
COPY --from=ipp-usb /out/quirks/ /usr/share/ipp-usb/quirks/
COPY --from=ipp-usb /src/LICENSE /usr/share/licenses/ipp-usb/LICENSE
COPY --from=ipp-usb /out/LICENSE.go /out/LICENSE.goipp /usr/share/licenses/ipp-usb/
COPY --from=airsane /out/airsaned /usr/local/bin/airsaned
COPY --from=airsane /src/LICENSE /usr/share/licenses/airsane/LICENSE
# libsane1's xerox_mfp backend, replaced by the patched build (same file name, checked in xerox-mfp).
COPY --from=xerox-mfp /xerox-mfp/ /
COPY images/device/dbus.conf images/device/avahi-daemon.conf /etc/pagewire/
# HP plug-in paths point into the blob PVC, where the plug-in is installed after
# the user accepts HP's licence; the image never contains it. mkdir the parents,
# remove whatever is at each link path, then link, so no link lands inside a directory.
RUN mkdir -p /usr/share/hplip/prnt /usr/share/hplip/scan /usr/share/hplip/fax /usr/share/hplip/data /var/lib/hp \
 && for p in prnt/plugins scan/plugins fax/plugins data/firmware; do \
      rm -rf "/usr/share/hplip/$p" && ln -s "/blobs/hplip-plugin/$p" "/usr/share/hplip/$p"; \
    done \
 && rm -rf /var/lib/hp/hplip.state \
 && ln -s /blobs/hplip-plugin/hplip.state /var/lib/hp/hplip.state \
 && test -x /usr/bin/dbus-daemon && test -x /usr/sbin/avahi-daemon && test -x /usr/sbin/ippeveprinter \
 && test -e "/usr/lib/$(uname -m)-linux-gnu/sane/libsane-hpaio.so.1" \
 && test -e "/usr/lib/$(uname -m)-linux-gnu/sane/libsane-test.so.1" \
 && tr '\0' '\n' < "/usr/lib/$(uname -m)-linux-gnu/sane/libsane-xerox_mfp.so.1" | grep -qx '%s: ADF present: %s' \
 && if ldd /usr/local/sbin/ipp-usb /usr/local/bin/airsaned "/usr/lib/$(uname -m)-linux-gnu/sane/libsane-xerox_mfp.so.1" | grep 'not found'; then exit 1; fi
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-device
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
LABEL io.github.thephaseless.pagewire.uid.messagebus="${MESSAGEBUS_UID}" \
      io.github.thephaseless.pagewire.uid.avahi="${AVAHI_UID}" \
      io.github.thephaseless.pagewire.uid.nonroot="${NONROOT_UID}"
EXPOSE 8090 8631 60000-60009

# The SANE USB index (internal/catalog/sane-usb.tsv) from the device stage's backends.
FROM golang:1.27.4-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS sane-index-build
WORKDIR /src
COPY go.mod go.sum ./
COPY tools/saneindex/ tools/saneindex/
COPY --from=device /etc/sane.d/ /in/sane.d/
COPY --from=device /usr/lib/*-linux-gnu/sane/ /in/libs/
COPY --from=sane-desc /out/ /in/desc/
RUN mkdir /out \
 && GOTOOLCHAIN=local CGO_ENABLED=0 go run ./tools/saneindex -o /out/sane-usb.tsv \
      -conf /in/sane.d -dll /in/sane.d/dll.conf -libs /in/libs \
      -desc /in/desc/descriptions -desc /in/desc/descriptions-external

FROM scratch AS sane-index
COPY --from=sane-index-build /out/ /

# Package lists for the sources target, chosen by SOURCES_LISTS.
FROM scratch AS lists-local
COPY --from=device /usr/share/pagewire/source-packages.txt /lists/device.txt
COPY --from=device /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/device.txt

# The pushed stages only COPY, so the arm64 one needs no emulation.
FROM --platform=linux/amd64 ${PUSHED_IMAGE} AS pushed-amd64
FROM --platform=linux/arm64 ${PUSHED_IMAGE} AS pushed-arm64

FROM scratch AS lists-pushed
COPY --from=pushed-amd64 /usr/share/pagewire/source-packages.txt /lists/amd64.txt
COPY --from=pushed-arm64 /usr/share/pagewire/source-packages.txt /lists/arm64.txt
COPY --from=pushed-amd64 /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/amd64.txt
COPY --from=pushed-arm64 /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/arm64.txt

FROM lists-${SOURCES_LISTS} AS lists

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS sources-fetch
# docker-clean turns apt's binary caches off; without them each apt-get call below takes 10 times as long.
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources \
 && rm -f /etc/apt/apt.conf.d/docker-clean
COPY images/apt-source-fetch.sh /usr/local/bin/
COPY --from=lists /lists/ /sources/lists/
WORKDIR /sources/ubuntu
# apt-source-fetch.sh falls back to the recorded snapshots for versions Ubuntu has superseded.
RUN sort -u /sources/lists/*.txt > /sources/source-packages.txt \
 && chown _apt /sources/ubuntu \
 && sh /usr/local/bin/apt-source-fetch.sh /sources/source-packages.txt /sources/lists/snapshots/*.txt \
 && test "$(ls *.dsc | wc -l)" = "$(wc -l < /sources/source-packages.txt)"
# Upstream sources as git archives of the pinned commits the binaries were built from.
COPY --from=ipp-usb /out/upstream/ /sources/upstream/
COPY --from=airsane /out/upstream/ /sources/upstream/
# The patches applied to Ubuntu source packages built here.
COPY images/device/patches/ /sources/patches/
# How the image was built.
COPY images/device/Dockerfile images/device/dbus.conf images/device/avahi-daemon.conf images/apt-source-fetch.sh /sources/build/

FROM scratch AS sources
COPY --from=sources-fetch /sources /sources
