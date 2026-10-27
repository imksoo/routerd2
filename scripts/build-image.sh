#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
package_dir=
binary_dir=
output_dir=
config_file="$project_dir/images/config-empty.json"
boot_file="$project_dir/images/boot.json"
kernel_root=/
engine_image="$project_dir/dist/pppoe-engine.tar"
engine_sources="$project_dir/dist/pppoe-sources.tar"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --package-dir) package_dir=$2; shift 2 ;;
    --binary-dir) binary_dir=$2; shift 2 ;;
    --output) output_dir=$2; shift 2 ;;
    --config) config_file=$2; shift 2 ;;
    --boot) boot_file=$2; shift 2 ;;
    --kernel-root) kernel_root=$2; shift 2 ;;
    --engine-image) engine_image=$2; shift 2 ;;
    --engine-sources) engine_sources=$2; shift 2 ;;
    *) printf 'Unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
if [ -z "$output_dir" ] || { [ -z "$package_dir" ] && [ -z "$binary_dir" ]; } || { [ -n "$package_dir" ] && [ -n "$binary_dir" ]; }; then
  printf '%s\n' 'Usage: build-image.sh (--package-dir DIR | --binary-dir DIR) --output DIR [--config JSON] [--boot JSON] [--kernel-root DIR]' >&2
  exit 2
fi

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM
package_root="$work_dir/package"
rootfs="$work_dir/rootfs"
mkdir -p "$package_root" "$rootfs" "$output_dir"
package_file=
if [ -n "$package_dir" ]; then
  set -- "$package_dir"/routerd2-*.apk
  if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
    printf '%s\n' 'Expected exactly one routerd2 APK in package directory.' >&2
    exit 1
  fi
  package_file=$1
  engine_sources="$package_dir/pppoe-sources.tar"
  # The APK is generated in the same build. --no-scripts prevents packaging hooks
  # from introducing runtime init or service-manager dependencies.
  apk --root "$package_root" --initdb --no-scripts --no-network --allow-untrusted add "$package_file"
else
  # Explicit development path for testing already-built binaries. The manifest
  # distinguishes it from an APK build; it does not skip or claim abuild checks.
  mkdir -p "$package_root/usr/sbin" "$package_root/usr/libexec" "$package_root/usr/share/licenses/routerd2" "$package_root/usr/lib/routerd"
  cp "$engine_image" "$package_root/usr/lib/routerd/pppoe-engine.tar"
  cp "$binary_dir/routerd" "$package_root/usr/sbin/routerd"
  cp "$binary_dir/routerd-modules" "$package_root/usr/libexec/routerd-modules"
  cp "$binary_dir/routerd-logging" "$package_root/usr/libexec/routerd-logging"
  cp "$project_dir/LICENSE" "$package_root/usr/share/licenses/routerd2/LICENSE"
  if [ ! -f "$project_dir/dist/licenses/manifest.json" ]; then
    printf '%s\n' 'Development images require dependency notices; run make licenses first.' >&2
    exit 1
  fi
  cp -a "$project_dir/dist/licenses" "$package_root/usr/share/licenses/routerd2/dependencies"
fi

mkdir -p "$rootfs/usr/sbin" "$rootfs/usr/libexec" "$rootfs/etc/routerd" \
  "$rootfs/usr/lib/routerd" \
  "$rootfs/etc/ssl/certs" "$rootfs/usr/share/licenses/routerd2" \
  "$rootfs/lib/modules" "$rootfs/dev" "$rootfs/proc" "$rootfs/sys" \
  "$rootfs/run" "$rootfs/tmp" "$rootfs/var/lib/routerd"
