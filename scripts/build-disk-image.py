#!/usr/bin/env python3
"""Build a BIOS disk from the standard routerd kernel and initramfs."""

import argparse
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile


def run(command, **kwargs):
    return subprocess.run(command, check=True, text=True, **kwargs)


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def provision_state(source, target):
    """Copy an explicitly prepared native state tree, preserving file modes."""
    if not stat.S_ISDIR(source.lstat().st_mode):
        raise ValueError("state-dir must name a prepared directory")
    for directory, directories, files in os.walk(source):
        for name in directories + files:
            path = Path(directory) / name
            mode = path.lstat().st_mode
            if not (stat.S_ISREG(mode) or stat.S_ISDIR(mode)):
                raise ValueError("state-dir accepts only regular files and directories")
    shutil.copytree(source, target, dirs_exist_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/disk"))
    parser.add_argument("--size-gib", type=int, default=32)
    parser.add_argument("--state-device", default="/dev/vda2")
    parser.add_argument("--config", type=Path)
    parser.add_argument("--authorized-key", type=Path)
    parser.add_argument("--etc-dir", type=Path, help="Provision configuration and trust files below /etc")
    parser.add_argument("--state-dir", type=Path, help="Copy a prepared native state directory into the new state partition; stage only selected identities, not the old router state directory")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root in a private mount namespace (sudo unshare --mount --propagation private)")
    if args.size_gib < 2:
        parser.error("disk must leave room for the 1 GiB boot partition and state")
    device = Path(args.state_device)
    if not device.is_relative_to("/dev") or device == Path("/dev") or ".." in device.parts:
        parser.error("state-device must be an absolute device path below /dev")
    image = args.image_dir.resolve()
    output = args.output.resolve()
    source_manifest = json.loads((image / "manifest.json").read_text())
    if source_manifest.get("input_type") != "apk":
        parser.error("use the standard APK-derived image")
    if source_manifest["kernel_package"].get("architecture") != "x86_64":
        parser.error("BIOS disk requires an amd64/x86_64 image manifest")
    for tool in ("sfdisk", "losetup", "mkfs.ext4", "mount", "umount", "grub-install", "cpio"):
        if shutil.which(tool) is None:
            parser.error(f"missing build tool: {tool}")
    with gzip.open(image / "initramfs.cpio.gz", "rb") as archive:
        extracted = subprocess.run(
            ["cpio", "--quiet", "--extract", "--to-stdout", "etc/routerd/boot.json"],
            input=archive.read(), capture_output=True, check=True,
        )
    boot = json.loads(extracted.stdout)
    if device.name.startswith("sd") and "sd_mod" not in boot["kernel_modules"]:
        parser.error("SCSI state requires an image built with sd_mod in images/boot.json")
    boot["state_volume"] = {"device": str(device), "wait_timeout_seconds": 10}
    if args.authorized_key:
        boot["ssh"]["authorized_keys"] = args.authorized_key.read_text().strip().splitlines()
    output.mkdir(parents=True, exist_ok=True)
    disk = output / "routerd.raw"
    # Only a newly created regular image is provisioned; existing disks are never accepted.
    descriptor = os.open(disk, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "wb") as target:
        target.truncate(args.size_gib * 1024**3)
    spec = importlib.util.spec_from_file_location(
        "mkinitramfs", Path(__file__).resolve().parents[1] / "images/mkinitramfs.py"
    )
    mkinitramfs = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mkinitramfs)
    work = Path(tempfile.mkdtemp(prefix="routerd2-disk-"))
    mounted = None
    try:
        overlay = work / "overlay"
        if args.etc_dir:
            shutil.copytree(args.etc_dir, overlay / "etc")
        (overlay / "etc/routerd").mkdir(parents=True, exist_ok=True)
        (overlay / "etc/routerd/boot.json").write_text(json.dumps(boot, indent=2) + "\n")
        if args.config:
            shutil.copyfile(args.config, overlay / "etc/routerd/config.json")
        selected_config = overlay / "etc/routerd/config.json"
        config_hash = digest(selected_config) if selected_config.exists() else source_manifest["config_sha256"]
        mkinitramfs.write_archive(overlay, work / "boot-overlay.gz", 0)
        run(["sfdisk", str(disk)], input="label: dos\nstart=2048,size=2097152,type=83,bootable\ntype=83\n",
            stdout=subprocess.DEVNULL)
        loop = run(["losetup", "--find", "--show", "--partscan", str(disk)],
                   capture_output=True).stdout.strip()
        try:
            boot_root = work / "boot-volume"
            boot_root.mkdir()
            run(["mkfs.ext4", "-q", "-F", "-L", "routerd-boot", loop + "p1"])
            run(["mkfs.ext4", "-q", "-F", "-L", "routerd-state", loop + "p2"])
            if args.state_dir:
                state_root = work / "state-volume"
                state_root.mkdir()
                run(["mount", "-o", "nosuid,nodev", loop + "p2", str(state_root)])
                mounted = state_root
                provision_state(args.state_dir, state_root)
                run(["umount", str(state_root)])
                mounted = None
            run(["mount", "-o", "nosuid,nodev", loop + "p1", str(boot_root)])
            mounted = boot_root
            grub = boot_root / "boot/grub"
            grub.mkdir(parents=True)
            shutil.copyfile(image / "vmlinuz", boot_root / "boot/vmlinuz")
            initramfs = boot_root / "boot/initramfs.cpio.gz"
            shutil.copyfile(image / "initramfs.cpio.gz", initramfs)
            with initramfs.open("ab") as target, (work / "boot-overlay.gz").open("rb") as extra:
                shutil.copyfileobj(extra, target)
            (grub / "grub.cfg").write_text(
                "serial --unit=0 --speed=115200\n"
                "terminal_input serial\nterminal_output serial\n"
                "set timeout=0\nset default=0\n"
                "menuentry 'routerd2' {\n"
                "  set root=(hd0,msdos1)\n"
                "  linux /boot/vmlinuz console=ttyS0,115200 panic=-1\n"
                "  initrd /boot/initramfs.cpio.gz\n}\n"
            )
            run(["grub-install", "--target=i386-pc", "--boot-directory=" + str(boot_root / "boot"),
                 "--no-floppy", loop])
            installed_initramfs_hash = digest(initramfs)
        finally:
            try:
                if mounted:
                    run(["umount", str(mounted)])
                    mounted = None
            finally:
                run(["losetup", "--detach", loop])
    finally:
        # Never recursively remove a filesystem whose unmount failed.
        if not mounted:
            shutil.rmtree(work)
    manifest = {
        "release_id": source_manifest["release_id"],
        "source_image_manifest_sha256": digest(image / "manifest.json"),
        "source_kernel_sha256": digest(image / "vmlinuz"),
        "source_initramfs_sha256": digest(image / "initramfs.cpio.gz"),
        "installed_initramfs_sha256": installed_initramfs_hash,
        "disk": disk.name, "size_bytes": disk.stat().st_size,
        "boot_method": "bios-grub", "bootloader": run(["grub-install", "--version"], capture_output=True).stdout.strip(),
        "partitions": ["ext4 boot", "ext4 persistent state"],
        "state_volume": boot["state_volume"],
        "initial_state_provisioned": args.state_dir is not None,
        "boot_config_sha256": hashlib.sha256(json.dumps(boot, indent=2).encode() + b"\n").hexdigest(),
        "ssh_enabled": bool(boot.get("ssh", {}).get("authorized_keys")),
        "config_sha256": config_hash,
        "bootloader_ab_updates": False,
    }
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Built BIOS disk {disk} with persistent state {device}")


if __name__ == "__main__":
    main()
