# Control-plane image: a static Go binary with the web UI embedded, on distroless static.
# Every external FROM is pinned as tag@sha256 (multi-arch index digest); Renovate moves both.

FROM oven/bun:1.4.5-alpine@sha256:da816670775f4c2b4e990b6b1847cf5540dcdc721a42033dd331ce7a88ba6a3b AS ui
ARG BUILDKIT_SBOM_SCAN_STAGE=true
WORKDIR /src/ui
COPY ui/package.json ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY ui/ ./
RUN bun run build

FROM golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS go
ARG BUILDKIT_SBOM_SCAN_STAGE=true
RUN apk add --no-cache build-base pkgconf libjpeg-turbo-dev libjpeg-turbo-static libjpeg-turbo-doc libpng-dev libpng-static zlib-static
# cmd/go uses only the first word of $PKG_CONFIG, so --static goes through a wrapper
# (it adds libpng's private -lz and -lm to the static link).
RUN printf '#!/bin/sh\nexec pkgconf --static "$@"\n' > /usr/local/bin/pkg-config-static && chmod +x /usr/local/bin/pkg-config-static
ENV PKG_CONFIG=/usr/local/bin/pkg-config-static
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=ui /src/ui/dist ./ui/dist
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -linkmode external -extldflags -static -X main.version=${VERSION}" -o /out/pagewire ./cmd/pagewire \
 && go tool go-licenses save ./cmd/pagewire --save_path /out/licenses/go \
 # go-licenses skips the standard library, which is linked into the binary too.
 && cp "$(go env GOROOT)/LICENSE" /out/licenses/go/LICENSE-go-stdlib
# Licence texts of the statically linked C libraries. Alpine ships no licence file for musl,
# libpng or zlib, so libpng's and zlib's come from their headers. musl's COPYRIGHT is committed
# (extracted from the checksummed musl-1.2.6 release tarball), so the build does not depend on
# musl.libc.org being reachable.
RUN mkdir -p /out/licenses/c/musl /out/licenses/c/libjpeg-turbo /out/licenses/c/libpng /out/licenses/c/zlib /out/data \
 && cp images/licenses/musl-1.2.6-COPYRIGHT /out/licenses/c/musl/COPYRIGHT \
 && cp /usr/share/doc/libjpeg-turbo/LICENSE.md /usr/share/doc/libjpeg-turbo/README.ijg /out/licenses/c/libjpeg-turbo/ \
 && awk '/COPYRIGHT NOTICE, DISCLAIMER, and LICENSE/,/\*\//' /usr/include/libpng16/png.h > /out/licenses/c/libpng/LICENSE \
 && sed -n '1,/\*\//p' /usr/include/zlib.h > /out/licenses/c/zlib/LICENSE \
 && for f in musl/COPYRIGHT libjpeg-turbo/LICENSE.md libjpeg-turbo/README.ijg libpng/LICENSE zlib/LICENSE; do test -s "/out/licenses/c/$f" || { echo "empty licence file $f" >&2; exit 1; }; done

FROM gcr.io/distroless/static-debian13:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=go /out/pagewire /pagewire
COPY --from=go /out/licenses /licenses
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /licenses/pagewire/
COPY --from=go --chown=65532:65532 /out/data /data
USER 65532:65532
EXPOSE 8080 8081
HEALTHCHECK CMD ["/pagewire", "healthcheck"]
ENTRYPOINT ["/pagewire"]
CMD ["serve"]
