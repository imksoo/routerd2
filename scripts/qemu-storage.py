#!/usr/bin/env python3
# SPDX-License-Identifier: BSD-3-Clause
"""Verify persistent appliance state and signed fallback across two real boots."""

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
import time
import uuid


PROJECT = Path(__file__).resolve().parent.parent
MARKER = "ROUTERD_STORAGE_TEST"
DISK_BYTES = 256 * 1024 * 1024
IDENTITIES = {"baseline": "storage-gate-baseline", "trial": "storage-gate-trial"}


def load_helper(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    return helper


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def verify_gzip(path):
    with gzip.open(path, "rb") as source:
        while source.read(1024 * 1024):
            pass


def require_regular_disk(fd):
    metadata = os.fstat(fd)
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_size != DISK_BYTES:
        raise RuntimeError("fixture disk must be the newly created bounded regular file")
    if metadata.st_uid != os.geteuid() or stat.S_IMODE(metadata.st_mode) != 0o600 or metadata.st_nlink != 1:
        raise RuntimeError("fixture disk ownership, permissions or link count changed")


def build_guest(args, temporary, common):
    image = args.image_dir.resolve()
    root = temporary / "root"
    root.mkdir()
    for directory in ("dev", "proc", "sys", "run", "tmp", "etc/routerd", "var/lib/routerd", "test"):
        (root / directory).mkdir(parents=True, exist_ok=True)
    (root / "tmp").chmod(0o1777)
    source_archive = temporary / "source-initramfs.cpio.gz"
    kernel = temporary / "vmlinuz"
    shutil.copyfile(image / "initramfs.cpio.gz", source_archive)
    shutil.copyfile(image / "vmlinuz", kernel)
    source_manifest = (image / "manifest.json").read_bytes()
    kernel_manifest = json.loads(source_manifest)
    print("Copying appliance modules for the two-boot storage guest...", flush=True)
    common.extract_modules(source_archive, root)
    module_names = {p.name.removesuffix(".gz").removesuffix(".xz").removesuffix(".zst") for p in (root / "lib/modules").rglob("*.ko*")}
    for module in ("virtio_blk.ko", "ext4.ko", "jbd2.ko", "mbcache.ko", "crc16.ko"):
        if module not in module_names:
            raise RuntimeError(f"source kernel is missing required storage module {module}")
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64",
                       GOAMD64="v1", GOFLAGS="")
    # Keep the project-selected toolchain, even when the system go launcher is
    # older and relies on Go's automatic toolchain download/cache.
    go_root = subprocess.check_output([args.go, "env", "GOROOT"], cwd=PROJECT, env=environment, text=True).strip()
    go_binary = str(Path(go_root) / "bin/go")
    environment["GOTOOLCHAIN"] = "local"
    print("Building the guest initializer and fresh signed command pairs...", flush=True)
    subprocess.run([
        go_binary, "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
        "-ldflags", "-s -w", "-o", str(root / "init"), "./images/storage-test/guest",
    ], check=True, cwd=PROJECT, env=environment)
    hashes = {}
    for directory, identity in IDENTITIES.items():
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
    archive = temporary / "initramfs.cpio.gz"
    packer = load_helper("routerd_storage_packer", PROJECT / "images/mkinitramfs.py")
    packer.write_archive(root, archive, 0, compresslevel=1)
    verify_gzip(archive)
    manifest = {
        "schema_version": 1, "purpose": "persistent-state-and-signed-userland-two-boot-fallback",
        "go": subprocess.check_output([go_binary, "version"], env=environment, text=True).strip(),
        "target": "linux/amd64", "kernel_release": kernel_manifest["kernel_release"],
        "kernel_sha256": sha256(kernel), "initramfs_sha256": sha256(archive),
        "source_image_manifest_sha256": hashlib.sha256(source_manifest).hexdigest(),
        "source_initramfs_sha256": sha256(source_archive),
        "initializer_sha256": sha256(root / "init"), "binary_sha256": hashes,
        "release_ids": IDENTITIES, "state_persistence": "ext4-virtio-block-regular-file",
        "disk_bytes": DISK_BYTES, "host_block_devices_used": False,
        "host_filesystem_mounts_used": False, "guest_external_network_interfaces": False,
        "kernel_rootfs_updates": False, "two_boot_disk_fallback_tested": False,
        "phases": [], "result": "not-run",
    }
    return kernel, archive, manifest


