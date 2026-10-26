# Independent client OS profile for scripts/pppoe-peer.py, not a release image.
ARG ALPINE_VERSION=3.24.3
FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ppp-pppoe=2.5.3-r0