chmod 1777 "$rootfs/tmp"
cp "$package_root/usr/sbin/routerd" "$rootfs/usr/sbin/routerd"
cp "$package_root/usr/libexec/routerd-modules" "$rootfs/usr/libexec/routerd-modules"
cp "$package_root/usr/libexec/routerd-logging" "$rootfs/usr/libexec/routerd-logging"
cp "$package_root/usr/lib/routerd/pppoe-engine.tar" "$rootfs/usr/lib/routerd/pppoe-engine.tar"
cp "$engine_sources" "$output_dir/pppoe-sources.tar"
cp "$package_root/usr/share/licenses/routerd2/LICENSE" "$rootfs/usr/share/licenses/routerd2/LICENSE"
cp -a "$package_root/usr/share/licenses/routerd2/dependencies" "$rootfs/usr/share/licenses/routerd2/"
cp "$config_file" "$rootfs/etc/routerd/config.json"
cp "$boot_file" "$rootfs/etc/routerd/boot.json"
cp /etc/ssl/certs/ca-certificates.crt "$rootfs/etc/ssl/certs/ca-certificates.crt"
ln -s usr/sbin/routerd "$rootfs/init"

set -- "$kernel_root"/lib/modules/*
if [ "$#" -ne 1 ] || [ ! -d "$1" ]; then
  printf '%s\n' 'Expected one installed kernel module tree.' >&2
  exit 1
fi
kernel_version=${1##*/}
cp -a "$1" "$rootfs/lib/modules/"
module_dir="$rootfs/lib/modules/$kernel_version"
rm -f "$module_dir/build" "$module_dir/source" "$module_dir/vmlinuz"
# routerd's native loader consumes raw .ko files and modules.dep. All external
# decompressors and depmod remain build-time tools.
find "$module_dir" -type f -name '*.ko.gz' -exec gzip -d '{}' \;
find "$module_dir" -type f -name '*.ko.xz' -exec xz -d '{}' \;
find "$module_dir" -type f -name '*.ko.zst' -exec zstd -q -d --rm '{}' \;
depmod -b "$rootfs" "$kernel_version"
# The lts kernel supplies PPPoE; virt deliberately omits it. Keep the declared
# router/storage driver closure rather than putting an entire general-purpose
# kernel's module tree into the initramfs. modprobe is a build-time tool only.
python3 - "$rootfs" "$kernel_version" "$boot_file" <<'PY'
import json
from pathlib import Path
import subprocess
import sys

root, version = Path(sys.argv[1]), sys.argv[2]
names = json.loads(Path(sys.argv[3]).read_text())["kernel_modules"]
keep = set()
for name in names:
    result = subprocess.run(["modprobe", "--dirname", str(root), "--set-version", version, "--show-depends", name], check=True, capture_output=True, text=True)
    for line in result.stdout.splitlines():
        if line.startswith("insmod "):
            keep.add(Path(line.split()[1]).resolve())
for file in (root / "lib/modules" / version).rglob("*.ko"):
    if file.resolve() not in keep:
        file.unlink()
PY
depmod -b "$rootfs" "$kernel_version"
cp "$kernel_root/boot/vmlinuz-lts" "$output_dir/vmlinuz"

python3 - "$rootfs" "$output_dir" "$kernel_version" "$package_file" "$kernel_root" <<'PY'
import hashlib
import json
import os
from pathlib import Path
import re
import sys

root, output = Path(sys.argv[1]), Path(sys.argv[2])
boot_path = root / "etc/routerd/boot.json"
boot = json.loads(boot_path.read_text())
key = os.environ.get("SSH_AUTHORIZED_KEY", "").strip()
if key:
    if "\n" in key or not key.startswith(("ssh-ed25519 ", "ssh-rsa ", "ecdsa-sha2-")):
        raise SystemExit("SSH_AUTHORIZED_KEY must contain one OpenSSH public key")
    boot["ssh"]["authorized_keys"] = [key]
