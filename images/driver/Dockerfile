# printscan driver images: PAPPL retrofit Printer Applications built the way
# the upstream rock recipes (rockcraft.yaml) build them, without their
# avahi-daemon and dbus parts (the device pod's single Avahi comes from the
# device image), and with Ubuntu packages for CUPS, its backends, the filters
# and the drivers. Always build with an explicit target:
#   docker buildx build -f images/driver/Dockerfile --target ps .
#   (ps, hplip, gutenprint, ghostscript)
#   docker buildx build -f images/driver/Dockerfile --target sources -o type=local,dest=out .

ARG PAPPL_VERSION=v1.4.12 # renovate: datasource=github-tags depName=michaelrsweet/pappl
ARG PAPPL_RETROFIT_VERSION=1.0b3 # renovate: datasource=github-tags depName=OpenPrinting/pappl-retrofit
ARG PS_PRINTER_APP_COMMIT=5ce06c52f8a3a00420587f885e7eb4a238d586ee # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/ps-printer-app currentValue=master
ARG HPLIP_PRINTER_APP_COMMIT=90478d9edd063df2756e1a1045aaf1bc4012e3f5 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/hplip-printer-app currentValue=master
ARG GUTENPRINT_PRINTER_APP_COMMIT=fbfce9086b3e612447eec729240ab68986608857 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/gutenprint-printer-app currentValue=master
ARG GHOSTSCRIPT_PRINTER_APP_COMMIT=acf18a476bee525b88526ff8e0154f004b361914 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/ghostscript-printer-app currentValue=master

# PAPPL and pappl-retrofit, both under /usr/local. The rock's PAPPL edits are in
# images/driver/patches/pappl-max-vendor.patch (PAPPL_MAX_VENDOR is still 32 in 1.4.12).
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS pappl-base
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG PAPPL_VERSION
ARG PAPPL_RETROFIT_VERSION
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git build-essential \
      autoconf automake libtool autotools-dev autopoint gettext pkg-config perl-base \
      libcups2-dev libcupsfilters-dev libppd-dev libgnutls28-dev libavahi-client-dev \
      libjpeg-dev libpng-dev libusb-1.0-0-dev libpam0g-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*
COPY images/driver/patches/pappl-max-vendor.patch /src/
WORKDIR /src/pappl
RUN git clone --depth 1 --branch "${PAPPL_VERSION}" https://github.com/michaelrsweet/pappl.git . \
 && patch -p1 < /src/pappl-max-vendor.patch \
 && ./configure --prefix=/usr/local --enable-libjpeg --enable-libpng --enable-libusb --with-dnssd=avahi \
 && make -j"$(nproc)" \
 && make install \
 && install -D -m 0644 -t /out/licenses/pappl LICENSE NOTICE
WORKDIR /src/pappl-retrofit
RUN git clone --depth 1 --branch "${PAPPL_RETROFIT_VERSION}" https://github.com/OpenPrinting/pappl-retrofit.git . \
 && ./autogen.sh \
 && ./configure --prefix=/usr/local \
 && make -j"$(nproc)" \
 && make install \
 && ldconfig \
 && install -D -m 0644 -t /out/licenses/pappl-retrofit LICENSE NOTICE COPYING \
 # A2: pappl-retrofit runs CUPS backends from "/usr/lib/<app>/backend" (a
 # symlink to CUPS's serverbin that the apps' "make install" makes only for
 # prefix /usr), not from CUPS's /usr/lib/cups/backend. The targets below
 # set BACKEND_DIR and FILTER_DIR, which pappl-retrofit reads at start-up.
 && grep -q '"/usr/lib/%s/backend"' pappl-retrofit/pappl-retrofit.c \
 && grep -q 'getenv("BACKEND_DIR")' pappl-retrofit/pappl-retrofit.c

