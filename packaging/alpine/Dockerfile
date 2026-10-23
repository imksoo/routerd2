# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24.3

FROM golang:${GO_VERSION}-alpine3.24 AS toolchain

FROM alpine:${ALPINE_VERSION} AS package-build
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local
RUN apk add --no-cache alpine-sdk ca-certificates tar \
 && adduser -D -G abuild builder \
 && mkdir -p /out \
 && chown builder:abuild /out
WORKDIR /src
COPY . .
ARG RELEASE_ID=dev
ARG SOURCE_DATE_EPOCH=0
ENV ROUTERD_RELEASE_ID=$RELEASE_ID SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH
USER builder
RUN sh packaging/alpine/build.sh /src /out

FROM scratch AS apk-output
COPY --from=package-build /out/ /

FROM alpine:${ALPINE_VERSION} AS image-build
RUN apk add --no-cache linux-virt kmod python3 gzip xz zstd ca-certificates
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
