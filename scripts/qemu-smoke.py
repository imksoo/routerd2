#!/usr/bin/env python3
"""Boot routerd as PID 1 under a real Linux kernel and optionally query SSH."""

import argparse
import json
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time


def free_port():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--timeout", type=float, default=90)
    parser.add_argument("--ssh-key", type=Path)
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    args = parser.parse_args()
    image = args.image_dir.resolve()
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing image artifact: {image / name}")
    if not shutil.which(args.qemu):
        parser.error(f"missing QEMU executable: {args.qemu}")
    if args.ssh_key and not args.ssh_key.is_file():
        parser.error("SSH private key does not exist")
    manifest = json.loads((image / "manifest.json").read_text())
    if args.ssh_key and not manifest.get("ssh_enabled"):
        parser.error("image has SSH disabled; rebuild with SSH_AUTHORIZED_KEY")

    port = free_port()
    log_path = image / "qemu-serial.log"
    # QEMU opens this file after Popen returns. Stale readiness/panic markers
    # must not satisfy or fail a new boot attempt.
    log_path.unlink(missing_ok=True)
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "512M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-serial", f"file:{log_path}",
        "-kernel", str(image / "vmlinuz"),
        "-initrd", str(image / "initramfs.cpio.gz"),
        "-append", "console=ttyS0,115200 panic=-1",
        "-netdev", f"user,id=wan,hostfwd=tcp:127.0.0.1:{port}-:22",
        "-device", "virtio-net-pci,netdev=wan,mac=52:54:00:12:34:56",
    ]
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    started = time.monotonic()
    try:
        while time.monotonic() - started < args.timeout:
            if process.poll() is not None:
                raise RuntimeError(f"QEMU exited early: {process.stderr.read().decode(errors='replace')}")
            output = log_path.read_text(errors="replace") if log_path.exists() else ""
            if "Kernel panic" in output:
                raise RuntimeError("guest kernel panicked")
            if "ROUTERD_READY" in output:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("guest did not report ROUTERD_READY before timeout")

        if args.ssh_key:
            with tempfile.TemporaryDirectory(prefix="routerd2-ssh-") as temp:
                base = [
                    "ssh", "-F", "/dev/null", "-i", str(args.ssh_key.resolve()),
                    "-p", str(port), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
                    "-o", "StrictHostKeyChecking=accept-new",
                    "-o", f"UserKnownHostsFile={temp}/known_hosts",
                    "-o", "ConnectTimeout=2", "admin@127.0.0.1",
                ]
                # Readiness and SSH listen happen close together; retry connection
                # setup while keeping one overall smoke-test deadline.
                while time.monotonic() - started < args.timeout:
                    result = subprocess.run(base + ["show status"], capture_output=True, text=True, timeout=5)
                    if result.returncode == 0:
                        status = json.loads(result.stdout)
                        break
                    time.sleep(0.2)
                else:
                    raise RuntimeError(f"SSH status failed: {result.stderr}")
                if not status.get("ready") or status.get("pid") != 1 or status.get("platform") != "appliance":
                    raise RuntimeError(f"unexpected supervisor state: {status}")
                if status.get("release_id") != manifest["release_id"]:
                    raise RuntimeError("supervisor release does not match the image manifest")
                previous_network = next(module for module in status["modules"] if module["name"] == "network")
                for module in status["modules"]:
                    if module["required"] and (not module["ready"] or module["release_id"] != manifest["release_id"]):
                        raise RuntimeError(f"required worker is not ready for this release: {module}")

                def query_json(query):
                    result = subprocess.run(base + [query], capture_output=True, text=True, timeout=5)
                    if result.returncode:
                        raise RuntimeError(f"{query} failed: {result.stderr}")
                    value = json.loads(result.stdout)
                    print(f"SSH {query}: OK")
                    return value

                network = query_json("show network")
                if network.get("status") != "applied" or network.get("observation_errors"):
                    raise RuntimeError(f"network did not reconcile cleanly: {network}")
                interface = next((item for item in network.get("interfaces", []) if item["name"] == "eth0"), None)
                if not interface or not interface["up"] or not any(
                    address["address"] == "10.0.2.15/24" for address in interface.get("addresses", [])
                ):
                    raise RuntimeError("QEMU eth0 did not acquire its configured address")
                config = query_json("show config")
                version = query_json("show version")
                if not config.get("healthy") or version.get("release_id") != manifest["release_id"]:
                    raise RuntimeError("configuration or version did not match the active release")

                query_json("restart network")
                while time.monotonic() - started < args.timeout:
                    result = subprocess.run(base + ["show status"], capture_output=True, text=True, timeout=5)
                    if result.returncode == 0:
                        restarted = json.loads(result.stdout)
                        module = next(item for item in restarted["modules"] if item["name"] == "network")
                        if restarted["ready"] and module["generation"] > previous_network["generation"]:
                            break
                    time.sleep(0.2)
                else:
                    raise RuntimeError("network worker did not recover after restart")
                recovered = query_json("show network")
                if recovered["revision"] != network["revision"] or recovered["status"] != "applied":
                    raise RuntimeError("network worker did not recover the committed revision")
                print("SSH network worker restart/recovery: OK")
        print(f"QEMU Linux/PID1 smoke: OK ({time.monotonic() - started:.1f}s)")
    except (RuntimeError, subprocess.TimeoutExpired, json.JSONDecodeError) as error:
        if log_path.exists():
            print(log_path.read_text(errors="replace"))
        raise SystemExit(str(error)) from error
    finally:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


if __name__ == "__main__":
    main()
