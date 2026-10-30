# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24.3
ARG TARGETARCH
ARG RELEASE_ID=dev

FROM --platform=$BUILDPLATFORM routerd2-ndpi-modules:${RELEASE_ID}-${TARGETARCH} AS ndpi-artifacts
FROM routerd2-pppoe-engine:${ALPINE_VERSION}-${TARGETARCH} AS ppp-artifacts
FROM routerd2-dnsmasq-engine:${ALPINE_VERSION}-${TARGETARCH} AS dhcp-artifacts
FROM routerd2-conntrack-engine:${ALPINE_VERSION}-${TARGETARCH} AS conntrack-artifacts
FROM routerd2-tailscale-engine:${ALPINE_VERSION}-${TARGETARCH} AS tailscale-artifacts
FROM routerd2-chrony-engine:${ALPINE_VERSION}-${TARGETARCH} AS chrony-artifacts

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine3.24 AS toolchain

FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS build
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local CGO_ENABLED=0
RUN apk add --no-cache ca-certificates python3
WORKDIR /src
COPY go.mod go.sum ./
# The pinned local nftables patch is part of module resolution itself.
COPY third_party/google-nftables/ ./third_party/google-nftables/
RUN go mod download
COPY . .
COPY --from=ndpi-artifacts /ndpi-notices/ /inputs/ndpi-notices/
ARG TARGETOS=linux
ARG TARGETARCH
ARG RELEASE_ID=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:$RELEASE_ID:END" \
      -o /out/routerd ./cmd/routerd \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:$RELEASE_ID:END" \
      -o /out/routerd-logging ./cmd/routerd-logging \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH python3 scripts/licenses.py --output dist/licenses --native-notices /inputs/ndpi-notices
RUN python3 -c 'import json; b=json.load(open("images/boot.json")); b["platform"]="container"; b.pop("kernel_modules"); b["ssh"]={"user":"admin"}; json.dump(b,open("/out/boot.json","w"))'

FROM scratch AS runtime
LABEL org.opencontainers.image.title="routerd2" \
      org.opencontainers.image.source="https://github.com/imksoo/routerd2" \
      org.opencontainers.image.licenses="BSD-3-Clause AND BSD-4-Clause AND BSD-4-Clause-Shortened AND GPL-2.0-or-later AND LGPL-3.0-or-later AND LGPL-2.1-or-later AND LGPL-2.0-or-later AND Public-Domain AND RSA-MD AND Apache-2.0 AND MIT"
COPY --from=build /out/routerd /usr/sbin/routerd
COPY --from=ndpi-artifacts /routerd-modules /usr/libexec/routerd-modules
COPY --from=build /out/routerd-logging /usr/libexec/routerd-logging
COPY --from=ndpi-artifacts /ndpi-sources.tar /usr/share/doc/routerd2/ndpi-sources.tar
COPY --from=chrony-artifacts /chrony-engine.tar /usr/lib/routerd/chrony-engine.tar
COPY --from=chrony-artifacts /chrony-sources.tar /usr/share/doc/routerd2/chrony-sources.tar
COPY --from=conntrack-artifacts /conntrack-engine.tar /usr/lib/routerd/conntrack-engine.tar
COPY --from=conntrack-artifacts /conntrack-sources.tar /usr/share/doc/routerd2/conntrack-sources.tar
COPY --from=tailscale-artifacts /tailscale-engine.tar /usr/lib/routerd/tailscale-engine.tar
COPY --from=tailscale-artifacts /tailscale-sources.tar /usr/share/doc/routerd2/tailscale-sources.tar
COPY --from=dhcp-artifacts /dnsmasq-engine.tar /usr/lib/routerd/dnsmasq-engine.tar
COPY --from=dhcp-artifacts /dnsmasq-sources.tar /usr/share/doc/routerd2/dnsmasq-sources.tar
COPY --from=ppp-artifacts /pppoe-engine.tar /usr/lib/routerd/pppoe-engine.tar
COPY --from=ppp-artifacts /pppoe-sources.tar /usr/share/doc/routerd2/pppoe-sources.tar
COPY --from=build /out/boot.json /etc/routerd/boot.json
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY images/config-empty.json /etc/routerd/config.json
COPY LICENSE /usr/share/licenses/routerd2/LICENSE
COPY --from=build /src/dist/licenses/ /usr/share/licenses/routerd2/dependencies/
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/sbin/routerd"]
CMD ["boot", "--file", "/etc/routerd/boot.json"]
