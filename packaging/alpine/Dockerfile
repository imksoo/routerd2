# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24.3
ARG TARGETARCH

FROM routerd2-pppoe-engine:${ALPINE_VERSION}-${TARGETARCH} AS ppp-artifacts
FROM routerd2-dnsmasq-engine:${ALPINE_VERSION}-${TARGETARCH} AS dhcp-artifacts
FROM routerd2-conntrack-engine:${ALPINE_VERSION}-${TARGETARCH} AS conntrack-artifacts
FROM routerd2-tailscale-engine:${ALPINE_VERSION}-${TARGETARCH} AS tailscale-artifacts
FROM routerd2-chrony-engine:${ALPINE_VERSION}-${TARGETARCH} AS chrony-artifacts

FROM golang:${GO_VERSION}-alpine3.24 AS toolchain

FROM alpine:${ALPINE_VERSION} AS package-build
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local
RUN apk add --no-cache alpine-sdk ca-certificates tar python3 \
 && adduser -D -G abuild builder \
 && mkdir -p /out \
 && chown builder:abuild /out
COPY --from=chrony-artifacts /chrony-engine.tar /inputs/chrony-engine.tar
COPY --from=chrony-artifacts /chrony-sources.tar /out/chrony-sources.tar
COPY --from=conntrack-artifacts /conntrack-engine.tar /inputs/conntrack-engine.tar
COPY --from=conntrack-artifacts /conntrack-sources.tar /out/conntrack-sources.tar
COPY --from=tailscale-artifacts /tailscale-engine.tar /inputs/tailscale-engine.tar
COPY --from=tailscale-artifacts /tailscale-sources.tar /out/tailscale-sources.tar
COPY --from=dhcp-artifacts /dnsmasq-engine.tar /inputs/dnsmasq-engine.tar
COPY --from=dhcp-artifacts /dnsmasq-sources.tar /out/dnsmasq-sources.tar
COPY --from=ppp-artifacts /pppoe-engine.tar /inputs/pppoe-engine.tar
COPY --from=ppp-artifacts /pppoe-sources.tar /out/pppoe-sources.tar
WORKDIR /src
COPY . .
ARG RELEASE_ID=dev
ARG SOURCE_DATE_EPOCH=0
ENV ROUTERD_RELEASE_ID=$RELEASE_ID SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH
USER builder
RUN --mount=type=cache,target=/home/builder/go/pkg/mod,uid=1000 \
    --mount=type=cache,target=/home/builder/.cache/go-build,uid=1000 \
    GOMAXPROCS=2 sh packaging/alpine/build.sh /src /out

FROM scratch AS apk-output
COPY --from=package-build /out/ /

FROM alpine:${ALPINE_VERSION} AS image-build
RUN apk add --no-cache linux-lts kmod python3 gzip xz zstd ca-certificates
COPY --from=package-build /out/ /packages/
COPY images/ /src/images/
COPY scripts/build-image.sh /src/scripts/build-image.sh
ARG RELEASE_ID=dev
ARG SOURCE_DATE_EPOCH=0
ARG IMAGE_CONFIG=empty
ARG SSH_AUTHORIZED_KEY=""
ENV ROUTERD_RELEASE_ID=$RELEASE_ID SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH SSH_AUTHORIZED_KEY=$SSH_AUTHORIZED_KEY
RUN case "$IMAGE_CONFIG" in empty|qemu) ;; *) exit 2 ;; esac \
 && sh /src/scripts/build-image.sh --package-dir /packages --output /out \
      --config "/src/images/config-$IMAGE_CONFIG.json" --boot /src/images/boot.json

FROM scratch AS appliance-output
COPY --from=image-build /out/ /
