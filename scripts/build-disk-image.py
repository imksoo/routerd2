#!/usr/bin/env python3
"""Build a BIOS/UEFI GPT disk from the standard routerd kernel and initramfs."""

import argparse
from email.parser import Parser
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
import tarfile

from engine_sources import add


def run(command, **kwargs):
    return subprocess.run(command, check=True, text=True, **kwargs)


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def unpack_initramfs(source, target):
    target.mkdir()
    with gzip.open(source, "rb") as archive:
        subprocess.run(["cpio", "--quiet", "--extract", "--make-directories", "--no-absolute-filenames"], cwd=target, input=archive.read(), check=True)
    # write_archive provides the initial devices itself. They were decoded only
    # because the source image is a complete initramfs.
    for name in ("console", "null"):
        (target / "dev" / name).unlink(missing_ok=True)


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


def collect_grub_sources(source_dir, output):
    paths = [shutil.which("grub-install"), "/usr/lib/grub/i386-pc/modinfo.sh", "/usr/lib/grub/x86_64-efi/modinfo.sh"]
    ownership = run(["dpkg-query", "--search", *paths], capture_output=True).stdout
    owners = sorted({line.rsplit(": ", 1)[0] for line in ownership.splitlines()})
    fields = "${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\t${Architecture}\n"
    installed = run(["dpkg-query", "--show", "--showformat=" + fields, *owners], capture_output=True).stdout
    packages = []
    for line in installed.splitlines():
        name, version, source_name, source_version, architecture = line.split("\t")
        packages.append({"name": name, "version": version, "source_name": source_name,
                         "source_version": source_version, "architecture": architecture})
    descriptions = []
    for path in sorted(source_dir.glob("*.dsc")):
        raw = path.read_bytes()
        body = raw.decode().replace("\r\n", "\n")
        if body.startswith("-----BEGIN PGP SIGNED MESSAGE-----"):
            body = body.split("\n\n", 1)[1].split("\n-----BEGIN PGP SIGNATURE-----", 1)[0]
            body = "\n".join(line.removeprefix("- ") for line in body.splitlines())
        descriptions.append((path, Parser().parsestr(body), raw))
    metadata = {"version": run(["grub-install", "--version"], capture_output=True).stdout.strip(),
                "packages": packages, "sources": []}
    with tarfile.open(output, "w", format=tarfile.USTAR_FORMAT) as archive:
        for source_name, source_version in sorted({(p["source_name"], p["source_version"]) for p in packages}):
            for path, description, raw in descriptions:
                if description.get("Source") != source_name:
                    continue
                equal = subprocess.run(["dpkg", "--compare-versions", description.get("Version", ""), "eq", source_version])
                if equal.returncode == 0:
                    break
            else:
                raise ValueError(f"download the authenticated source package {source_name}={source_version} into {source_dir}")
            prefix = f"sources/{source_name}/{source_version}/"
            add(archive, prefix + path.name, raw)
            checksums = [line for line in description.get("Checksums-Sha256", "").splitlines() if line.strip()]
            if not checksums:
                raise ValueError(f"{path.name} has no SHA256 source checksums")
            for checksum in checksums:
                expected, size, name = checksum.split()
                filename = Path(name)
                if filename.parent != Path(".") or filename.name in ("", ".", ".."):
                    raise ValueError(f"source filename must stay in the selected directory: {name}")
                source = source_dir / filename
                if source.stat().st_size != int(size) or digest(source) != expected.lower():
                    raise ValueError(f"GRUB source checksum mismatch: {name}")
                entry = tarfile.TarInfo(prefix + filename.name)
                entry.size, entry.mode = int(size), 0o444
                with source.open("rb") as content:
                    archive.addfile(entry, content)
            metadata["sources"].append({"name": source_name, "version": source_version, "directory": prefix})
        for package in packages:
            name = package["name"].split(":", 1)[0]
            add(archive, "notices/" + name + "/copyright", (Path("/usr/share/doc") / name / "copyright").read_bytes())
        add(archive, "metadata.json", (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode())
    metadata["sources_file"] = output.name
    metadata["sources_sha256"] = digest(output)
    return metadata


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/disk"))
    parser.add_argument("--size-gib", type=int, default=32)
    parser.add_argument("--state-device", help="Override the image's persistent state device")
    parser.add_argument("--config", type=Path)
    parser.add_argument("--authorized-key", type=Path)
    parser.add_argument("--etc-dir", type=Path, help="Provision configuration and trust files below /etc")
    parser.add_argument("--state-dir", type=Path, help="Copy a prepared native state directory into the new state partition; stage only selected identities, not the old router state directory")
    parser.add_argument("--grub-source-dir", type=Path, required=True, help="Authenticated Debian/Ubuntu GRUB source packages downloaded with apt-get source --download-only")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root in a private mount namespace (sudo unshare --mount --propagation private)")
    if args.size_gib < 2:
        parser.error("disk must leave room for the 1 GiB boot partition and state")
    image = args.image_dir.resolve()
    output = args.output.resolve()
    source_manifest = json.loads((image / "manifest.json").read_text())
    if source_manifest.get("input_type") != "apk":
        parser.error("use the standard APK-derived image")
    if source_manifest["kernel_package"].get("architecture") != "x86_64":
        parser.error("x86 disk requires an amd64/x86_64 image manifest")
    for tool in ("sfdisk", "losetup", "mkfs.ext4", "mkfs.vfat", "blkid", "mount", "umount",
                 "grub-install", "dpkg-query", "dpkg", "cpio"):
        if shutil.which(tool) is None:
            parser.error(f"missing build tool: {tool}")
    with gzip.open(image / "initramfs.cpio.gz", "rb") as archive:
        extracted = subprocess.run(
            ["cpio", "--quiet", "--extract", "--to-stdout", "etc/routerd/boot.json"],
            input=archive.read(), capture_output=True, check=True,
        )
    boot = json.loads(extracted.stdout)
    state_volume = boot.get("state_volume")
    if state_volume is None:
        state_volume = {"device": "/dev/vda4", "wait_timeout_seconds": 10}
    if args.state_device is not None:
        state_volume["device"] = args.state_device
    device = Path(state_volume["device"])
    if not device.is_relative_to("/dev") or device == Path("/dev") or ".." in device.parts:
        parser.error("state-device must be an absolute device path below /dev")
    if device.name.startswith("sd") and "sd_mod" not in boot["kernel_modules"]:
        parser.error("SCSI state requires an image built with sd_mod in kernel_modules")
    state_volume["device"] = str(device)
    boot["state_volume"] = state_volume
    if args.authorized_key:
        boot["ssh"]["authorized_keys"] = args.authorized_key.read_text().strip().splitlines()
    spec = importlib.util.spec_from_file_location(
        "mkinitramfs", Path(__file__).resolve().parents[1] / "images/mkinitramfs.py"
    )
    mkinitramfs = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mkinitramfs)
    work = Path(tempfile.mkdtemp(prefix="routerd2-disk-"))
    mounted = []
    try:
        bootloader = collect_grub_sources(args.grub_source_dir, work / "grub-sources.tar")
        output.mkdir(parents=True, exist_ok=True)
        disk = output / "routerd.raw"
        # Reserve this image before changing any existing distribution files.
        descriptor = os.open(disk, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "wb") as target:
            target.truncate(args.size_gib * 1024**3)
        overlay = work / "overlay"
        if args.etc_dir:
            shutil.copytree(args.etc_dir, overlay / "etc")
        (overlay / "etc/routerd").mkdir(parents=True, exist_ok=True)
        (overlay / "etc/routerd/boot.json").write_text(json.dumps(boot, indent=2) + "\n")
        if args.config:
            shutil.copyfile(args.config, overlay / "etc/routerd/config.json")
        selected_config = overlay / "etc/routerd/config.json"
        config_hash = digest(selected_config) if selected_config.exists() else source_manifest["config_sha256"]
        root = work / "root"
        unpack_initramfs(image / "initramfs.cpio.gz", root)
        shutil.copytree(overlay, root, dirs_exist_ok=True)
        merged_initramfs = work / "initramfs.cpio.gz"
        mkinitramfs.write_archive(root, merged_initramfs, 0)
        run(["sfdisk", str(disk)], input=(
            "label: gpt\n"
            "start=2048,size=2048,type=21686148-6449-6E6F-744E-656564454649,name=routerd-bios\n"
            "size=262144,type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B,name=routerd-efi\n"
            "size=2097152,type=0FC63DAF-8483-4772-8E79-3D69D8477DE4,name=routerd-boot\n"
            "type=0FC63DAF-8483-4772-8E79-3D69D8477DE4,name=routerd-state\n"),
            stdout=subprocess.DEVNULL)
        loop = run(["losetup", "--find", "--show", "--partscan", str(disk)],
                   capture_output=True).stdout.strip()
        try:
            boot_root = work / "boot-volume"
            boot_root.mkdir()
            run(["mkfs.vfat", "-F", "32", "-n", "ROUTERD-EFI", loop + "p2"])
            run(["mkfs.ext4", "-q", "-F", "-L", "routerd-boot", loop + "p3"])
            run(["mkfs.ext4", "-q", "-F", "-L", "routerd-state", loop + "p4"])
            boot_uuid = run(["blkid", "-s", "UUID", "-o", "value", loop + "p3"], capture_output=True).stdout.strip()
            if args.state_dir:
                state_root = work / "state-volume"
                state_root.mkdir()
                run(["mount", "-o", "nosuid,nodev", loop + "p4", str(state_root)])
                mounted.append(state_root)
                provision_state(args.state_dir, state_root)
                run(["umount", str(state_root)])
                mounted.pop()
            run(["mount", "-o", "nosuid,nodev", loop + "p3", str(boot_root)])
            mounted.append(boot_root)
            efi_root = boot_root / "boot/efi"
            efi_root.mkdir(parents=True)
            run(["mount", "-o", "nosuid,nodev", loop + "p2", str(efi_root)])
            mounted.append(efi_root)
            grub = boot_root / "boot/grub"
            grub.mkdir(parents=True)
            shutil.copyfile(image / "vmlinuz", boot_root / "boot/vmlinuz")
            initramfs = boot_root / "boot/initramfs.cpio.gz"
            shutil.copyfile(merged_initramfs, initramfs)
            (grub / "grub.cfg").write_text(
                "serial --unit=0 --speed=115200\n"
                "terminal_input console serial\nterminal_output console serial\n"
                "set timeout=0\nset default=0\n"
                "menuentry 'routerd2' {\n"
                f"  search --no-floppy --fs-uuid --set=root {boot_uuid}\n"
                "  linux /boot/vmlinuz console=tty0 console=ttyS0,115200 panic=-1\n"
                "  initrd /boot/initramfs.cpio.gz\n}\n"
            )
            run(["grub-install", "--target=i386-pc", "--boot-directory=" + str(boot_root / "boot"),
                 "--directory=/usr/lib/grub/i386-pc", "--fonts=", "--themes=", "--locales=", "--no-floppy", loop])
            run(["grub-install", "--target=x86_64-efi", "--boot-directory=" + str(boot_root / "boot"),
                 "--directory=/usr/lib/grub/x86_64-efi", "--fonts=", "--themes=", "--locales=",
                 "--efi-directory=" + str(efi_root), "--removable", "--no-nvram", "--no-uefi-secure-boot"])
            installed_initramfs_hash = digest(initramfs)
        finally:
            try:
                for mount_point in reversed(mounted):
                    run(["umount", str(mount_point)])
                    mounted.pop()
            finally:
                run(["losetup", "--detach", loop])
        shutil.copyfile(work / "grub-sources.tar", output / "grub-sources.tar")
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
        "boot_methods": ["bios-grub", "uefi-x86_64-grub"], "bootloader": bootloader,
        "partition_table": "gpt",
        "partitions": ["BIOS boot", "FAT32 EFI system", "ext4 boot", "ext4 persistent state"],
        "state_volume": boot["state_volume"],
        "initial_state_provisioned": args.state_dir is not None,
        "boot_config_sha256": hashlib.sha256(json.dumps(boot, indent=2).encode() + b"\n").hexdigest(),
        "ssh_enabled": bool(boot.get("ssh", {}).get("authorized_keys")),
        "config_sha256": config_hash,
        "bootloader_ab_updates": False,
    }
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Built BIOS/UEFI disk {disk} with persistent state {device}")


if __name__ == "__main__":
    main()
