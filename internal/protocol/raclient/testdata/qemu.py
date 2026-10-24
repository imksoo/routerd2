#!/usr/bin/env python3
"""Run native RA/RS integration inside an Alpine-kernel guest with no external NIC."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

PROJECT = Path(__file__).resolve().parents[4]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=PROJECT / "dist/image")
    parser.add_argument("--output", type=Path, default=PROJECT / "dist/ra-client-test")
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--timeout", type=float, default=90)
    args = parser.parse_args()
    image, output = args.image_dir.resolve(), args.output.resolve()
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing appliance artifact: {image / name}")
    if not shutil.which(args.qemu):
        parser.error(f"missing QEMU executable: {args.qemu}")
    output.mkdir(parents=True, exist_ok=True)
    source = json.loads((image / "manifest.json").read_text())
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
    extractor = module("network_gate", PROJECT / "scripts/qemu-network.py")
    packer = module("initramfs", PROJECT / "images/mkinitramfs.py")
    with tempfile.TemporaryDirectory(prefix="routerd2-raclient-test-") as temporary:
        root = Path(temporary)
        for name in ("dev", "proc", "sys", "run", "tmp", "var/lib/routerd"):
            (root / name).mkdir(parents=True, exist_ok=True)
        (root / "tmp").chmod(0o1777)
        extractor.extract_modules(image / "initramfs.cpio.gz", root)
        subprocess.run([
            args.go, "build", "-trimpath", "-buildvcs=false", "-o", str(root / "init"),
            "./internal/protocol/raclient/testdata/qemu-init",
        ], check=True, cwd=PROJECT, env=env)
        subprocess.run([
            args.go, "test", "-tags", "integration", "-c", "-o", str(root / "raclient.test"),
            "./internal/protocol/raclient",
        ], check=True, cwd=PROJECT, env=env)
        manifest = {
            "schema_version": 1,
            "purpose": "isolated-native-ra-client",
            "kernel_release": source["kernel_release"],
            "kernel_sha256": digest(image / "vmlinuz"),
            "init_sha256": digest(root / "init"),
            "test_sha256": digest(root / "raclient.test"),
            "host_network_access": False,
            "result": "not-run",
        }
        packer.write_archive(root, output / "initramfs.cpio.gz", 0, compresslevel=1)
    log = output / "qemu-serial.log"
    log.unlink(missing_ok=True)
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "512M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{log}",
        "-kernel", str(image / "vmlinuz"), "-initrd", str(output / "initramfs.cpio.gz"),
        "-append", "console=ttyS0,115200 panic=-1",
    ]
    print("Booting isolated native RA client gate (-nic none)...", flush=True)
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    started = time.monotonic()
    try:
        while time.monotonic() - started < args.timeout:
            content = log.read_text(errors="replace") if log.exists() else ""
            if "ROUTERD_RA_CLIENT_TEST_PASS" in content:
                manifest["result"] = "pass"
                break
            if "ROUTERD_RA_CLIENT_TEST_FAIL" in content or process.poll() is not None:
                manifest["result"] = "fail"
                break
            time.sleep(0.2)
        else:
            manifest["result"] = "timeout"
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
    manifest["elapsed_seconds"] = round(time.monotonic() - started, 3)
    if log.exists():
        manifest["serial_sha256"] = digest(log)
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(json.dumps(manifest, indent=2))
    if manifest["result"] != "pass":
        print(log.read_text(errors="replace")[-16000:] if log.exists() else "no serial output")
        raise SystemExit(1)


if __name__ == "__main__":
    main()
