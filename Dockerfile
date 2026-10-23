# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24.3

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
ARG TARGETOS=linux
ARG TARGETARCH
ARG RELEASE_ID=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.ReleaseID=$RELEASE_ID" \
      -o /out/routerd ./cmd/routerd \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.ReleaseID=$RELEASE_ID" \
      -o /out/routerd-modules ./cmd/routerd-modules \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH python3 scripts/licenses.py --output dist/licenses

FROM scratch AS runtime
LABEL org.opencontainers.image.title="routerd2" \
      org.opencontainers.image.source="https://github.com/imksoo/routerd2" \
      org.opencontainers.image.licenses="BSD-3-Clause"
COPY --from=build /out/routerd /usr/sbin/routerd
COPY --from=build /out/routerd-modules /usr/libexec/routerd-modules
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY images/config-empty.json /etc/routerd/config.json
COPY LICENSE /usr/share/licenses/routerd2/LICENSE
COPY --from=build /src/dist/licenses/ /usr/share/licenses/routerd2/dependencies/
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/sbin/routerd"]
CMD ["run", "--platform", "container", "--config", "/etc/routerd/config.json", "--module-path", "/usr/libexec/routerd-modules", "--runtime-dir", "/run/routerd", "--state-dir", "/var/lib/routerd"]
