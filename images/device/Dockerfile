# pagewire device image: dbus, avahi, ipp-usb, AirSane, the SANE backends and
# ippeveprinter. Always build with an explicit target:
#   docker buildx build -f images/device/Dockerfile --target device .
#   docker buildx build -f images/device/Dockerfile --target sources -o type=local,dest=out .
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

FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS ipp-usb
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

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS device
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
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      dbus avahi-daemon libavahi-client3 libusb-1.0-0 libsane1 libsane-hpaio \
      cups-ipp-utils libjpeg8 libpng16-16t64 ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && test "$(id -u messagebus)" = "${MESSAGEBUS_UID}" \
 && test "$(id -u avahi)" = "${AVAHI_UID}"
COPY --from=ipp-usb /out/ipp-usb /usr/local/sbin/ipp-usb
COPY --from=ipp-usb /out/quirks/ /usr/share/ipp-usb/quirks/
COPY --from=ipp-usb /src/LICENSE /usr/share/licenses/ipp-usb/LICENSE
COPY --from=ipp-usb /out/LICENSE.go /out/LICENSE.goipp /usr/share/licenses/ipp-usb/
COPY --from=airsane /out/airsaned /usr/local/bin/airsaned
COPY --from=airsane /src/LICENSE /usr/share/licenses/airsane/LICENSE
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
 && if ldd /usr/local/sbin/ipp-usb /usr/local/bin/airsaned | grep 'not found'; then exit 1; fi
# Last RUN that touches packages: the source packages the sources target downloads
# as the corresponding source of this image.
RUN mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-device
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL dev.pagewire.sources="${SOURCES_REF}"
LABEL dev.pagewire.uid.messagebus="${MESSAGEBUS_UID}" \
      dev.pagewire.uid.avahi="${AVAHI_UID}" \
      dev.pagewire.uid.nonroot="${NONROOT_UID}"
EXPOSE 8090 8631 60000-60009

# Package lists for the sources target, chosen by SOURCES_LISTS.
FROM scratch AS lists-local
COPY --from=device /usr/share/pagewire/source-packages.txt /lists/device.txt

# The pushed stages only COPY, so the arm64 one needs no emulation.
FROM --platform=linux/amd64 ${PUSHED_IMAGE} AS pushed-amd64
FROM --platform=linux/arm64 ${PUSHED_IMAGE} AS pushed-arm64

FROM scratch AS lists-pushed
COPY --from=pushed-amd64 /usr/share/pagewire/source-packages.txt /lists/amd64.txt
COPY --from=pushed-arm64 /usr/share/pagewire/source-packages.txt /lists/arm64.txt

FROM lists-${SOURCES_LISTS} AS lists

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS sources-fetch
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources
COPY --from=lists /lists/ /sources/lists/
WORKDIR /sources/ubuntu
# Lines are source package names (--only-source: never mapped through binary names,
# e.g. coreutils vs coreutils-from). apt-get names any line it cannot resolve and fails.
RUN sort -u /sources/lists/*.txt > /sources/source-packages.txt \
 && chown _apt /sources/ubuntu \
 && apt-get update \
 && xargs apt-get source --only-source --download-only < /sources/source-packages.txt \
 && test "$(ls *.dsc | wc -l)" = "$(wc -l < /sources/source-packages.txt)"
# Upstream sources as git archives of the pinned commits the binaries were built from.
COPY --from=ipp-usb /out/upstream/ /sources/upstream/
COPY --from=airsane /out/upstream/ /sources/upstream/
# How the image was built.
COPY images/device/Dockerfile images/device/dbus.conf images/device/avahi-daemon.conf /sources/build/

FROM scratch AS sources
COPY --from=sources-fetch /sources /sources
