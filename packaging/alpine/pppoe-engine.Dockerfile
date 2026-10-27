# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.24.3
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS build
RUN apk add --no-cache python3 ca-certificates
ARG TARGETARCH
# Only build tools execute here. Foreign packages are installed without scripts.
RUN case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) exit 2 ;; esac \
 && mkdir -p /engine-root/etc/apk \
 && cp -a /etc/apk/keys /engine-root/etc/apk/keys \
 && cp /usr/share/apk/keys/"$arch"/*.pub /engine-root/etc/apk/keys/ \
 && cp /etc/apk/repositories /engine-root/etc/apk/repositories \
 && apk --root /engine-root --arch "$arch" --initdb --no-scripts --no-cache add \
      ppp-pppoe=2.5.3-r0 musl=1.2.6-r3 libpcap=1.10.7-r0 \
      libssl3=3.5.9-r0 libcrypto3=3.5.9-r0
COPY scripts/engine_sources.py /build/engine_sources.py
COPY scripts/pppoe-image.py /build/pppoe-image.py
RUN python3 /build/pppoe-image.py --root /engine-root --arch "$TARGETARCH" \
      --cache /sources --output /out/pppoe-engine.tar --sources-output /out/pppoe-sources.tar

FROM scratch AS artifacts
COPY --from=build /out/ /
