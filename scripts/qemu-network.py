#!/usr/bin/env python3
"""Run native routing, firewall, NAT, DS-Lite, link and HA packet tests in QEMU."""

import argparse
import gzip
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import tempfile
import time


PROJECT = Path(__file__).resolve().parent.parent


def exact_read(stream, size):
    value = stream.read(size)
    if len(value) != size:
        raise ValueError("truncated source initramfs")
    return value


def extract_modules(archive, root):
    """Extract only safe regular module files from our own newc image archive."""
    count = 0
    with gzip.open(archive, "rb") as stream:
        while True:
            header = exact_read(stream, 110)
            if header[:6] != b"070701":
                raise ValueError("source initramfs is not a newc archive")
            values = [int(header[index:index + 8], 16) for index in range(6, 110, 8)]
            mode, size, name_size = values[1], values[6], values[11]
            if not 0 < name_size <= 4096:
                raise ValueError("invalid initramfs path length")
            encoded = exact_read(stream, name_size)
            exact_read(stream, (-(110 + name_size)) % 4)
            if encoded[-1:] != b"\0":
                raise ValueError("invalid initramfs path encoding")
            name = encoded[:-1].decode("utf-8")
            if name == "TRAILER!!!":
                break
            relative = PurePosixPath(name)
            if relative.is_absolute() or ".." in relative.parts:
                raise ValueError("source initramfs contains an unsafe path")
            selected = len(relative.parts) >= 2 and relative.parts[:2] == ("lib", "modules")
            data = exact_read(stream, size)
            exact_read(stream, (-size) % 4)
            if not selected:
                continue
            target = root.joinpath(*relative.parts)
            if stat.S_ISDIR(mode):
                target.mkdir(parents=True, exist_ok=True)
            elif stat.S_ISREG(mode):
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(data)
                target.chmod(stat.S_IMODE(mode))
                count += 1
            elif stat.S_ISLNK(mode):
                # Alpine also puts a convenience vmlinuz symlink here. The
                # native module loader needs regular .ko files only; do not
                # reproduce build/source/kernel links into the test image.
                continue
            else:
                raise ValueError("module archive contains a special file")
    if not count:
        raise ValueError("source image has no kernel modules")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/network-test"))
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--timeout", type=float, default=100)
    args = parser.parse_args()
    image = args.image_dir.resolve()
    output = args.output.resolve()
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing appliance image artifact: {image / name}")
    if not shutil.which(args.qemu):
        parser.error(f"missing QEMU executable: {args.qemu}")
    output.mkdir(parents=True, exist_ok=True)
    source_manifest = json.loads((image / "manifest.json").read_text())
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
    with tempfile.TemporaryDirectory(prefix="routerd2-kernel-test-") as temporary:
        root = Path(temporary)
        for directory in ("dev", "proc", "sys", "run", "tmp", "var/lib/routerd"):
            (root / directory).mkdir(parents=True, exist_ok=True)
        (root / "tmp").chmod(0o1777)
        print("Extracting appliance kernel modules for the isolated test image...", flush=True)
        extract_modules(image / "initramfs.cpio.gz", root)
        print("Building the test-only init and native networking test binary...", flush=True)
        subprocess.run([
            args.go, "build", "-trimpath", "-buildvcs=false", "-o", str(root / "init"),
            "./images/kernel-test-init",
        ], check=True, cwd=PROJECT, env=environment)
        subprocess.run([
            args.go, "test", "-tags", "integration", "-c", "-o", str(root / "network.test"),
            "./internal/network",
        ], check=True, cwd=PROJECT, env=environment)
        initializer = root / "init"
        test_binary = root / "network.test"
        manifest = {
            "schema_version": 1, "purpose": "isolated-native-network-integration",
            "kernel_release": source_manifest["kernel_release"],
            "kernel_sha256": hashlib.sha256((image / "vmlinuz").read_bytes()).hexdigest(),
            "initializer_sha256": hashlib.sha256(initializer.read_bytes()).hexdigest(),
            "test_binary_sha256": hashlib.sha256(test_binary.read_bytes()).hexdigest(),
            "source_image_manifest_sha256": hashlib.sha256((image / "manifest.json").read_bytes()).hexdigest(),
            "host_network_access": False, "result": "not-run",
        }
        spec = importlib.util.spec_from_file_location("mkinitramfs", PROJECT / "images/mkinitramfs.py")
        packer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(packer)
        archive = output / "initramfs.cpio.gz"
        packer.write_archive(root, archive, 0, compresslevel=1)

    log_path = output / "qemu-serial.log"
    # QEMU opens its serial file asynchronously. Never interpret a previous
    # run's PASS/FAIL marker before this process has begun writing.
    log_path.unlink(missing_ok=True)
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "512M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{log_path}",
        "-kernel", str(image / "vmlinuz"), "-initrd", str(archive),
        "-append", "console=ttyS0 panic=-1",
    ]
    print("Booting the network test guest (no external network interface)...", flush=True)
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    started = time.monotonic()
    try:
        while time.monotonic() - started < args.timeout:
            text = log_path.read_text(errors="replace") if log_path.exists() else ""
            if "ROUTERD_NETWORK_TEST_FAIL" in text or "Kernel panic" in text:
                raise RuntimeError("native network test failed in guest")
            if "ROUTERD_NETWORK_TEST_PASS" in text:
                manifest["result"] = "passed"
                manifest["duration_seconds"] = round(time.monotonic() - started, 2)
                print(text[text.find("=== RUN"):])
                print(f"QEMU native routes/ACL/NAT/DS-Lite/links/HA: PASS ({manifest['duration_seconds']}s)")
                break
            if process.poll() is not None:
                raise RuntimeError(f"QEMU exited with status {process.returncode} without a passing test: {process.stderr.read().decode(errors='replace')}")
            time.sleep(0.2)
        else:
            raise RuntimeError("native network test exceeded guest timeout")
    except (RuntimeError, OSError) as error:
        manifest["result"] = "failed"
        manifest["error"] = str(error)
        if log_path.exists():
            log = log_path.read_text(errors="replace")
            start = log.find("=== RUN")
            print(log[start:] if start >= 0 else "\n".join(log.splitlines()[-80:]))
        raise SystemExit(str(error)) from error
    finally:
        manifest["duration_seconds"] = round(time.monotonic() - started, 2)
        manifest["qemu_exit_status_before_cleanup"] = process.poll()
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")


if __name__ == "__main__":
    main()