def run_boot(args, phase, kernel, archive, disk_fd, temporary):
    require_regular_disk(disk_fd)
    serial = temporary / f"qemu-{phase}.log"
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "1024M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{serial}",
        "-kernel", str(kernel), "-initrd", str(archive),
        "-drive", f"if=none,id=state,format=raw,file=/proc/self/fd/{disk_fd},cache=writeback",
        "-device", "virtio-blk-pci,drive=state",
        "-append", f"console=ttyS0,115200 panic=-1 routerd2_storage_fixture=1 routerd2_storage_phase={phase}",
    ]
    print(f"Booting storage phase {phase} (no NIC, one private regular-file disk)...", flush=True)
    process = subprocess.Popen(command, pass_fds=(disk_fd,), stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    start = time.monotonic()
    result = {"phase": phase, "result": "not-run"}
    prefix = f"{MARKER}_WITNESS "
    expected = f"{MARKER}_{phase.upper()}_PASS"
    try:
        while time.monotonic() - start < args.timeout:
            log = serial.read_text(errors="replace") if serial.exists() else ""
            if MARKER + "_FAIL" in log or "Kernel panic" in log:
                raise RuntimeError(f"storage phase {phase} failed in guest")
            status = process.poll()
            if status is not None:
                log = serial.read_text(errors="replace") if serial.exists() else ""
                lines = [line.strip() for line in log.splitlines()]
                witnesses = [json.loads(line[len(prefix):]) for line in lines if line.startswith(prefix)]
                if status != 0 or expected not in lines or len(witnesses) != 1 or witnesses[0].get("phase") != phase:
                    detail = process.stderr.read().decode(errors="replace")
                    raise RuntimeError(f"storage phase {phase} exited {status} without its exact marker and witness: {detail}")
                if MARKER + "_FAIL" in log or "Kernel panic" in log:
                    raise RuntimeError(f"storage phase {phase} reported failure before exiting")
                result["result"], result["witness"] = "passed", witnesses[0]
                print("\n".join(line for line in lines if line.startswith(MARKER)), flush=True)
                return result
            time.sleep(0.2)
        raise RuntimeError(f"storage phase {phase} exceeded its guest timeout")
    finally:
        result["duration_seconds"] = round(time.monotonic() - start, 2)
        result["qemu_exit_status_before_cleanup"] = process.poll()
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        process.stderr.close()


def check_witnesses(phases):
    first, second = (phase["witness"] for phase in phases)
    for witness in (first, second):
        value = witness.get("boot_id")
        try:
            valid = isinstance(value, str) and str(uuid.UUID(value)) == value
        except ValueError:
            valid = False
        if not valid:
            raise RuntimeError("each kernel boot must provide a canonical UUID boot ID")
    if first["boot_id"] == second["boot_id"]:
        raise RuntimeError("the two kernel boots did not produce distinct boot IDs")
    for witness in (first, second):
        if witness.get("pid") != 1 or witness.get("revision") != 2 or not witness.get("pinned_exec_verified"):
            raise RuntimeError("guest witness did not verify actual PID 1, committed config and pinned executables")
    if first.get("release_id") != IDENTITIES["trial"] or first.get("release_operation") != "begin-boot" or not first.get("pending"):
        raise RuntimeError("first boot did not leave a real unconfirmed trial")
    if second.get("release_id") != IDENTITIES["baseline"] or second.get("release_operation") != "boot-rollback" or second.get("pending"):
        raise RuntimeError("second boot did not fall back to the baseline")
    if second.get("failed_boot_id") != first["boot_id"]:
        raise RuntimeError("fallback was not caused by the first boot's unconfirmed trial")
    for field in ("config_sha256", "host_key_sha256", "host_key_fingerprint", "sentinel_sha256"):
        if not first.get(field) or first[field] != second.get(field):
            raise RuntimeError(f"persistent {field} changed across boots")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/storage-test"))
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--mke2fs", default="mke2fs")
    parser.add_argument("--timeout", type=float, default=240)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout must be positive")
    image, output = args.image_dir.resolve(), args.output.resolve()
    if output in (image, PROJECT, Path("/")):
        parser.error("output must be a separate artifact directory")
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing appliance image artifact: {image / name}")
    for name in (args.qemu, args.mke2fs):
        if not shutil.which(name):
            parser.error(f"required executable is unavailable: {name}")
    common = load_helper("routerd_storage_common", PROJECT / "scripts/qemu-network.py")
    output.mkdir(parents=True, exist_ok=True)
    for name in ("manifest.json", "initramfs.cpio.gz", "qemu-first.log", "qemu-second.log", "data.ext4"):
        (output / name).unlink(missing_ok=True)
    with tempfile.TemporaryDirectory(prefix="routerd2-storage-test-") as temporary_path:
        temporary = Path(temporary_path)
        kernel, archive, manifest = build_guest(args, temporary, common)
        disk = temporary / "data.ext4"
        disk_fd = os.open(disk, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
        try:
            os.ftruncate(disk_fd, DISK_BYTES)
            require_regular_disk(disk_fd)
            version = subprocess.run([args.mke2fs, "-V"], capture_output=True, text=True, check=True)
            manifest["mke2fs_version"] = (version.stdout + version.stderr).strip()
            print("Formatting only the newly created private regular-file disk...", flush=True)
            subprocess.run([
                args.mke2fs, "-t", "ext4", "-q", "-F", "-L", "routerd-data",
                "-E", "lazy_itable_init=0,lazy_journal_init=0", f"/proc/self/fd/{disk_fd}",
            ], check=True, pass_fds=(disk_fd,))
            os.fsync(disk_fd)
            manifest["initial_disk_sha256"] = sha256(disk)
            for phase in ("first", "second"):
                result = run_boot(args, phase, kernel, archive, disk_fd, temporary)
                manifest["phases"].append(result)
                os.fsync(disk_fd)
                result["disk_sha256_after_boot"] = sha256(disk)
                if sha256(archive) != manifest["initramfs_sha256"] or sha256(kernel) != manifest["kernel_sha256"]:
                    raise RuntimeError("private boot inputs changed during the fixture")
            check_witnesses(manifest["phases"])
            manifest["result"], manifest["two_boot_disk_fallback_tested"] = "passed", True
        except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError, KeyboardInterrupt) as error:
            manifest["result"], manifest["error"] = "failed", str(error) or "interrupted"
            for path in sorted(temporary.glob("qemu-*.log")):
                print(f"{path.name}:\n" + "\n".join(path.read_text(errors="replace").splitlines()[-100:]))
        finally:
            os.close(disk_fd)
            manifest["final_disk_sha256"] = sha256(disk)
            for name in ("initramfs.cpio.gz", "qemu-first.log", "qemu-second.log", "data.ext4"):
                if (temporary / name).is_file():
                    common.publish_artifact(temporary / name, output / name)
            private_manifest = temporary / "manifest.json"
            private_manifest.write_text(json.dumps(manifest, indent=2) + "\n")
            common.publish_artifact(private_manifest, output / "manifest.json")
        if manifest["result"] != "passed":
            raise SystemExit(manifest.get("error", "storage gate failed"))
        duration = sum(phase["duration_seconds"] for phase in manifest["phases"])
        print(f"QEMU persistent config/SSH state and two-boot signed fallback: PASS ({duration:.2f}s)")


if __name__ == "__main__":
    main()
