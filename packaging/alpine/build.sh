#!/bin/sh
set -eu

source_dir=${1:-/src}
output_dir=${2:-/out}
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
mkdir -p "$work_dir/aports/main/routerd2" "$output_dir"
cd "$work_dir/aports/main/routerd2"
cp "$source_dir/packaging/alpine/APKBUILD" APKBUILD
pkgver=$(sed -n 's/^pkgver=//p' APKBUILD)

tar --sort=name --format=posix --pax-option=delete=atime,delete=ctime \
  --mtime="@${SOURCE_DATE_EPOCH:-0}" --owner=0 --group=0 --numeric-owner \
  --exclude='./.git' --exclude='./dist' --exclude='./coverage*' \
  --transform="s,^\.,routerd2-$pkgver," \
  -czf "routerd2-$pkgver.tar.gz" -C "$source_dir" .

export REPODEST="$work_dir/packages"
export SRCDEST="$work_dir/distfiles"
mkdir -p "$SRCDEST"
# This builder supplies the pinned official Go toolchain in /usr/local/go.
# -d skips apk's dependency resolver; alpine-sdk provides the other build tools.
abuild-keygen -a -n
abuild -P "$REPODEST" -s "$SRCDEST" checksum
abuild -P "$REPODEST" -s "$SRCDEST" -d -r

find "$REPODEST" -type f -name 'routerd2-*.apk' -exec cp '{}' "$output_dir/" \;
find "$output_dir" -maxdepth 1 -type f -name 'routerd2-*.apk' | grep -q .
cp APKBUILD "$output_dir/APKBUILD"
cp "$work_dir/aports/main/routerd2/routerd2-$pkgver.tar.gz" "$output_dir/"
find "$HOME/.abuild" -maxdepth 1 -type f -name '*.pub' -exec cp '{}' "$output_dir/" \;
go version > "$output_dir/toolchain.txt"
