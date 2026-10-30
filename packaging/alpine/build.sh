#!/bin/sh
set -eu

source_dir=${1:-/src}
output_dir=${2:-/out}
engine_image=${3:-/inputs/pppoe-engine.tar}
# abuild expands $APK as a command plus arguments, so keep this path free of
# whitespace even when the caller uses a different TMPDIR.
work_dir=$(mktemp -d /tmp/routerd2-abuild.XXXXXX)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
mkdir -p "$work_dir/aports/main/routerd2" "$output_dir"
cd "$work_dir/aports/main/routerd2"
cp "$source_dir/packaging/alpine/APKBUILD" APKBUILD
cp "$engine_image" pppoe-engine.tar
cp /inputs/dnsmasq-engine.tar dnsmasq-engine.tar
cp /inputs/conntrack-engine.tar conntrack-engine.tar
cp /inputs/tailscale-engine.tar tailscale-engine.tar
cp /inputs/chrony-engine.tar chrony-engine.tar
cp /inputs/routerd-modules routerd-modules
cp -a /inputs/ndpi-notices ndpi-notices
pkgver=$(sed -n 's/^pkgver=//p' APKBUILD)

tar --sort=name --format=posix --pax-option=delete=atime,delete=ctime \
  --mtime="@${SOURCE_DATE_EPOCH:-0}" --owner=0 --group=0 --numeric-owner \
  --exclude='./.git' --exclude='./dist' --exclude='./coverage*' \
  --exclude='*/__pycache__' --exclude='*.pyc' \
  --transform="s,^\.,routerd2-$pkgver," \
  --transform="s,^pppoe-engine.tar$,routerd2-$pkgver/pppoe-engine.tar," \
  --transform="s,^dnsmasq-engine.tar$,routerd2-$pkgver/dnsmasq-engine.tar," \
  --transform="s,^conntrack-engine.tar$,routerd2-$pkgver/conntrack-engine.tar," \
  --transform="s,^chrony-engine.tar$,routerd2-$pkgver/chrony-engine.tar," \
  --transform="s,^tailscale-engine.tar$,routerd2-$pkgver/tailscale-engine.tar," \
  --transform="s,^routerd-modules$,routerd2-$pkgver/routerd-modules," \
  --transform="s,^ndpi-notices,routerd2-$pkgver/ndpi-notices," \
  -czf "routerd2-$pkgver.tar.gz" -C "$source_dir" . -C "$work_dir/aports/main/routerd2" pppoe-engine.tar dnsmasq-engine.tar conntrack-engine.tar tailscale-engine.tar chrony-engine.tar routerd-modules ndpi-notices

export REPODEST="$work_dir/packages"
export SRCDEST="$work_dir/distfiles"
export ABUILD_USERDIR="$work_dir/abuild"
export ABUILD_USERCONF="$ABUILD_USERDIR/abuild.conf"
mkdir -p "$SRCDEST"
# This builder supplies the pinned official Go toolchain in /usr/local/go.
# -d skips apk's dependency resolver; alpine-sdk provides the other build tools.
abuild-keygen -a -n
# apk index verifies the freshly signed package. Trust the development key
# only in this private build directory, alongside Alpine's existing keys.
mkdir -p "$work_dir/apk-keys"
for key in /etc/apk/keys/*.pub "$ABUILD_USERDIR"/*.pub; do
  [ -f "$key" ] || continue
  cp "$key" "$work_dir/apk-keys/"
done
export APK="apk --keys-dir $work_dir/apk-keys"
abuild -P "$REPODEST" -s "$SRCDEST" checksum
abuild -P "$REPODEST" -s "$SRCDEST" -d -r

find "$REPODEST" -type f -name 'routerd2-*.apk' -exec cp '{}' "$output_dir/" \;
find "$output_dir" -maxdepth 1 -type f -name 'routerd2-*.apk' | grep -q .
cp APKBUILD "$output_dir/APKBUILD"
cp "$work_dir/aports/main/routerd2/routerd2-$pkgver.tar.gz" "$output_dir/"
find "$ABUILD_USERDIR" -maxdepth 1 -type f -name '*.pub' -exec cp '{}' "$output_dir/" \;
go version > "$output_dir/toolchain.txt"
