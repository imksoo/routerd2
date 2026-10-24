#!/usr/bin/env python3
# SPDX-License-Identifier: BSD-3-Clause
"""Cut a live Health queue and registry ACK, then recover on three real boots."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import uuid


PROJECT = Path(__file__).resolve().parent.parent
MARKER = "ROUTERD_HEALTH_POWER"
IDENTITY = "health-power-gate"
PHASES = ("produce", "registry", "recovered")


def load_helper(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    helper = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(helper)
    return helper


def require_uuid(value):
    try:
        valid = isinstance(value, str) and str(uuid.UUID(value)) == value
    except ValueError:
        valid = False
    if not valid:
        raise RuntimeError("missing canonical kernel boot ID")


def validate_witness(w, phase):
    if not isinstance(w, dict) or w.get("phase") != phase:
        raise RuntimeError("wrong power-cut witness phase")
    require_uuid(w.get("boot_id"))
    require_uuid(w.get("origin_boot_id"))
    if w.get("pid") != 1 or w.get("exec_verified") is not True or w.get("filesystem") != "ext4":
        raise RuntimeError("witness did not verify actual product PID 1 and ext4")
    for key, size in (("original_supervisor", 32), ("original_epoch", 32), ("original_digest", 64)):
        if not isinstance(w.get(key), str) or not re.fullmatch(f"[0-9a-f]{{{size}}}", w[key]):
            raise RuntimeError("original authority is not pinned")
    if type(w.get("original_generation")) is not int or w["original_generation"] <= 0:
        raise RuntimeError("missing original worker generation")
    expected = {
        "produce": (1, 1, 0, True, True),
        "registry": (2, 1, 1, False, True),
        "recovered": (2, 0, 0, False, False),
    }[phase]
    for key, value in zip(("revision", "registered", "retiring", "health_worker", "health_error"), expected):
        if type(w.get(key)) is not type(value) or w[key] != value:
            raise RuntimeError(f"incorrect {phase} {key}")
    items = w.get("observations")
    if not isinstance(items, list) or not 2 <= len(items) <= 64:
        raise RuntimeError("incomplete or unbounded accepted queue witness")
    ids, cursors = set(), set()
    for item in items:
        if not isinstance(item, dict) or not isinstance(item.get("id"), str) or not re.fullmatch(r"health/wan/[0-9a-f]{32}", item["id"]):
            raise RuntimeError("invalid original observation ID")
        if not isinstance(item.get("sha256"), str) or not re.fullmatch(r"[0-9a-f]{64}", item["sha256"]):
            raise RuntimeError("invalid full-content hash")
        cursor = item.get("cursor")
        if type(cursor) is not int or cursor < 0 or (phase == "produce") != (cursor == 0):
            raise RuntimeError("invalid original publication cursor")
        if item["id"] in ids or cursor and cursor in cursors:
            raise RuntimeError("duplicate observation identity or cursor")
        ids.add(item["id"])
        cursors.add(cursor)
    pending = len(items) if phase == "produce" else 0
    if type(w.get("pending_count")) is not int or w["pending_count"] != pending:
        raise RuntimeError("pending local ACK does not match the cut point")
    return {item["id"]: (item["sha256"], item["cursor"]) for item in items}


def parse_witness(log, phase, cut):
    if MARKER + "_FAIL" in log or "Kernel panic" in log:
        raise RuntimeError("guest reported failure")
    if cut and ("reboot: Power down" in log or "Preparing to enter system sleep" in log or MARKER + "_PASS " in log):
        raise RuntimeError("guest began an orderly shutdown before the cut")
    prefix = MARKER + ("_CUT " if cut else "_PASS ")
    lines = [line.strip() for line in log.splitlines()]
    matches = [line[len(prefix):] for line in lines if line.startswith(prefix)]
    if len(matches) != 1:
        raise RuntimeError("one exact power-cut witness is required")
    try:
        witness = json.loads(matches[0])
    except ValueError as error:
        raise RuntimeError("malformed power-cut witness") from error
    validate_witness(witness, phase)
    return witness


def has_complete_marker(log, prefix):
    return any(line.startswith(prefix) and line.endswith("\n") for line in log.splitlines(keepends=True))


def check_phases(phases):
    if len(phases) != 3:
        raise RuntimeError("three actual kernel boots are required")
    maps, witnesses = [], []
    for result, phase in zip(phases, PHASES):
        if result.get("phase") != phase or result.get("result") != "passed":
            raise RuntimeError("wrong kernel boot order")
        termination, status = ("SIGKILL", -9) if phase != "recovered" else ("poweroff", 0)
        if result.get("termination") != termination or result.get("qemu_exit_status") != status:
            raise RuntimeError("required abrupt cut or completed recovery did not occur")
        w = result["witness"]
        maps.append(validate_witness(w, phase))
        witnesses.append(w)
    first, second, third = witnesses
    if len({w["boot_id"] for w in witnesses}) != 3 or first["origin_boot_id"] != first["boot_id"]:
        raise RuntimeError("boots are not distinct or origin is wrong")
    for w in witnesses[1:]:
        for key in ("origin_boot_id", "original_supervisor", "original_epoch", "original_generation", "original_digest"):
            if w[key] != first[key]:
                raise RuntimeError("original authority changed across cuts")
    for event_id, (body_hash, _) in maps[0].items():
        if event_id not in maps[1] or maps[1][event_id][0] != body_hash:
            raise RuntimeError("first cut lost or changed accepted history")
    if maps[1] != maps[2]:
        raise RuntimeError("second cut lost, changed, duplicated or recursored acknowledged history")


def build_guest(args, temporary, storage, common):
    image = args.image_dir.resolve()
    root = temporary / "root"
    root.mkdir()
    for directory in ("dev", "proc", "sys", "run", "tmp", "etc/routerd", "var/lib/routerd", "test/bin"):
        (root / directory).mkdir(parents=True, exist_ok=True)
    (root / "tmp").chmod(0o1777)
    source = temporary / "source.cpio.gz"
    kernel = temporary / "vmlinuz"
    shutil.copyfile(image / "initramfs.cpio.gz", source)
    shutil.copyfile(image / "vmlinuz", kernel)
    common.extract_modules(source, root)
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64", GOAMD64="v1", GOFLAGS="")
    print("Building dedicated power-cut init/probe and fresh actual command pair...", flush=True)
    subprocess.run([args.go, "build", "-trimpath", "-buildvcs=false", "-ldflags", "-s -w", "-o", str(root / "init"), "./images/health-power-test/guest"], cwd=PROJECT, env=environment, check=True)
    hashes = {}
    for name in ("routerd", "routerd-modules"):
        path = root / "test/bin" / name
        subprocess.run([args.go, "build", "-trimpath", "-buildvcs=false", "-ldflags", f"-s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:{IDENTITY}:END", "-o", str(path), f"./cmd/{name}"], cwd=PROJECT, env=environment, check=True)
        hashes[f"/test/bin/{name}"] = storage.sha256(path)
    manifest = {
        "schema_version": 1, "purpose": "health-queue-three-boot-two-SIGKILL-recovery",
        "product_commit": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=PROJECT, text=True).strip(),
        "initializer_sha256": storage.sha256(root / "init"), "binary_sha256": hashes,
        "kernel_sha256": storage.sha256(kernel), "source_initramfs_sha256": storage.sha256(source),
        "source_image_manifest_sha256": storage.sha256(image / "manifest.json"),
        "guest_memory_mib": 1024, "guest_vcpus": 2, "disk_bytes": storage.DISK_BYTES,
        "state_persistence": "ext4-virtio-block-regular-file", "host_block_devices_used": False,
        "host_filesystem_mounts_used": False, "guest_external_network_interfaces": False,
        "power_loss_scope": "guest VM process/device memory only; host filesystem and physical power loss are not emulated",
        "witness_transport": "UART to host, then test-only initramfs file; no guest witness fsync at cut points",
        "phases": [], "result": "not-run",
    }
    return root, kernel, manifest


def run_boot(args, phase, kernel, archive, fd, temporary, storage, result):
    storage.require_regular_disk(fd)
    serial = temporary / f"qemu-{phase}.log"
    command = [args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max", "-m", "1024M", "-smp", "2", "-display", "none", "-monitor", "none", "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{serial}", "-kernel", str(kernel), "-initrd", str(archive), "-drive", f"if=none,id=state,format=raw,file=/proc/self/fd/{fd},cache=writeback", "-device", "virtio-blk-pci,drive=state", "-append", f"console=ttyS0,115200 panic=-1 routerd2_health_power_fixture=1 routerd2_health_power_phase={phase}"]
    print(f"Booting {phase} on the same private disk (no NIC)...", flush=True)
    process = subprocess.Popen(command, pass_fds=(fd,), stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    started = time.monotonic()
    cut = phase != "recovered"
    prefix = MARKER + ("_CUT " if cut else "_PASS ")
    try:
        while time.monotonic() - started < args.timeout:
            log = serial.read_text(errors="replace") if serial.exists() else ""
            if MARKER + "_FAIL" in log or "Kernel panic" in log:
                raise RuntimeError(f"{phase} guest failed")
            if cut and has_complete_marker(log, prefix):
                witness = parse_witness(log, phase, True)
                if process.poll() is not None:
                    raise RuntimeError("QEMU exited before the host could cut it")
                process.kill()
                status = process.wait(timeout=5)
                final_log = serial.read_text(errors="replace")
                parse_witness(final_log, phase, True)
                if status != -9:
                    raise RuntimeError("QEMU was not actually killed by SIGKILL")
                result.update(result="passed", termination="SIGKILL", qemu_exit_status=status, witness=witness)
                return result
            status = process.poll()
            if status is not None:
                log = serial.read_text(errors="replace") if serial.exists() else ""
                if cut or status != 0:
                    raise RuntimeError(f"{phase} exited {status} without required abrupt cut")
                result.update(result="passed", termination="poweroff", qemu_exit_status=status, witness=parse_witness(log, phase, False))
                return result
            time.sleep(0.05)
        raise RuntimeError(f"{phase} timed out")
    except (OSError, RuntimeError, ValueError, subprocess.TimeoutExpired, KeyboardInterrupt) as error:
        result.update(result="failed", error=str(error) or "interrupted")
        raise
    finally:
        result["duration_seconds"] = round(time.monotonic() - started, 2)
        result["qemu_exit_status_before_cleanup"] = process.poll()
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        process.stderr.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/health-power-test"))
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--mke2fs", default="mke2fs")
    parser.add_argument("--timeout", type=float, default=210)
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout must be positive")
    image, output = args.image_dir.resolve(), args.output.resolve()
    if output in (image, PROJECT, Path("/")):
        parser.error("output must be a separate artifact directory")
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing appliance input {image / name}")
    for executable in (args.qemu, args.mke2fs):
        if not shutil.which(executable):
            parser.error(f"missing executable {executable}")
    storage = load_helper("health_power_storage", PROJECT / "scripts/qemu-storage.py")
    common = load_helper("health_power_common", PROJECT / "scripts/qemu-network.py")
    packer = load_helper("health_power_packer", PROJECT / "images/mkinitramfs.py")
    output.mkdir(parents=True, exist_ok=True)
    for name in ("manifest.json", "data.ext4", *(f"qemu-{p}.log" for p in PHASES), *(f"initramfs-{p}.cpio.gz" for p in PHASES)):
        (output / name).unlink(missing_ok=True)
    with tempfile.TemporaryDirectory(prefix="routerd2-health-power-") as path:
        temporary = Path(path)
        root, kernel, manifest = build_guest(args, temporary, storage, common)
        disk = temporary / "data.ext4"
        fd = os.open(disk, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW, 0o600)
        try:
            os.ftruncate(fd, storage.DISK_BYTES)
            storage.require_regular_disk(fd)
            subprocess.run([args.mke2fs, "-t", "ext4", "-q", "-F", "-L", "routerd-data", "-E", "lazy_itable_init=0,lazy_journal_init=0", f"/proc/self/fd/{fd}"], pass_fds=(fd,), check=True)
            os.fsync(fd)  # New empty filesystem only, before any accepted probe.
            for phase in PHASES:
                if manifest["phases"]:
                    (root / "test/expected.json").write_text(json.dumps(manifest["phases"][-1]["witness"]))
                archive = temporary / f"initramfs-{phase}.cpio.gz"
                packer.write_archive(root, archive, 0, compresslevel=1)
                storage.verify_gzip(archive)
                digest = storage.sha256(archive)
                result = {"phase": phase, "result": "not-run"}
                manifest["phases"].append(result)
                run_boot(args, phase, kernel, archive, fd, temporary, storage, result)
                result["initramfs_sha256"] = digest
                if storage.sha256(archive) != digest or storage.sha256(kernel) != manifest["kernel_sha256"]:
                    raise RuntimeError("private boot inputs changed during a live phase")
                # No host fsync or guest pre-mount is inserted between cuts.
                result["disk_sha256_after_boot"] = storage.sha256(disk)
            check_phases(manifest["phases"])
            manifest["result"] = "passed"
        except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError, subprocess.TimeoutExpired, KeyboardInterrupt) as error:
            manifest.update(result="failed", error=str(error) or "interrupted")
            for log in sorted(temporary.glob("qemu-*.log")):
                print(log.name + ":\n" + "\n".join(log.read_text(errors="replace").splitlines()[-65:]))
        finally:
            os.close(fd)
            manifest["final_disk_sha256"] = storage.sha256(disk)
            for artifact in [disk, *temporary.glob("qemu-*.log"), *temporary.glob("initramfs-*.cpio.gz")]:
                common.publish_artifact(artifact, output / artifact.name)
            description = temporary / "manifest.json"
            description.write_text(json.dumps(manifest, indent=2) + "\n")
            common.publish_artifact(description, output / "manifest.json")
        if manifest["result"] != "passed":
            raise SystemExit(manifest.get("error", "power-cut gate failed"))
        print(f"{MARKER}: PASS ({sum(p['duration_seconds'] for p in manifest['phases']):.2f}s)")


if __name__ == "__main__":
    main()