# Each retrofit app at its pinned commit, built as the rock's app part builds it
# ("make LDFLAGS=-ljpeg", then "make install"), under /usr/local.
# A1: every app must use pappl-retrofit's CUPS backends only.
COPY --chmod=0755 <<'SCRIPT' /src/build-app.sh
#!/bin/sh
# usage: build-app.sh <app> <commit>
set -eux
app="$1"
mkdir -p /src/app
cd /src/app
git init -q .
git fetch -q --depth 1 "https://github.com/OpenPrinting/${app}.git" "$2"
git checkout -q FETCH_HEAD
grep -q 'PR_COPTIONS_NO_PAPPL_BACKENDS' "${app}.c"
grep -q 'PR_COPTIONS_CUPS_BACKENDS' "${app}.c"
make -j"$(nproc)" prefix=/usr/local LDFLAGS=-ljpeg
make install prefix=/usr/local
# The rock ships pappl-retrofit's test page as the app's test page.
cp /usr/local/share/legacy-printer-app/testpage.pdf "/usr/local/share/${app}/"
# Drop build-only files, the unused legacy-printer-app and the app's serverbin
# (BACKEND_DIR and FILTER_DIR point at /usr/lib/cups instead).
cd /usr/local
rm -rf include lib/pkgconfig lib/*.a lib/*.la bin/pappl-makeresheader bin/legacy-printer-app lib/legacy-printer-app share/legacy-printer-app "lib/${app}"
cd /src/app
install -D -m 0644 -t "/out/licenses/${app}" LICENSE NOTICE
SCRIPT

FROM pappl-base AS ps-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG PS_PRINTER_APP_COMMIT
RUN /src/build-app.sh ps-printer-app "${PS_PRINTER_APP_COMMIT}"

FROM pappl-base AS hplip-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG HPLIP_PRINTER_APP_COMMIT
# hplip-printer-app also links libcurl and libcrypto (its own plug-in downloader).
RUN apt-get update \
 && apt-get install -y --no-install-recommends libcurl4-openssl-dev libssl-dev \
 && rm -rf /var/lib/apt/lists/* \
 && /src/build-app.sh hplip-printer-app "${HPLIP_PRINTER_APP_COMMIT}"

FROM pappl-base AS gutenprint-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG GUTENPRINT_PRINTER_APP_COMMIT
RUN /src/build-app.sh gutenprint-printer-app "${GUTENPRINT_PRINTER_APP_COMMIT}"

FROM pappl-base AS ghostscript-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG GHOSTSCRIPT_PRINTER_APP_COMMIT
RUN /src/build-app.sh ghostscript-printer-app "${GHOSTSCRIPT_PRINTER_APP_COMMIT}"

# Shared runtime: CUPS (for its usb, socket and ipp backends; cupsd is never
# started) and the libraries PAPPL and pappl-retrofit link against.
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS runtime
ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends ca-certificates cups \
      libcups2t64 libcupsfilters2t64 libppd2 libavahi-client3 libgnutls30t64 \
      libjpeg8 libpng16-16t64 libusb-1.0-0 libpam0g zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && useradd --uid 10001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin printscan
ENV BACKEND_DIR=/usr/lib/cups/backend \
    FILTER_DIR=/usr/lib/cups/filter

FROM runtime AS ps
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=ps-printer-app
RUN apt-get update \
 && apt-get install -y --no-install-recommends foomatic-db-compressed-ppds ghostscript cups-filters-core-drivers \
 && rm -rf /var/lib/apt/lists/*
COPY --from=ps-build /usr/local /usr/local
COPY --from=ps-build /out/licenses /usr/share/licenses
# Writable at run time (emptyDirs in the pod): /var/lib/<app>, /var/spool/<app>, /tmp,
# /run/dbus, and /etc/cups/ssl, where PAPPL writes its self-signed HTTPS/IPPS
# certificate (without it, TLS connections fail). Optional: /run, for the
# app's local CLI socket /run/<app>.sock (the agent uses loopback IPP instead).
# Same for every target.
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/printscan \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/printscan/source-packages.txt \
 && test -s /usr/share/printscan/source-packages.txt
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
EXPOSE 8001
CMD ["/usr/local/bin/ps-printer-app", "-o", "server-port=8001", "server"]

FROM runtime AS hplip
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=hplip-printer-app
RUN apt-get update \
 && apt-get install -y --no-install-recommends 'hplip=3.24.4*' printer-driver-hpcups hplip-data libcurl4t64 libssl3t64 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=hplip-build /usr/local /usr/local
COPY --from=hplip-build /out/licenses /usr/share/licenses
# HP plug-in paths point into the blob PVC (decision 14). mkdir the parents,
# remove whatever is at each link path, then link, so no link lands inside a directory.
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && mkdir -p /usr/share/hplip/prnt /usr/share/hplip/scan /usr/share/hplip/fax /usr/share/hplip/data /var/lib/hp \
 && for p in prnt/plugins scan/plugins fax/plugins data/firmware; do \
      rm -rf "/usr/share/hplip/$p" && ln -s "/blobs/hplip-plugin/$p" "/usr/share/hplip/$p"; \
    done \
 && rm -rf /var/lib/hp/hplip.state \
 && ln -s /blobs/hplip-plugin/hplip.state /var/lib/hp/hplip.state \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && test -x /usr/lib/cups/backend/hp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/printscan \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/printscan/source-packages.txt \
 && test -s /usr/share/printscan/source-packages.txt
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
EXPOSE 8002
CMD ["/usr/local/bin/hplip-printer-app", "-o", "server-port=8002", "server"]

FROM runtime AS gutenprint
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=gutenprint-printer-app
RUN apt-get update \
 && apt-get install -y --no-install-recommends printer-driver-gutenprint libgutenprint9 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=gutenprint-build /usr/local /usr/local
COPY --from=gutenprint-build /out/licenses /usr/share/licenses
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/printscan \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/printscan/source-packages.txt \
 && test -s /usr/share/printscan/source-packages.txt
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
EXPOSE 8003
CMD ["/usr/local/bin/gutenprint-printer-app", "-o", "server-port=8003", "server"]

FROM runtime AS ghostscript
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=ghostscript-printer-app
RUN apt-get update \
 && apt-get install -y --no-install-recommends ghostscript foomatic-db-compressed-ppds printer-driver-foo2zjs printer-driver-splix \
 && rm -rf /var/lib/apt/lists/*
COPY --from=ghostscript-build /usr/local /usr/local
COPY --from=ghostscript-build /out/licenses /usr/share/licenses
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/printscan \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/printscan/source-packages.txt \
 && test -s /usr/share/printscan/source-packages.txt
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
EXPOSE 8004
CMD ["/usr/local/bin/ghostscript-printer-app", "-o", "server-port=8004", "server"]

# Corresponding source (decision 20): the union of the four targets' package
# lists, plus the upstream tarballs of everything built from source and the patch.
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS sources-fetch
ARG PAPPL_VERSION
ARG PAPPL_RETROFIT_VERSION
ARG PS_PRINTER_APP_COMMIT
ARG HPLIP_PRINTER_APP_COMMIT
ARG GUTENPRINT_PRINTER_APP_COMMIT
ARG GHOSTSCRIPT_PRINTER_APP_COMMIT
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources
COPY --from=ps /usr/share/printscan/source-packages.txt /sources/lists/ps.txt
COPY --from=hplip /usr/share/printscan/source-packages.txt /sources/lists/hplip.txt
COPY --from=gutenprint /usr/share/printscan/source-packages.txt /sources/lists/gutenprint.txt
COPY --from=ghostscript /usr/share/printscan/source-packages.txt /sources/lists/ghostscript.txt
WORKDIR /sources/ubuntu
# Lines are source package names (--only-source: never mapped through binary names).
# apt-get names any line it cannot resolve and fails.
RUN sort -u /sources/lists/*.txt > /sources/source-packages.txt \
 && chown _apt /sources/ubuntu \
 && apt-get update \
 && xargs apt-get source --only-source --download-only < /sources/source-packages.txt \
 && test "$(ls *.dsc | wc -l)" = "$(wc -l < /sources/source-packages.txt)"
ADD https://github.com/michaelrsweet/pappl/archive/refs/tags/${PAPPL_VERSION}.tar.gz /sources/upstream/pappl-${PAPPL_VERSION}.tar.gz
ADD https://github.com/OpenPrinting/pappl-retrofit/archive/refs/tags/${PAPPL_RETROFIT_VERSION}.tar.gz /sources/upstream/pappl-retrofit-${PAPPL_RETROFIT_VERSION}.tar.gz
ADD https://github.com/OpenPrinting/ps-printer-app/archive/${PS_PRINTER_APP_COMMIT}.tar.gz /sources/upstream/ps-printer-app-${PS_PRINTER_APP_COMMIT}.tar.gz
ADD https://github.com/OpenPrinting/hplip-printer-app/archive/${HPLIP_PRINTER_APP_COMMIT}.tar.gz /sources/upstream/hplip-printer-app-${HPLIP_PRINTER_APP_COMMIT}.tar.gz
ADD https://github.com/OpenPrinting/gutenprint-printer-app/archive/${GUTENPRINT_PRINTER_APP_COMMIT}.tar.gz /sources/upstream/gutenprint-printer-app-${GUTENPRINT_PRINTER_APP_COMMIT}.tar.gz
ADD https://github.com/OpenPrinting/ghostscript-printer-app/archive/${GHOSTSCRIPT_PRINTER_APP_COMMIT}.tar.gz /sources/upstream/ghostscript-printer-app-${GHOSTSCRIPT_PRINTER_APP_COMMIT}.tar.gz
COPY images/driver/patches/ /sources/patches/

FROM scratch AS sources
COPY --from=sources-fetch /sources /sources
