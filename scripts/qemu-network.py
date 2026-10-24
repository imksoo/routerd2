#!/usr/bin/env python3
"""Run a fixed native-network or two-binary daemon test suite in isolated QEMU."""

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
SUITES = {
    "bfd": {
        "package": "./internal/protocol/bfd", "binary": "bfd.test",
        "test": "TestIsolatedNativeBFD",
        "marker": "ROUTERD_BFD_NATIVE_TEST", "timeout": 100,
        "description": "native IPv4 and IPv6 BFD sessions and failure detection",
    },
    "wireguard": {
        "package": "./internal/network", "binary": "network.test",
        "test": "TestIsolatedKernelWireGuardOwnershipAndTraffic",
        "marker": "ROUTERD_WIREGUARD_TEST", "timeout": 120,
        "description": "native WireGuard encrypted traffic, ownership and recovery",
    },
    "network": {
        "package": "./internal/network", "binary": "network.test",
        "test": "TestIsolatedKernelForwardingNATAndDSLite",
        "marker": "ROUTERD_NETWORK_TEST", "timeout": 100,
        "description": "native routes/ACL/NAT/DS-Lite/links/HA/RA expiry",
    },
    "ra-dns": {
        "package": "./internal/protocol/ra", "binary": "ra.test",
        "test": "TestIsolatedLearnedDNSLifetimes",
        "marker": "ROUTERD_RA_DNS_TEST", "timeout": 75,
        "description": "RA learned DNS lifetimes and live updates",
    },
    "daemon": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonRAClientConfigurationAndExpiry",
        "marker": "ROUTERD_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "two-binary daemon RA configuration and expiry",
    },
    "daemon-aftr": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonAFTRResolutionAndExpiry",
        "marker": "ROUTERD_AFTR_DAEMON_TEST", "timeout": 180,
        "commands": True, "output": "dist/aftr-daemon-test",
        "description": "two-binary daemon AFTR resolution and expiry",
    },
    "daemon-management": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonConfigurationAndRecovery",
        "marker": "ROUTERD_MANAGEMENT_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "two-binary daemon SSH configuration and recovery",
    },
    "daemon-eventrule": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonEventRuleDurabilityAndLifecycle",
        "marker": "ROUTERD_EVENTRULE_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "durable EventRule count/timer, worker recovery and source lifecycle",
    },
    "daemon-derived": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonDerivedHealthCurrentStatusAndDurability",
        "marker": "ROUTERD_DERIVED_DAEMON_TEST", "timeout": 200,
        "commands": True,
        "description": "finite health status to durable DerivedEvent/EventRule and recovery",
    },
    "daemon-resource-program": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonResourceProgramLifecycleAndRecovery",
        "marker": "ROUTERD_RESOURCE_PROGRAM_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "ResourceProgram daemon ownership, traffic, validation and recovery",
    },
    "daemon-health-pbr": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonHealthPBRFailoverAndRecovery",
        "marker": "ROUTERD_HEALTH_PBR_DAEMON_TEST", "timeout": 240,
        "commands": True,
        "description": "two-WAN health/PBR TCP/UDP failover, marks, capture and worker recovery",
    },
    "daemon-dhcp-dns": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonDHCPDNSAndOwnershipExpiry",
        "marker": "ROUTERD_DHCP_DNS_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "two-binary daemon DHCP-to-DNS and ownership expiry",
    },
    "daemon-legacy-dhcp-dns": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonMigratedDHCPDNSAndOwnershipExpiry",
        "marker": "ROUTERD_LEGACY_DHCP_DNS_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "legacy DHCP/DNS migration through the real two-binary daemon",
    },
    "daemon-bgp": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonBGPFIBAndOwnershipExpiry",
        "marker": "ROUTERD_BGP_DAEMON_TEST", "timeout": 180,
        "commands": True,
        "description": "two-binary daemon BGP FIB and ownership expiry",
    },
    "daemon-bfd": {
        "package": "./internal/integration", "binary": "daemon.test",
        "test": "TestDaemonBFDControlsBGPSessionAndExpiry",
        "marker": "ROUTERD_BFD_DAEMON_TEST", "timeout": 200,
        "commands": True,
        "description": "two-binary daemon BFD-controlled BGP sessions and expiry",
    },
    "dhcp": {
        "package": "./internal/protocol/dhcp4", "binary": "dhcp.test",
        "test": "TestIsolatedNativeDHCPAndRA",
        "marker": "ROUTERD_DHCP_NATIVE_TEST", "timeout": 110,
        "description": "native DHCPv4, DHCPv6-PD and router advertisements",
    },
    "dhcp-server": {
        "package": "./internal/protocol/dhcpserver", "binary": "dhcp-server.test",
        "test": "TestIsolatedNativeDHCPServer",
        "marker": "ROUTERD_DHCP_SERVER_TEST", "timeout": 140,
        "description": "native DHCP server leases and recovery",
    },
    "vrrp": {
        "package": "./internal/protocol/vrrp", "binary": "vrrp.test",
        "test": "TestIsolatedNativeVRRP",
        "marker": "ROUTERD_VRRP_NATIVE_TEST", "timeout": 110,
        "description": "native IPv4 and IPv6 VRRP elections and failover",
    },
    "ra-client": {
        "package": "./internal/protocol/raclient", "binary": "ra-client.test",
        "test": "TestIsolatedNativeRAClient",
        "marker": "ROUTERD_RA_CLIENT_TEST", "timeout": 100,
        "description": "native RA client lifetimes, DAD and restart",
    },
    "aftr": {
        "package": "./internal/protocol/aftr", "binary": "aftr.test",
        "test": "TestIsolatedNativeAFTR",
        "marker": "ROUTERD_AFTR_NATIVE_TEST", "timeout": 90,
        "description": "native AFTR DNS transport and expiry",
    },
}


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
        # Consume gzip's trailer even though cpio has already ended. A source
        # archive with complete module entries but a missing tail must fail.
        while stream.read(1024 * 1024):
            pass
    if not count:
        raise ValueError("source image has no kernel modules")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite", choices=tuple(SUITES), default="network")
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path)
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--timeout", type=float)
    args = parser.parse_args()
    suite = SUITES[args.suite]
    timeout = args.timeout if args.timeout is not None else suite["timeout"]
    if timeout <= 0:
        parser.error("timeout must be positive")
    image = args.image_dir.resolve()
    output = (args.output or Path(suite.get("output", f"dist/{args.suite}-test"))).resolve()
    if image == output:
        parser.error("test output must differ from the source image directory")
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            parser.error(f"missing appliance image artifact: {image / name}")
    if not shutil.which(args.qemu):
        parser.error(f"missing QEMU executable: {args.qemu}")
    output.mkdir(parents=True, exist_ok=True)
    # A build failure must not leave an old passing manifest or serial log
    # looking like the result of the new invocation.
    for name in ("manifest.json", "qemu-serial.log", "initramfs.cpio.gz"):
        (output / name).unlink(missing_ok=True)
    source_manifest_bytes = (image / "manifest.json").read_bytes()
    source_manifest = json.loads(source_manifest_bytes)
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64")
    with tempfile.TemporaryDirectory(prefix="routerd2-kernel-test-") as temporary:
        root = Path(temporary) / "root"
        root.mkdir()
        for directory in ("dev", "proc", "sys", "run", "tmp", "var/lib/routerd"):
            (root / directory).mkdir(parents=True, exist_ok=True)
        (root / "tmp").chmod(0o1777)
        (root / "test-suite").write_text(args.suite + "\n")
        print("Extracting appliance kernel modules for the isolated test image...", flush=True)
        source_archive = Path(temporary) / "source-initramfs.cpio.gz"
        shutil.copyfile(image / "initramfs.cpio.gz", source_archive)
        extract_modules(source_archive, root)
        print(f"Building the test-only init and {args.suite} test binary...", flush=True)
        subprocess.run([
            args.go, "build", "-trimpath", "-buildvcs=false", "-o", str(root / "init"),
            "./images/kernel-test-init",
        ], check=True, cwd=PROJECT, env=environment)
        subprocess.run([
            args.go, "test", "-tags", "integration", "-c", "-o", str(root / suite["binary"]),
            suite["package"],
        ], check=True, cwd=PROJECT, env=environment)
        binary_hashes = {}
        if suite.get("commands"):
            binary_dir = root / "test/bin"
            binary_dir.mkdir(parents=True)
            for name in ("routerd", "routerd-modules"):
                binary = binary_dir / name
                subprocess.run([
                    args.go, "build", "-trimpath", "-buildvcs=false",
                    "-ldflags", "-s -w -X github.com/imksoo/routerd2/internal/buildinfo.Identity=ROUTERD2-RELEASE-V1:qemu-daemon-test:END",
                    "-o", str(binary), f"./cmd/{name}",
                ], check=True, cwd=PROJECT, env=environment)
                binary_hashes[f"/test/bin/{name}"] = hashlib.sha256(binary.read_bytes()).hexdigest()
        initializer = root / "init"
        test_binary = root / suite["binary"]
        manifest = {
            "schema_version": 1, "purpose": f"isolated-{args.suite}-integration",
            "suite": args.suite, "required_test": suite["test"],
            "kernel_release": source_manifest["kernel_release"],
            "kernel_sha256": hashlib.sha256((image / "vmlinuz").read_bytes()).hexdigest(),
            "initializer_sha256": hashlib.sha256(initializer.read_bytes()).hexdigest(),
            "test_binary_sha256": hashlib.sha256(test_binary.read_bytes()).hexdigest(),
            "binary_sha256": binary_hashes,
            "source_image_manifest_sha256": hashlib.sha256(source_manifest_bytes).hexdigest(),
            "source_initramfs_sha256": hashlib.sha256(source_archive.read_bytes()).hexdigest(),
            "host_network_access": False, "result": "not-run",
        }
        spec = importlib.util.spec_from_file_location("mkinitramfs", PROJECT / "images/mkinitramfs.py")
        packer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(packer)
        # Boot inputs and the live serial stream remain in a private directory.
        # Publishing an archive while another workspace reader/synchronizer can
        # observe it produced a truncated gzip in a constrained executor. Keep
        # the complete VM lifetime isolated, then publish finished artifacts.
        archive = Path(temporary) / "initramfs.cpio.gz"
        packer.write_archive(root, archive, 0, compresslevel=1)
        # Read through gzip's trailer/CRC before asking the kernel to unpack it.
        with gzip.open(archive, "rb") as stream:
            while stream.read(1024 * 1024):
                pass
        manifest["initramfs_sha256"] = hashlib.sha256(archive.read_bytes()).hexdigest()
        kernel = Path(temporary) / "vmlinuz"
        shutil.copyfile(image / "vmlinuz", kernel)
        if hashlib.sha256(kernel.read_bytes()).hexdigest() != manifest["kernel_sha256"]:
            raise RuntimeError("source kernel changed while preparing the guest")
        run_guest(args, suite, output, manifest, timeout, kernel, archive)


