# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine3.24 AS build
RUN apk add --no-cache python3 ca-certificates
WORKDIR /build
COPY scripts/tailscale-image.py scripts/engine_sources.py scripts/licenses.py ./
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOMAXPROCS=2 python3 tailscale-image.py --arch "$TARGETARCH" --output /out

FROM scratch AS artifacts
COPY --from=build /out/ /
