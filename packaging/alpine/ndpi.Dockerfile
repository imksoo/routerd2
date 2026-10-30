# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27.3
ARG ALPINE_VERSION=3.24.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine3.24 AS toolchain
FROM --platform=$BUILDPLATFORM alpine:${ALPINE_VERSION} AS libraries
RUN apk add --no-cache autoconf automake libtool make pkgconf python3 ca-certificates tar binutils zig=0.16.0-r1
COPY --from=toolchain /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local GOMAXPROCS=2 GOFLAGS=-p=2
ARG TARGETARCH
RUN case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) exit 2 ;; esac \
 && mkdir -p /target/etc/apk /opt/ndpi/include /opt/ndpi/lib \
 && cp -a /etc/apk/keys /target/etc/apk/keys \
 && cp /usr/share/apk/keys/"$arch"/*.pub /target/etc/apk/keys/ \
 && cp /etc/apk/repositories /target/etc/apk/repositories \
 && apk --root /target --arch "$arch" --initdb --no-scripts --no-cache add \
      libgcrypt-dev=1.12.2-r0 libgcrypt-static=1.12.2-r0 libgpg-error-dev=1.61-r0 libgpg-error-static=1.61-r0 \
 && cp /target/usr/include/gcrypt.h /target/usr/include/gpg-error.h /opt/ndpi/include/ \
 && cp /target/usr/lib/libgcrypt.a /target/usr/lib/libgpg-error.a /opt/ndpi/lib/
RUN --mount=type=cache,target=/root/.cache/zig mkdir -p /sources \
 && wget -qO /sources/nDPI-4.2.tar.gz https://codeload.github.com/ntop/nDPI/tar.gz/refs/tags/4.2 \
 && echo 'e54ce8fe13adc5d747be7553513657fae78f796e0bd459e122c280cc06ce4daf  /sources/nDPI-4.2.tar.gz' | sha256sum -c - \
 && mkdir -p /build/ndpi \
 && tar -xzf /sources/nDPI-4.2.tar.gz --strip-components=1 -C /build/ndpi \
 && cd /build/ndpi \
 && case "$TARGETARCH" in amd64) target=x86_64-linux-musl ;; arm64) target=aarch64-linux-musl ;; esac \
 && export SOURCE_DATE_EPOCH="$(stat -c %Y CHANGELOG.md)" \
 && autoreconf -fi \
 && CC="zig cc -target $target" AR="zig ar" RANLIB="zig ranlib" CFLAGS="-O2 -std=gnu17 -I/opt/ndpi/include" CPPFLAGS=-I/opt/ndpi/include LDFLAGS=-L/opt/ndpi/lib \
      ./configure --host="$target" --build="$(uname -m)-linux-musl" --prefix=/opt/ndpi --with-only-libndpi --disable-shared \
 && make -C src/lib -j2 libndpi.a \
 && mkdir -p /opt/ndpi/include/ndpi /opt/ndpi/lib/pkgconfig \
 && cp src/lib/libndpi.a /opt/ndpi/lib/ \
 && cp src/include/*.h /opt/ndpi/include/ndpi/ \
 && printf '%s\n' 'prefix=/opt/ndpi' 'libdir=${prefix}/lib' 'includedir=${prefix}/include/ndpi' \
      'Name: libndpi' 'Description: nDPI packet classifier' 'Version: 4.2.0' \
      'Libs: -L${libdir} -lndpi -lm -lgcrypt -lgpg-error' 'Cflags: -I${includedir}' >/opt/ndpi/lib/pkgconfig/libndpi.pc

FROM libraries AS build
WORKDIR /src
ARG TARGETARCH
COPY scripts/ndpi-sources.py scripts/engine_sources.py ./scripts/
COPY packaging/alpine/licenses/GPL-3.0.txt ./packaging/alpine/licenses/
RUN python3 scripts/ndpi-sources.py --root /target --arch "$TARGETARCH"
COPY go.mod go.sum ./
COPY third_party/google-nftables/ ./third_party/google-nftables/
RUN go mod download
COPY . .
ARG RELEASE_ID=dev
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/.cache/zig \
    case "$TARGETARCH" in amd64) target=x86_64-linux-musl ;; arm64) target=aarch64-linux-musl ;; *) exit 2 ;; esac \
 && mkdir -p /out \
 && CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" CC="zig cc -target $target" PKG_CONFIG_PATH=/opt/ndpi/lib/pkgconfig \
      go build -buildmode=exe -tags=libndpi,netgo,osusergo -trimpath -buildvcs=false \
      -ldflags "-s -w -linkmode external -extldflags -static -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:$RELEASE_ID:END" \
      -o /out/routerd-modules ./cmd/routerd-modules
RUN tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner \
      --transform='s,^\.,routerd2,' -rf /out/ndpi-sources.tar -C /src .

FROM scratch AS artifacts
COPY --from=build /out/ /
