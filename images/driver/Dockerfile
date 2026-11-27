# pagewire driver images: PAPPL retrofit Printer Applications built the way
# the upstream rock recipes (rockcraft.yaml) build them, without their
# avahi-daemon and dbus parts (the device pod's single Avahi comes from the
# device image), and with Ubuntu packages for CUPS, its backends, the filters
# and the drivers. Always build with an explicit target:
#   docker buildx build -f images/driver/Dockerfile --target ps .
#   (ps, hplip, gutenprint, ghostscript, legacy)
#   docker buildx build -f images/driver/Dockerfile --target sources -o type=local,dest=out .
# The driver-index target builds internal/catalog/drivers.tsv.gz from the five targets' driver lists:
#   docker buildx build -f images/driver/Dockerfile --target driver-index -o type=local,dest=.cache/idx .
# The sources target lists the packages of the five local targets by default. CI passes
# SOURCES_LISTS=pushed and PUSHED_IMAGE=<the image it pushed for this commit>, so the
# list is the union of that image's amd64 and arm64 package lists instead.

# Upstreams are built by commit; the Renovate comment keeps the release tag or branch.
ARG PAPPL_COMMIT=6db8e137557ad84662e78d24fdb2a591c621f4ac # renovate: datasource=github-tags depName=michaelrsweet/pappl currentValue=v1.4.12
ARG PAPPL_RETROFIT_COMMIT=9f0d34031e0a1c16acb58c2dffc1413bc8f6b497 # renovate: datasource=github-tags depName=OpenPrinting/pappl-retrofit currentValue=1.0b3
ARG PS_PRINTER_APP_COMMIT=5ce06c52f8a3a00420587f885e7eb4a238d586ee # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/ps-printer-app currentValue=master
ARG HPLIP_PRINTER_APP_COMMIT=90478d9edd063df2756e1a1045aaf1bc4012e3f5 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/hplip-printer-app currentValue=master
ARG GUTENPRINT_PRINTER_APP_COMMIT=fbfce9086b3e612447eec729240ab68986608857 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/gutenprint-printer-app currentValue=master
ARG GHOSTSCRIPT_PRINTER_APP_COMMIT=acf18a476bee525b88526ff8e0154f004b361914 # renovate: datasource=git-refs depName=https://github.com/OpenPrinting/ghostscript-printer-app currentValue=master
ARG VERSION=dev
ARG SOURCES_LISTS=local
# A sentinel: the pushed-* stages that read it are never built in local mode.
ARG PUSHED_IMAGE=scratch

