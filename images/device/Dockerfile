# printscan device image: dbus, avahi, ipp-usb, AirSane, the SANE backends and
# ippeveprinter. Always build with an explicit target:
#   docker buildx build -f images/device/Dockerfile --target device .
#   docker buildx build -f images/device/Dockerfile --target sources -o type=local,dest=out .

ARG IPP_USB_VERSION=0.9.34 # renovate: datasource=github-tags depName=OpenPrinting/ipp-usb
ARG AIRSANE_VERSION=v0.4.16 # renovate: datasource=github-tags depName=SimulPiscator/AirSane
# Fixed system uids, so the renderer can set runAsUser (also recorded in LABELs below).
ARG MESSAGEBUS_UID=101
ARG AVAHI_UID=102
ARG NONROOT_UID=65532

FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS ipp-usb
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG IPP_USB_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends libusb-1.0-0-dev libavahi-client-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch "${IPP_USB_VERSION}" https://github.com/OpenPrinting/ipp-usb.git . \
 && go build -trimpath -mod=vendor -tags nethttpomithttp2 -ldflags "-s -w" -o /out/ipp-usb . \
 && mkdir -p /out/quirks \
 && cp ipp-usb-quirks/*.conf /out/quirks/ \
 # The quirks directory this image installs to is one ipp-usb searches by default.
 && grep -q 'DefaultPathGlobalQuirksDir = "/usr/share/ipp-usb/quirks"' paths.go

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS airsane
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG AIRSANE_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git make cmake g++ \
      libsane-dev libavahi-client-dev libjpeg-dev libpng-dev libusb-1.0-0-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 --branch "${AIRSANE_VERSION}" https://github.com/SimulPiscator/AirSane.git . \
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
COPY --from=airsane /out/airsaned /usr/local/bin/airsaned
COPY --from=airsane /src/LICENSE /usr/share/licenses/airsane/LICENSE
COPY images/device/dbus.conf images/device/avahi-daemon.conf /etc/printscan/
# HP plug-in paths point into the blob PVC (decision 14). mkdir the parents,
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
# Last RUN that touches packages: the Corresponding source list (decision 20).
RUN mkdir -p /usr/share/printscan \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/printscan/source-packages.txt \
 && test -s /usr/share/printscan/source-packages.txt
LABEL dev.printscan.uid.messagebus="${MESSAGEBUS_UID}" \
      dev.printscan.uid.avahi="${AVAHI_UID}" \
      dev.printscan.uid.nonroot="${NONROOT_UID}"
EXPOSE 8090 8631 60000-60009

FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS sources-fetch
ARG IPP_USB_VERSION
ARG AIRSANE_VERSION
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources
COPY --from=device /usr/share/printscan/source-packages.txt /sources/source-packages.txt
WORKDIR /sources/ubuntu
# Lines are source package names (--only-source: never mapped through binary names,
# e.g. coreutils vs coreutils-from). apt-get names any line it cannot resolve and fails.
RUN chown _apt /sources/ubuntu \
 && apt-get update \
 && xargs apt-get source --only-source --download-only < /sources/source-packages.txt \
 && test "$(ls *.dsc | wc -l)" = "$(wc -l < /sources/source-packages.txt)"
ADD https://github.com/OpenPrinting/ipp-usb/archive/refs/tags/${IPP_USB_VERSION}.tar.gz /sources/upstream/ipp-usb-${IPP_USB_VERSION}.tar.gz
ADD https://github.com/SimulPiscator/AirSane/archive/refs/tags/${AIRSANE_VERSION}.tar.gz /sources/upstream/AirSane-${AIRSANE_VERSION}.tar.gz

FROM scratch AS sources
COPY --from=sources-fetch /sources /sources