def publish_artifact(source, destination):
    """Only replace a public artifact after the complete private copy is synced."""
    with tempfile.NamedTemporaryFile(prefix=".completed-", dir=destination.parent, delete=False) as temporary:
        name = Path(temporary.name)
        try:
            with source.open("rb") as data:
                shutil.copyfileobj(data, temporary)
            temporary.flush()
            os.fsync(temporary.fileno())
            os.replace(name, destination)
        finally:
            name.unlink(missing_ok=True)


def require_guest_pass(text, suite, exit_status):
    """A clean exit without this exact executed test is never a passing gate."""
    lines = [line.strip() for line in text.splitlines()]
    if exit_status != 0:
        raise RuntimeError(f"QEMU exited with status {exit_status}")
    if suite["marker"] + "_PASS" not in lines:
        raise RuntimeError("guest did not emit its exact PASS marker")
    if not any(line.startswith("--- PASS: " + suite["test"] + " (") for line in lines):
        raise RuntimeError("required integration test did not report PASS")
    if "--- SKIP:" in text or "--- FAIL:" in text or "Kernel panic" in text:
        raise RuntimeError("guest reported a skipped or failed test")
    if suite["marker"] + "_FAIL" in text or "ROUTERD_KERNEL_TEST_FAIL" in text:
        raise RuntimeError("guest initializer reported failure")