boot_path.write_text(json.dumps(boot, indent=2) + "\n")
config = json.loads((root / "etc/routerd/config.json").read_text())
release = os.environ.get("ROUTERD_RELEASE_ID", "dev")
kernel_package = None
database = Path(sys.argv[5]) / "lib/apk/db/installed"
for block in database.read_text().split("\n\n"):
    fields = dict(line.split(":", 1) for line in block.splitlines() if ":" in line)
    if fields.get("P") == "linux-lts":
        commit = fields.get("c", "")
        origin = fields.get("o", "")
        if not re.fullmatch(r"[0-9a-f]{40}", commit) or not re.fullmatch(r"[a-z0-9_-]+", origin):
            raise SystemExit("kernel APK metadata has no valid pinned source origin")
        kernel_package = {
            "name": fields["P"], "version": fields.get("V"),
            "license_expression_from_apk": fields.get("L"),
            "origin": origin, "aports_commit": commit,
            "source_recipe_url": f"https://github.com/alpinelinux/aports/blob/{commit}/main/{origin}/APKBUILD",
            "source_recipe_directory_url": f"https://github.com/alpinelinux/aports/tree/{commit}/main/{origin}",
            "corresponding_source_archive_included": False,
        }
        break
if kernel_package is None:
    raise SystemExit("linux-lts package provenance is absent from the kernel root")
(root / "etc/os-release").write_text(
    'NAME="routerd2"\nID=routerd2\nID_LIKE=alpine\n'
    'PRETTY_NAME="routerd2 appliance"\nBUILD_ID=' + json.dumps(release) + "\n"
)
manifest = {
    "schema_version": 1,
    "release_id": release,
    "alpine_release": Path("/etc/alpine-release").read_text().strip(),
    "kernel_release": sys.argv[3],
    "kernel_package": kernel_package,
    "input_type": "apk" if sys.argv[4] else "development-binaries",
    "package": Path(sys.argv[4]).name if sys.argv[4] else None,
    "package_sha256": hashlib.sha256(Path(sys.argv[4]).read_bytes()).hexdigest() if sys.argv[4] else None,
    "binary_sha256": {
        name: hashlib.sha256((root / name.lstrip("/")).read_bytes()).hexdigest()
        for name in ("/usr/sbin/routerd", "/usr/libexec/routerd-modules", "/usr/libexec/routerd-logging")
    },
    "dependency_license_manifest_sha256": hashlib.sha256(
        (root / "usr/share/licenses/routerd2/dependencies/manifest.json").read_bytes()
    ).hexdigest(),
    "init": "/usr/sbin/routerd",
    "programs": ["/usr/sbin/routerd", "/usr/libexec/routerd-modules", "/usr/libexec/routerd-logging"],
    "pppoe_engine": {
        "path": "/usr/lib/routerd/pppoe-engine.tar",
        "sha256": hashlib.sha256((root / "usr/lib/routerd/pppoe-engine.tar").read_bytes()).hexdigest(),
        "sources_file": "pppoe-sources.tar",
        "sources_sha256": hashlib.sha256((output / "pppoe-sources.tar").read_bytes()).hexdigest(),
    },
    "platform": boot["platform"],
    "kernel_modules": boot["kernel_modules"],
    "ssh_enabled": bool(boot.get("ssh", {}).get("authorized_keys")),
    "config_sha256": hashlib.sha256((root / "etc/routerd/config.json").read_bytes()).hexdigest(),
    "boot_config_sha256": hashlib.sha256(boot_path.read_bytes()).hexdigest(),
    "state_persistence": "configured-ext4-state-volume" if boot.get("state_volume") is not None else "volatile-initramfs",
    "state_volume": boot.get("state_volume"),
    "state_volume_provisioned_by_builder": False,
    "userland_release_selection_configured": boot.get("release") is not None,
    "boot_method": "direct-kernel-initramfs",
    "bootloader_ab_updates": False,
}
(output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
PY

python3 "$project_dir/images/mkinitramfs.py" "$rootfs" "$output_dir/initramfs.cpio.gz" --epoch "${SOURCE_DATE_EPOCH:-0}"
(
  cd "$output_dir"
  sha256sum vmlinuz initramfs.cpio.gz manifest.json pppoe-sources.tar > SHA256SUMS
)
printf 'Built kernel %s and routerd initramfs in %s\n' "$kernel_version" "$output_dir"
