# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.24.3
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS build
RUN apk add --no-cache python3 ca-certificates
ARG TARGETARCH
RUN case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) exit 2 ;; esac \
 && mkdir -p /engine-root/etc/apk \
 && cp -a /etc/apk/keys /engine-root/etc/apk/keys \
 && cp /usr/share/apk/keys/"$arch"/*.pub /engine-root/etc/apk/keys/ \
 && cp /etc/apk/repositories /engine-root/etc/apk/repositories \
 && apk --root /engine-root --arch "$arch" --initdb --no-scripts --no-cache add \
      dnsmasq=2.92_p2-r0 musl=1.2.6-r3
COPY scripts/engine_sources.py scripts/dnsmasq-image.py /build/
COPY packaging/alpine/dnsmasq-engine.Dockerfile /build/
RUN python3 /build/dnsmasq-image.py --root /engine-root --arch "$TARGETARCH"

FROM scratch AS artifacts
COPY --from=build /out/ /
