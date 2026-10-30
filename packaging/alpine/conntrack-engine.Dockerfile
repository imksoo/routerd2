# syntax=docker/dockerfile:1
ARG ALPINE_VERSION=3.24.3
FROM alpine:${ALPINE_VERSION} AS build
RUN apk add --no-cache python3 binutils xz ca-certificates alpine-sdk bison flex linux-headers libmnl-dev libnfnetlink-dev libnetfilter_conntrack-dev libnetfilter_cthelper-dev libnetfilter_cttimeout-dev libnetfilter_queue-dev libtirpc-dev
ARG TARGETARCH
RUN case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) exit 2 ;; esac \
 && mkdir -p /engine-root/etc/apk \
 && cp -a /etc/apk/keys /engine-root/etc/apk/keys \
 && cp /usr/share/apk/keys/"$arch"/*.pub /engine-root/etc/apk/keys/ \
 && cp /etc/apk/repositories /engine-root/etc/apk/repositories \
 && apk --root /engine-root --arch "$arch" --initdb --no-scripts --no-cache add \
      conntrack-tools=1.4.9-r0 musl=1.2.6-r3 libmnl=1.0.5-r2 \
      libnfnetlink=1.0.2-r3 libnetfilter_conntrack=1.1.1-r0 \
      libnetfilter_cthelper=1.0.1-r2 libnetfilter_cttimeout=1.0.1-r2 \
      libnetfilter_queue=1.0.5-r2
COPY scripts/engine_sources.py scripts/conntrack-image.py /build/
COPY packaging/alpine/conntrack-commit-result.patch packaging/alpine/conntrack-copy-event.patch packaging/alpine/conntrack-engine.Dockerfile /build/
RUN python3 /build/conntrack-image.py --root /engine-root --arch "$TARGETARCH" \
 && mkdir /src \
 && tar -xf /out/conntrack-sources.tar -C /src \
 && tar -xf /src/libnetfilter_conntrack/libnetfilter_conntrack-1.1.1.tar.xz -C /src \
 && cd /src/libnetfilter_conntrack-1.1.1 \
 && patch -p1 < /build/conntrack-copy-event.patch \
 && ./configure --prefix=/usr \
 && make -j2 \
 && make install DESTDIR=/engine-root \
 && tar -xf /src/conntrack-tools/conntrack-tools-1.4.9.tar.xz -C /src \
 && cd /src/conntrack-tools-1.4.9 \
 && patch -p1 < /build/conntrack-commit-result.patch \
 && CFLAGS="-O2 -D_GNU_SOURCE $(pkgconf --cflags libtirpc)" LDFLAGS="$(pkgconf --libs libtirpc)" ./configure --prefix=/usr \
 && make -j2 \
 && cp src/conntrackd /engine-root/usr/sbin/conntrackd \
 && python3 /build/conntrack-image.py --root /engine-root --arch "$TARGETARCH"

FROM scratch AS artifacts
COPY --from=build /out/ /
