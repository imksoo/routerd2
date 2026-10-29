# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.24.3
ARG GO_VERSION=1.27.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine3.24 AS hook-build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/routerd-dhcp-event/ ./cmd/routerd-dhcp-event/
COPY internal/protocol/dhcpevent/ ./internal/protocol/dhcpevent/
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOMAXPROCS=2 GOTOOLCHAIN=local go build -trimpath -buildvcs=false -ldflags '-s -w' -o /out/routerd-dhcp-event ./cmd/routerd-dhcp-event \
 && go version > /out/toolchain.txt

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
COPY --from=hook-build /out/routerd-dhcp-event /engine-root/usr/sbin/routerd-dhcp-event
COPY --from=hook-build /out/toolchain.txt /build/hook/toolchain.txt
COPY --from=hook-build /usr/local/go/LICENSE /build/hook/Go-LICENSE
COPY --from=hook-build /usr/local/go/PATENTS /build/hook/Go-PATENTS
COPY cmd/routerd-dhcp-event/main.go /build/hook/main.go
COPY internal/protocol/dhcpevent/event.go /build/hook/event.go
COPY go.mod go.sum LICENSE /build/hook/
COPY scripts/engine_sources.py scripts/dnsmasq-image.py /build/
COPY packaging/alpine/dnsmasq-engine.Dockerfile /build/
RUN python3 /build/dnsmasq-image.py --root /engine-root --arch "$TARGETARCH"

FROM scratch AS artifacts
COPY --from=build /out/ /
