#!/usr/bin/env python3
# SPDX-License-Identifier: BSD-3-Clause
"""Verify signed pair selection and real PID-1 confirmation in a no-NIC VM."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import tempfile
import time


PROJECT = Path(__file__).resolve().parents[3]
MARKER = "ROUTERD_RELEASE_BOOT"
IDENTITIES = {"baseline": "release-gate-baseline", "trial": "release-gate-trial"}


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def exact_read(stream, size):
    data = stream.read(size)
    if len(data) != size:
        raise ValueError("truncated source initramfs")
    return data


def extract_modules(archive, root):
    """Copy regular kernel modules, without build/source/vmlinuz symlinks."""
    count = 0
    with gzip.open(archive, "rb") as source:
        while True:
            header = exact_read(source, 110)
            if header[:6] != b"070701":
                raise ValueError("source initramfs must use newc")
            values = [int(header[offset:offset + 8], 16) for offset in range(6, 110, 8)]
            mode, size, name_size = values[1], values[6], values[11]
            if not 0 < name_size <= 4096 or size > 512 * 1024 * 1024:
                raise ValueError("source initramfs entry exceeds fixture bounds")
            encoded = exact_read(source, name_size)
            exact_read(source, (-(110 + name_size)) % 4)
            if encoded[-1:] != b"\0":
                raise ValueError("source initramfs name is not terminated")
            name = encoded[:-1].decode("utf-8")
            if name == "TRAILER!!!":
                break
            relative = PurePosixPath(name)
            if relative.is_absolute() or ".." in relative.parts:
                raise ValueError("unsafe path in source initramfs")
            data = exact_read(source, size)
            exact_read(source, (-size) % 4)
            if len(relative.parts) < 2 or relative.parts[:2] != ("lib", "modules"):
                continue
            target = root.joinpath(*relative.parts)
            if stat.S_ISDIR(mode):
                target.mkdir(parents=True, exist_ok=True)
            elif stat.S_ISREG(mode):
                target.parent.mkdir(parents=True, exist_ok=True)
                if target.exists():
                    raise ValueError("duplicate kernel module archive path")
                target.write_bytes(data)
                target.chmod(stat.S_IMODE(mode))
                count += 1
            elif not stat.S_ISLNK(mode):
                raise ValueError("special kernel module archive entry")
    if count == 0:
        raise ValueError("source appliance image contains no kernel modules")


def write_entry(stream, inode, name, mode, size=0, source=None, device=(0, 0)):
    encoded = name.encode("utf-8") + b"\0"
    fields = (inode, mode, 0, 0, 2 if stat.S_ISDIR(mode) else 1, 0, size,
              0, 0, device[0], device[1], len(encoded), 0)
    stream.write(b"070701" + "".join(f"{field:08x}" for field in fields).encode())
    stream.write(encoded)
    stream.write(b"\0" * ((-(110 + len(encoded))) % 4))
    remaining = size
    while remaining:
        data = source.read(min(remaining, 1024 * 1024))
        if not data:
            raise ValueError("fixture file changed while archiving")
        stream.write(data)
        remaining -= len(data)
    stream.write(b"\0" * ((-size) % 4))


def write_initramfs(root, output):
    paths = {path.relative_to(root).as_posix(): path for path in root.rglob("*")}
    # Device entries are emitted into the archive for kernel extraction.
    # No device node is created on the build host.
    devices = {"dev/console": (5, 1), "dev/null": (1, 3)}
    if set(paths) & set(devices):
        raise ValueError("unexpected build-host device in fixture root")
    with output.open("wb") as raw:
        with gzip.GzipFile(fileobj=raw, filename="", mode="wb", mtime=0, compresslevel=1) as target:
            names = sorted(set(paths) | set(devices))
            for inode, name in enumerate(names, 1):
                if name in devices:
                    write_entry(target, inode, name, stat.S_IFCHR | 0o600, device=devices[name])
                    continue
                path = paths[name]
                metadata = path.lstat()
                if stat.S_ISDIR(metadata.st_mode):
                    write_entry(target, inode, name, metadata.st_mode)
                elif stat.S_ISREG(metadata.st_mode):
                    with path.open("rb") as source:
                        write_entry(target, inode, name, metadata.st_mode, metadata.st_size, source)
                else:
                    raise ValueError(f"unexpected fixture file type: {name}")
            write_entry(target, len(names) + 1, "TRAILER!!!", 0)


def build_image(args, image, output):
    source_manifest = json.loads((image / "manifest.json").read_text())
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64",
                       GOAMD64="v1", GOFLAGS="")
    # Resolve go.mod's selected toolchain before disabling automatic selection.
    # The system launcher may be older than the downloaded project toolchain.
    go_root = subprocess.check_output([args.go, "env", "GOROOT"], cwd=PROJECT, env=environment, text=True).strip()
    go_binary = str(Path(go_root) / "bin/go")
    environment["GOTOOLCHAIN"] = "local"
    with tempfile.TemporaryDirectory(prefix="routerd2-release-boot-") as temporary:
        root = Path(temporary)
        for directory in ("dev", "proc", "sys", "run", "tmp", "var/lib", "test"):
            (root / directory).mkdir(parents=True, exist_ok=True)
        (root / "tmp").chmod(0o1777)
        print("Copying appliance modules into an isolated release-boot image...", flush=True)
        extract_modules(image / "initramfs.cpio.gz", root)
        print("Building the guest-only initializer and independent health probe...", flush=True)
        subprocess.run([
            go_binary, "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
            "-ldflags", "-s -w", "-o", str(root / "init"),
            "./internal/releaseboot/testdata/guest",
        ], check=True, cwd=PROJECT, env=environment)
        hashes = {}
        for directory, identity in IDENTITIES.items():
            print(f"Building the actual routerd command pair for {identity}...", flush=True)
            target = root / "test/bin" / directory
            target.mkdir(parents=True)
            for name in ("routerd", "routerd-modules"):
                binary = target / name
                subprocess.run([
                    go_binary, "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
                    "-ldflags", "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity="
                    f"ROUTERD2-RELEASE-V1:{identity}:END",
                    "-o", str(binary), f"./cmd/{name}",
                ], check=True, cwd=PROJECT, env=environment)
                hashes[f"/test/bin/{directory}/{name}"] = sha256(binary)
        manifest = {
            "schema_version": 1,
            "go": subprocess.check_output([go_binary, "version"], env=environment, text=True).strip(),
            "purpose": "real-pid1-signed-userland-selection-and-health-confirmation",
            "target": "linux/amd64",
            "kernel_release": source_manifest["kernel_release"],
            "kernel_sha256": sha256(image / "vmlinuz"),
            "source_image_manifest_sha256": sha256(image / "manifest.json"),
            "initializer_sha256": sha256(root / "init"),
            "binary_sha256": hashes,
            "release_ids": IDENTITIES,
            "host_network_access": False,
            "guest_external_network_interfaces": False,
            "state_persistence": "volatile-initramfs",
            "kernel_rootfs_updates": False,
            "two_boot_disk_fallback_tested": False,
            "result": "not-run",
        }
        archive = output / "initramfs.cpio.gz"
        write_initramfs(root, archive)
        manifest["initramfs_sha256"] = sha256(archive)
    return manifest


def run_guest(args, image, output, manifest):
    serial = output / "qemu-serial.log"
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "1024M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{serial}",
        "-kernel", str(image / "vmlinuz"), "-initrd", str(output / "initramfs.cpio.gz"),
        "-append", "console=ttyS0,115200 panic=-1 routerd2_release_boot_fixture=1",
    ]
    print("Booting real PID-1 trial in QEMU (no NIC, no host ports or shared disk)...", flush=True)
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    start = time.monotonic()
    passed_at = None
    error = None
    try:
        while time.monotonic() - start < args.timeout:
            log = serial.read_text(errors="replace") if serial.exists() else ""
            lines = [line.strip() for line in log.splitlines()]
            if any(MARKER + "_FAIL" in line for line in lines) or "Kernel panic" in log:
                raise RuntimeError("release boot fixture failed in guest")
            if MARKER + "_PASS" in lines and passed_at is None:
                if not any(line.startswith(MARKER + "_WITNESS ") for line in lines):
                    raise RuntimeError("guest reported PASS without its independent execution witness")
                passed_at = time.monotonic()
            status = process.poll()
            if status is not None:
                if passed_at is None or status != 0:
                    detail = process.stderr.read().decode(errors="replace")
                    raise RuntimeError(f"QEMU exited {status} without confirmed trial and clean poweroff: {detail}")
                manifest["result"] = "passed"
                manifest["witness"] = next(line for line in lines if line.startswith(MARKER + "_WITNESS "))
                print("\n".join(line for line in lines if line.startswith(MARKER)))
                break
            if passed_at is not None and time.monotonic() - passed_at > 10:
                raise RuntimeError("confirmed guest did not power off within ten seconds")
            time.sleep(0.2)
        else:
            raise RuntimeError("release boot fixture exceeded QEMU timeout")
    except (RuntimeError, OSError, KeyboardInterrupt) as problem:
        error = str(problem) or "interrupted"
        manifest["result"] = "failed"
        manifest["error"] = error
        if serial.exists():
            print("\n".join(serial.read_text(errors="replace").splitlines()[-100:]))
    finally:
        manifest["duration_seconds"] = round(time.monotonic() - start, 2)
        manifest["qemu_exit_status_before_cleanup"] = process.poll()
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        process.stderr.close()
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    if error is not None:
        raise SystemExit(error)
    print(f"QEMU signed pair PID-1 exec and automatic health confirmation: PASS ({manifest['duration_seconds']}s)")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/release-boot-test"))
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--timeout", type=float, default=240)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout must be positive")
    image, output = args.image_dir.resolve(), args.output.resolve()
    if output in (image, PROJECT, Path("/")):
        parser.error("output must be a separate fixture artifact directory")
    for name in ("manifest.json", "initramfs.cpio.gz", "vmlinuz"):
        if not (image / name).is_file():
            parser.error(f"missing source appliance artifact: {image / name}")
    if not shutil.which(args.qemu):
        parser.error(f"missing QEMU executable: {args.qemu}")
    output.mkdir(parents=True, exist_ok=True)
    # A failed rebuild must never retain an old result or passing serial log.
    for name in ("manifest.json", "qemu-serial.log", "initramfs.cpio.gz"):
        (output / name).unlink(missing_ok=True)
    manifest = build_image(args, image, output)
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    run_guest(args, image, output, manifest)


if __name__ == "__main__":
    main()