def run_guest(args, suite, output, manifest, timeout, kernel, archive):
    log_path = archive.parent / "qemu-serial.log"
    command = [
        args.qemu, "-machine", "q35", "-accel", "tcg", "-cpu", "max",
        "-m", "512M", "-smp", "2", "-display", "none", "-monitor", "none",
        "-no-reboot", "-nodefaults", "-nic", "none", "-serial", f"file:{log_path}",
        "-kernel", str(kernel), "-initrd", str(archive),
        "-append", "console=ttyS0,115200 panic=-1",
    ]
    print(f"Booting the {args.suite} test guest (no external network interface)...", flush=True)
    process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    started = time.monotonic()
    try:
        while time.monotonic() - started < timeout:
            text = log_path.read_text(errors="replace") if log_path.exists() else ""
            if suite["marker"] + "_FAIL" in text or "ROUTERD_KERNEL_TEST_FAIL" in text or "Kernel panic" in text:
                raise RuntimeError(f"{args.suite} test failed in guest")
            if process.poll() is not None:
                # Re-read after exit: the final UART writes may have arrived
                # between this iteration's read and the process-state check.
                text = log_path.read_text(errors="replace") if log_path.exists() else ""
                try:
                    require_guest_pass(text, suite, process.returncode)
                except RuntimeError as error:
                    detail = process.stderr.read().decode(errors="replace")
                    raise RuntimeError(f"{error}: {detail}") from error
                if hashlib.sha256(archive.read_bytes()).hexdigest() != manifest["initramfs_sha256"]:
                    raise RuntimeError("private boot archive changed while the guest was running")
                manifest["result"] = "passed"
                manifest["duration_seconds"] = round(time.monotonic() - started, 2)
                print(text[text.find("=== RUN"):])
                print(f"QEMU {suite['description']}: PASS ({manifest['duration_seconds']}s)")
                break
            time.sleep(0.2)
        else:
            raise RuntimeError(f"{args.suite} test exceeded guest timeout")
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
        if hashlib.sha256(archive.read_bytes()).hexdigest() != manifest["initramfs_sha256"]:
            manifest["result"] = "failed"
            manifest["error"] = "private boot archive changed while the guest was running"
        publish_artifact(archive, output / "initramfs.cpio.gz")
        if log_path.exists():
            publish_artifact(log_path, output / "qemu-serial.log")
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        if manifest["result"] == "failed" and "private boot archive changed" in manifest.get("error", ""):
            raise SystemExit(manifest["error"])


if __name__ == "__main__":
    main()