# PAPPL and pappl-retrofit, both under /usr/local. The rock's PAPPL edits are in
# images/driver/patches/pappl-max-vendor.patch (PAPPL_MAX_VENDOR is still 32 in 1.4.12).
# images/driver/patches/pappl-web-admin-closed.patch is pagewire's own: it closes the
# web interface's admin pages and printer actions when no auth service is set.
# images/driver/patches/pappl-ipp-admin-local.patch is pagewire's own too: Disable-Printer,
# Enable-Printer and Cancel-Current-Job answer only local clients.
# images/driver/patches/pappl-retrofit-fd-leak.patch is upstream's fix for the
# descriptors 1.0b3 leaks on every device listing; drop it with the first tag that has it.
# images/driver/patches/pappl-retrofit-fork-gate.patch is pagewire's own: a fork gate in
# libpappl-retrofit, so the filter functions libcupsfilters and libppd run in a forked child
# never inherit a lock another thread holds (pappl-retrofit issue 35). pappl-base tests it with
# images/driver/fork-gate-test.c, and each final stage checks that its application uses it.
# images/driver/patches/pappl-retrofit-filter-reap.patch is pagewire's own too: each job's
# filter chain runs in a child process of its own, so two jobs printing at once no longer reap
# each other's filters and hang (pappl-retrofit issue 36). pappl-base tests it with
# images/driver/filter-chain-test.c.
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS pappl-base
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ARG PAPPL_COMMIT
ARG PAPPL_RETROFIT_COMMIT
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git build-essential \
      autoconf automake libtool autotools-dev autopoint gettext pkg-config perl-base \
      libcups2-dev libcupsfilters-dev libppd-dev libgnutls28-dev libavahi-client-dev \
      libjpeg-dev libpng-dev libusb-1.0-0-dev libpam0g-dev zlib1g-dev \
 && rm -rf /var/lib/apt/lists/*
COPY images/driver/patches/pappl-max-vendor.patch images/driver/patches/pappl-web-admin-closed.patch images/driver/patches/pappl-ipp-admin-local.patch images/driver/patches/pappl-retrofit-fd-leak.patch images/driver/patches/pappl-retrofit-fork-gate.patch images/driver/patches/pappl-retrofit-filter-reap.patch images/driver/fork-gate-test.c images/driver/filter-chain-test.c /src/
WORKDIR /src/pappl
# The installed patch is the notice of modification that Apache-2.0 4(b) asks for.
RUN git init -q . \
 && git fetch -q --depth 1 https://github.com/michaelrsweet/pappl.git "${PAPPL_COMMIT}" \
 && git checkout -q FETCH_HEAD \
 && mkdir -p /out/upstream \
 && git archive --prefix="pappl-${PAPPL_COMMIT}/" -o "/out/upstream/pappl-${PAPPL_COMMIT}.tar.gz" HEAD \
 && patch -p1 < /src/pappl-max-vendor.patch \
 && patch -p1 < /src/pappl-web-admin-closed.patch \
 && patch -p1 < /src/pappl-ipp-admin-local.patch \
 && ./configure --prefix=/usr/local --enable-libjpeg --enable-libpng --enable-libusb --with-dnssd=avahi \
 && make -j"$(nproc)" \
 && make install \
 && install -D -m 0644 -t /out/licenses/pappl LICENSE NOTICE /src/pappl-max-vendor.patch /src/pappl-web-admin-closed.patch /src/pappl-ipp-admin-local.patch
WORKDIR /src/pappl-retrofit
RUN git init -q . \
 && git fetch -q --depth 1 https://github.com/OpenPrinting/pappl-retrofit.git "${PAPPL_RETROFIT_COMMIT}" \
 && git checkout -q FETCH_HEAD \
 && git archive --prefix="pappl-retrofit-${PAPPL_RETROFIT_COMMIT}/" -o "/out/upstream/pappl-retrofit-${PAPPL_RETROFIT_COMMIT}.tar.gz" HEAD \
 && patch -p1 < /src/pappl-retrofit-fd-leak.patch \
 && patch -p1 < /src/pappl-retrofit-fork-gate.patch \
 && patch -p1 < /src/pappl-retrofit-filter-reap.patch \
 && ./autogen.sh \
 && ./configure --prefix=/usr/local \
 && make -j"$(nproc)" \
 && make install \
 && ldconfig \
 && install -D -m 0644 -t /out/licenses/pappl-retrofit LICENSE NOTICE COPYING /src/pappl-retrofit-fd-leak.patch /src/pappl-retrofit-fork-gate.patch /src/pappl-retrofit-filter-reap.patch \
 # pappl-retrofit runs CUPS backends from "/usr/lib/<app>/backend" (a
 # symlink to CUPS's serverbin that the apps' "make install" makes only for
 # prefix /usr), not from CUPS's /usr/lib/cups/backend. The targets below
 # set BACKEND_DIR and FILTER_DIR, which pappl-retrofit reads at start-up.
 && grep -q '"/usr/lib/%s/backend"' pappl-retrofit/pappl-retrofit.c \
 && grep -q 'getenv("BACKEND_DIR")' pappl-retrofit/pappl-retrofit.c \
 # pagewire's Find-Devices asks for type other-local, which lists the "cups"
 # scheme only because pappl-retrofit registers it for every device type.
 && grep -q 'papplDeviceAddScheme("cups", PAPPL_DEVTYPE_ALL' pappl-retrofit/pappl-retrofit.c \
 # The legacy target ships legacy-printer-app, which build-app.sh deletes from the others.
 && grep -q 'PR_COPTIONS_NO_PAPPL_BACKENDS' legacy/legacy-printer-app.c \
 && grep -q 'PR_COPTIONS_CUPS_BACKENDS' legacy/legacy-printer-app.c

# The fork gate's self-test (images/driver/fork-gate-test.c), built against the installed library
# and without it: every hang case must hang without the gate, or it tests nothing.
# Exit 2: a child hung; 3: a child exited 70, the gate's "would wait forever" exit.
# The syslog case checks that the syslog wrappers pass the arguments on.
RUN cc -O2 -Wall -Wextra -Werror -o /tmp/fork-gate-test /src/fork-gate-test.c -Wl,--no-as-needed -L/usr/local/lib -lpappl-retrofit \
 && cc -O2 -Wall -Wextra -Werror -o /tmp/fork-gate-test-ungated /src/fork-gate-test.c \
 && for c in wake wake-rw condwait owncond cancel tz timegm; do \
      /tmp/fork-gate-test "$c" 2>/tmp/fork-gate.err && ! test -s /tmp/fork-gate.err \
        || { echo "fork gate: case $c failed" >&2; cat /tmp/fork-gate.err >&2; exit 1; }; \
    done \
 && out=$(/tmp/fork-gate-test syslog 2>&1 >/dev/null) && test "$out" = "fork-gate-test: syslog 42" \
        || { echo "fork gate: case syslog printed: $out" >&2; exit 1; } \
 && for c in held held-rw; do \
      rc=0; /tmp/fork-gate-test "$c" 2>/tmp/fork-gate.err || rc=$?; \
      { test "$rc" -eq 3 && grep -q 'forking anyway' /tmp/fork-gate.err && grep -q 'would wait forever' /tmp/fork-gate.err; } \
        || { echo "fork gate: failsafe case $c exited $rc" >&2; cat /tmp/fork-gate.err >&2; exit 1; }; \
    done \
 && for c in wake wake-rw held held-rw; do \
      rc=0; /tmp/fork-gate-test-ungated "$c" >/dev/null || rc=$?; \
      test "$rc" -eq 2 || { echo "fork gate: case $c did not hang without the gate (exit $rc)" >&2; exit 1; }; \
    done \
 && rm -f /tmp/fork-gate-test /tmp/fork-gate-test-ungated /tmp/fork-gate.err

# The filter chain's self-test (images/driver/filter-chain-test.c), built against the patched
# library's _prFilterChain() and against cfFilterChain() as 1.0b3 called it from the job thread:
# steal and reap must fail without the patch, or they test nothing (both and orphan fail only
# sometimes without it, and pipefail passes: the unpatched chain returns without waiting).
# Exit 2: a chain hung; 3: a child reaped by the wrong waiter.
RUN cc -O2 -Wall -Wextra -Werror -DRUN_CHAIN=_prFilterChain -o /tmp/filter-chain-test /src/filter-chain-test.c -L/usr/local/lib -lpappl-retrofit -lcupsfilters -lcups \
 && cc -O2 -Wall -Wextra -Werror -DRUN_CHAIN=cfFilterChain -o /tmp/filter-chain-test-unpatched /src/filter-chain-test.c -lcupsfilters -lcups \
 && for c in steal reap both orphan pipefail; do \
      /tmp/filter-chain-test "$c" 2>/tmp/filter-chain.err && ! test -s /tmp/filter-chain.err \
        || { echo "filter chain: case $c failed" >&2; cat /tmp/filter-chain.err >&2; exit 1; }; \
    done \
 && for c in steal:2 reap:3; do \
      rc=0; /tmp/filter-chain-test-unpatched "${c%:*}" 2>/dev/null || rc=$?; \
      test "$rc" -eq "${c#*:}" || { echo "filter chain: case ${c%:*} exited $rc without the patch, want ${c#*:}" >&2; exit 1; }; \
    done \
 && rm -f /tmp/filter-chain-test /tmp/filter-chain-test-unpatched /tmp/filter-chain.err

# Each retrofit app at its pinned commit, built as the rock's app part builds it
# ("make LDFLAGS=-ljpeg", then "make install"), under /usr/local.
# Every app must use pappl-retrofit's CUPS backends only, never PAPPL's own.
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
mkdir -p /out/upstream
git archive --prefix="${app}-$2/" -o "/out/upstream/${app}-$2.tar.gz" HEAD
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

# Run by each final stage: the fork gate works only while every library resolves the calls it
# wraps to libpappl-retrofit, which must come before libc in the application's load order.
COPY --chmod=0755 <<'SCRIPT' /src/check-fork-gate.sh
#!/bin/sh
# usage: check-fork-gate.sh <application>
set -eu
gated='pthread_mutex_(lock|trylock|timedlock|clocklock|unlock)|pthread_rwlock_(rdlock|wrlock|tryrdlock|trywrlock|timedrdlock|timedwrlock|clockrdlock|clockwrlock|unlock)|pthread_cond_(wait|timedwait|clockwait)|pthread_create|gmtime_r|localtime_r|gmtime|localtime|mktime|timegm|timelocal|ctime|ctime_r|strptime|strftime|tzset|__gmtime_r|strftime_l|__strftime_l|wcsftime|wcsftime_l|__wcsftime_l|syslog|vsyslog|__syslog_chk|__vsyslog_chk|openlog|closelog|setlogmask'
b=$(LD_BIND_NOW=1 LD_DEBUG=bindings "$1" --version 2>&1 || true)
# Where the hang was traced: libcups' string pool mutex and PAPPL's rwlocks.
printf '%s\n' "$b" | grep -q "binding file [^ ]*/libcups\.so\.2 .* to /usr/local/lib/libpappl-retrofit\.so[^ ]* .*\`pthread_mutex_lock'" \
  || { echo "$1: libcups' pthread_mutex_lock does not resolve to the fork gate" >&2; exit 1; }
printf '%s\n' "$b" | grep -q "binding file /usr/local/lib/libpappl\.so[^ ]* .* to /usr/local/lib/libpappl-retrofit\.so[^ ]* .*\`pthread_rwlock_rdlock'" \
  || { echo "$1: libpappl's pthread_rwlock_rdlock does not resolve to the fork gate" >&2; exit 1; }
# No library resolves a gated call past the gate to libc.
if printf '%s\n' "$b" | grep -E " to [^ ]*/libc\.so\.6 .*\`($gated)'" | grep -v 'binding file /usr/local/lib/libpappl-retrofit\.so'; then
  echo "$1: the calls above bypass the fork gate" >&2
  exit 1
fi
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

# pappl-retrofit's own legacy-printer-app (classic CUPS drivers), already built and
# installed by pappl-base. Drop the same build-only files and serverbin as build-app.sh.
FROM pappl-base AS legacy-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
RUN cd /usr/local \
 && rm -rf include lib/pkgconfig lib/*.a lib/*.la bin/pappl-makeresheader lib/legacy-printer-app \
 && test -x bin/legacy-printer-app && test -s share/legacy-printer-app/testpage.pdf

# Shared runtime: CUPS (for its usb, socket and ipp backends; cupsd is never
# started) and the libraries PAPPL and pappl-retrofit link against.
FROM ubuntu:26.04@sha256:d66b366dac61b575bee2aef07e9c26e8070096fbe84a3d4d3eb0da069b5d76db AS runtime
ARG DEBIAN_FRONTEND=noninteractive
# The UTC times before and after each apt run in an image name the Ubuntu snapshots
# (snapshot.ubuntu.com) that hold its package versions, for sources-fetch.
# shortcut: a mirror lagging the publisher can make both times name a newer state; record each suite's InRelease Date if sources-fetch ever names such a line.
RUN mkdir -p /usr/share/pagewire \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends ca-certificates cups \
      libcups2t64 libcupsfilters2t64 libppd2 libavahi-client3 libgnutls30t64 \
      libjpeg8 libpng16-16t64 libusb-1.0-0 libpam0g zlib1g \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && useradd --uid 10001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin pagewire
# NO_DRIVERLESS_PPDS: no driverless (ippfind) drivers; IPP printers are ipp-usb's job.
ENV BACKEND_DIR=/usr/lib/cups/backend \
    FILTER_DIR=/usr/lib/cups/filter \
    NO_DRIVERLESS_PPDS=1

FROM runtime AS ps
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=ps-printer-app
RUN date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get install -y --no-install-recommends foomatic-db-compressed-ppds ghostscript cups-filters-core-drivers printer-driver-min12xxw \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt
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
 && mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
RUN --mount=type=bind,from=pappl-base,source=/src/check-fork-gate.sh,target=/tmp/check-fork-gate.sh sh /tmp/check-fork-gate.sh "/usr/local/bin/${APP}"
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
# The driver list for the index, after the ENV line so it sees the container's
# PPD_DIRS, BACKEND_DIR and NO_DRIVERLESS_PPDS.
RUN "/usr/local/bin/${APP}" drivers > /usr/share/pagewire/drivers.txt && test -s /usr/share/pagewire/drivers.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-driver-ps
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
EXPOSE 8001
CMD ["/usr/local/bin/ps-printer-app", "-o", "server-port=8001", "server"]

FROM runtime AS hplip
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=hplip-printer-app
RUN date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get install -y --no-install-recommends 'hplip=3.24.4*' printer-driver-hpcups hplip-data libcurl4t64 libssl3t64 \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt
COPY --from=hplip-build /usr/local /usr/local
COPY --from=hplip-build /out/licenses /usr/share/licenses
# HP plug-in paths point into the blob PVC, where the plug-in is installed after
# the user accepts HP's licence; the image never contains it. mkdir the parents,
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
 && mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
RUN --mount=type=bind,from=pappl-base,source=/src/check-fork-gate.sh,target=/tmp/check-fork-gate.sh sh /tmp/check-fork-gate.sh "/usr/local/bin/${APP}"
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
# The driver list for the index, after the ENV line so it sees the container's
# PPD_DIRS, BACKEND_DIR and NO_DRIVERLESS_PPDS.
RUN "/usr/local/bin/${APP}" drivers > /usr/share/pagewire/drivers.txt && test -s /usr/share/pagewire/drivers.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-driver-hplip
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
EXPOSE 8002
CMD ["/usr/local/bin/hplip-printer-app", "-o", "server-port=8002", "server"]

FROM runtime AS gutenprint
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=gutenprint-printer-app
RUN date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get install -y --no-install-recommends printer-driver-gutenprint libgutenprint9 \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt
COPY --from=gutenprint-build /usr/local /usr/local
COPY --from=gutenprint-build /out/licenses /usr/share/licenses
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
RUN --mount=type=bind,from=pappl-base,source=/src/check-fork-gate.sh,target=/tmp/check-fork-gate.sh sh /tmp/check-fork-gate.sh "/usr/local/bin/${APP}"
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
# The driver list for the index, after the ENV line so it sees the container's
# PPD_DIRS, BACKEND_DIR and NO_DRIVERLESS_PPDS.
RUN "/usr/local/bin/${APP}" drivers > /usr/share/pagewire/drivers.txt && test -s /usr/share/pagewire/drivers.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-driver-gutenprint
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
EXPOSE 8003
CMD ["/usr/local/bin/gutenprint-printer-app", "-o", "server-port=8003", "server"]

FROM runtime AS ghostscript
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=ghostscript-printer-app
RUN date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get install -y --no-install-recommends ghostscript foomatic-db-compressed-ppds printer-driver-foo2zjs printer-driver-min12xxw \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt
COPY --from=ghostscript-build /usr/local /usr/local
COPY --from=ghostscript-build /out/licenses /usr/share/licenses
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/pagewire \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
RUN --mount=type=bind,from=pappl-base,source=/src/check-fork-gate.sh,target=/tmp/check-fork-gate.sh sh /tmp/check-fork-gate.sh "/usr/local/bin/${APP}"
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/local/share/ppd:/usr/share/ppd:/usr/share/cups/model:/usr/lib/cups/driver:/usr/share/cups/drv:/var/lib/${APP}/ppd
# The driver list for the index, after the ENV line so it sees the container's
# PPD_DIRS, BACKEND_DIR and NO_DRIVERLESS_PPDS.
RUN "/usr/local/bin/${APP}" drivers > /usr/share/pagewire/drivers.txt && test -s /usr/share/pagewire/drivers.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-driver-ghostscript
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
EXPOSE 8004
CMD ["/usr/local/bin/ghostscript-printer-app", "-o", "server-port=8004", "server"]

FROM runtime AS legacy
ARG DEBIAN_FRONTEND=noninteractive
ARG APP=legacy-printer-app
# Free classic CUPS drivers. Ghostscript for pappl-retrofit's PDF and PostScript conversions.
ARG LEGACY_DRIVERS="printer-driver-splix printer-driver-brlaser printer-driver-ptouch printer-driver-c2esp printer-driver-sag-gdi printer-driver-escpr"
RUN date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt \
 && apt-get update \
 && apt-get install -y --no-install-recommends ${LEGACY_DRIVERS} ghostscript \
 && rm -rf /var/lib/apt/lists/* \
 && date -u +%Y%m%dT%H%M%SZ >> /usr/share/pagewire/apt-snapshots.txt
COPY --from=legacy-build /usr/local /usr/local
COPY --from=legacy-build /out/licenses /usr/share/licenses
# The app lists only /usr/share/pagewire/legacy-drivers: links to exactly these packages'
# .drv and PPD files and driver executables, not cups-filters' generic sample drivers.
RUN ldconfig \
 && mkdir -p "/var/lib/${APP}/ppd" "/var/spool/${APP}" \
 && test -x /usr/lib/cups/backend/usb && test -x /usr/lib/cups/backend/socket && test -x /usr/lib/cups/backend/ipp \
 && if ldd "/usr/local/bin/${APP}" | grep 'not found'; then exit 1; fi \
 && mkdir -p /usr/share/pagewire/legacy-drivers \
 && for p in ${LEGACY_DRIVERS}; do \
      n=0; \
      for f in $(dpkg -L "$p" | grep -E '^/usr/share/cups/drv/[^/]+\.drv$|^/usr/share/(ppd|cups/model)/.+\.ppd(\.gz)?$|^/usr/lib/cups/driver/[^/]+$'); do \
        if [ -f "$f" ]; then ln -s "$f" /usr/share/pagewire/legacy-drivers/ || exit 1; n=$((n + 1)); fi; \
      done; \
      if [ "$n" -eq 0 ]; then echo "$p ships no driver files" >&2; exit 1; fi; \
    done \
 && dpkg-query -W -f '${source:Package}=${source:Version}\n' | sort -u > /usr/share/pagewire/source-packages.txt \
 && test -s /usr/share/pagewire/source-packages.txt
RUN --mount=type=bind,from=pappl-base,source=/src/check-fork-gate.sh,target=/tmp/check-fork-gate.sh sh /tmp/check-fork-gate.sh "/usr/local/bin/${APP}"
# legacy-drivers comes first: pappl-retrofit marks every driver in the last directory "user-added".
ENV TESTPAGE_DIR=/usr/local/share/${APP} \
    PPD_DIRS=/usr/share/pagewire/legacy-drivers:/var/lib/${APP}/ppd
# The driver list for the index, after the ENV line so it sees the container's
# PPD_DIRS, BACKEND_DIR and NO_DRIVERLESS_PPDS.
RUN "/usr/local/bin/${APP}" drivers > /usr/share/pagewire/drivers.txt && test -s /usr/share/pagewire/drivers.txt
COPY NOTICE THIRD_PARTY_NOTICES.md /usr/share/pagewire/
# Where this image's source is: the tag CI's sources job publishes (v1.2.3 -> 1.2.3-sources,
# main -> main-sources). Declared late, so a new VERSION rebuilds only this layer.
ARG VERSION
ARG IMAGE=pagewire-driver-legacy
ARG SOURCES_REF=ghcr.io/thephaseless/${IMAGE}:${VERSION#v}-sources
RUN printf '%s\n' "${SOURCES_REF}" > /usr/share/pagewire/SOURCES
LABEL io.github.thephaseless.pagewire.sources="${SOURCES_REF}"
EXPOSE 8005
CMD ["/usr/local/bin/legacy-printer-app", "-o", "server-port=8005", "server"]

# The driver index (internal/catalog/drivers.tsv.gz) from the five targets' driver lists.
FROM golang:1.27.1-trixie@sha256:8f58fd67ea075142d947a60e0caa4317746a55118d312f027793d382c7741734 AS driver-index-build
WORKDIR /src
COPY go.mod go.sum ./
COPY tools/driverindex/ tools/driverindex/
COPY --from=ps /usr/share/pagewire/drivers.txt /in/ps.txt
COPY --from=hplip /usr/share/pagewire/drivers.txt /in/hplip.txt
COPY --from=gutenprint /usr/share/pagewire/drivers.txt /in/gutenprint.txt
COPY --from=ghostscript /usr/share/pagewire/drivers.txt /in/ghostscript.txt
COPY --from=legacy /usr/share/pagewire/drivers.txt /in/legacy.txt
RUN mkdir -p /out \
 && GOTOOLCHAIN=local CGO_ENABLED=0 go run ./tools/driverindex -o /out/drivers.tsv.gz \
      -in ps=/in/ps.txt -in hplip=/in/hplip.txt -in gutenprint=/in/gutenprint.txt \
      -in ghostscript=/in/ghostscript.txt -in legacy=/in/legacy.txt

FROM scratch AS driver-index
COPY --from=driver-index-build /out/ /

# Package lists for the sources target, chosen by SOURCES_LISTS.
FROM scratch AS lists-local
COPY --from=ps /usr/share/pagewire/source-packages.txt /lists/ps.txt
COPY --from=hplip /usr/share/pagewire/source-packages.txt /lists/hplip.txt
COPY --from=gutenprint /usr/share/pagewire/source-packages.txt /lists/gutenprint.txt
COPY --from=ghostscript /usr/share/pagewire/source-packages.txt /lists/ghostscript.txt
COPY --from=legacy /usr/share/pagewire/source-packages.txt /lists/legacy.txt
COPY --from=ps /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/ps.txt
COPY --from=hplip /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/hplip.txt
COPY --from=gutenprint /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/gutenprint.txt
COPY --from=ghostscript /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/ghostscript.txt
COPY --from=legacy /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/legacy.txt

# The pushed stages only COPY, so the arm64 one needs no emulation.
FROM --platform=linux/amd64 ${PUSHED_IMAGE} AS pushed-amd64
FROM --platform=linux/arm64 ${PUSHED_IMAGE} AS pushed-arm64

FROM scratch AS lists-pushed
COPY --from=pushed-amd64 /usr/share/pagewire/source-packages.txt /lists/amd64.txt
COPY --from=pushed-arm64 /usr/share/pagewire/source-packages.txt /lists/arm64.txt
COPY --from=pushed-amd64 /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/amd64.txt
COPY --from=pushed-arm64 /usr/share/pagewire/apt-snapshots.txt /lists/snapshots/arm64.txt

FROM lists-${SOURCES_LISTS} AS lists

# Corresponding source: the union of the package lists, plus git archives of the
# pinned commits of everything built from source, the patches and the build files.
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
COPY --from=pappl-base /out/upstream/ /sources/upstream/
COPY --from=ps-build /out/upstream/ /sources/upstream/
COPY --from=hplip-build /out/upstream/ /sources/upstream/
COPY --from=gutenprint-build /out/upstream/ /sources/upstream/
COPY --from=ghostscript-build /out/upstream/ /sources/upstream/
COPY images/driver/patches/ /sources/patches/
COPY images/driver/Dockerfile images/apt-source-fetch.sh /sources/build/

FROM scratch AS sources
COPY --from=sources-fetch /sources /sources
