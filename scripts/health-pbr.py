#!/usr/bin/env python3
"""Required real-daemon two-WAN gate in disposable Linux network namespaces."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time


PROJECT = Path(__file__).resolve().parent.parent
TEST = "TestDaemonHealthPBRFailoverAndRecovery"
MARKER = "ROUTERD_HEALTH_PBR_DAEMON_TEST_PASS"


def require_pass(output, code):
    if code != 0 or not re.search(r"^--- PASS: " + TEST + r" \(", output, re.M):
        raise RuntimeError("required real-daemon test did not report PASS")
    marker = re.search(r"^\s*(?:[\w./-]+\.go:\d+: )?" + MARKER + r"\s*$", output, re.M)
    if marker is None or "--- SKIP:" in output or "--- FAIL:" in output:
        raise RuntimeError("required real-daemon packet/recovery checks were skipped or failed")


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/health-pbr-test"))
    args = parser.parse_args()
    os.chdir(PROJECT)
    # Secure test key stores must not inherit a group-writable developer umask.
    os.umask(0o022)
    binary_dir = args.binary_dir.resolve()
    # Keep every run, including failures, independent of previous PASS/captures.
    output = args.output.resolve() / time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    output.mkdir(parents=True, exist_ok=False)
    binary = output / "integration.test"
    manifest = {
        "test": TEST, "result": "failed", "scope": "Linux namespaces, real daemon and kernel packets",
        "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
        "tracked_diff_sha256": hashlib.sha256(subprocess.check_output(["git", "diff", "HEAD"])).hexdigest(),
        "dirty": bool(subprocess.check_output(["git", "status", "--porcelain"])),
        "kernel": os.uname().release,
        "go": subprocess.check_output([args.go, "version"], text=True).strip(),
        "race_detector": True,
        "sources": {name: digest(PROJECT / name) for name in (
            "internal/integration/health_pbr_test.go", "internal/integration/bgp_test.go", "scripts/health-pbr.py",
        )},
    }
    try:
        subprocess.run([args.go, "test", "-c", "-race", "-tags", "integration", "-o", str(binary), "./internal/integration"], check=True)
        manifest["executables"] = {name: digest(path) for name, path in {
            "routerd": binary_dir / "routerd", "routerd-modules": binary_dir / "routerd-modules", "integration.test": binary,
        }.items()}
        env = dict(os.environ, ROUTERD2_TEST_BIN_DIR=str(binary_dir), ROUTERD2_INTEGRATION_REQUIRED="1", ROUTERD2_TEST_EVIDENCE_DIR=str(output))
        result = subprocess.run([str(binary), "-test.run=^" + TEST + "$", "-test.v", "-test.timeout=195s"], env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=210)
        (output / "test.log").write_text(result.stdout)
        print(result.stdout, end="")
        require_pass(result.stdout, result.returncode)
        for iface in ("wan0", "wan1"):
            pcap = output / (iface + ".pcap")
            if not pcap.exists() or pcap.stat().st_size <= 24:
                raise RuntimeError("required Ethernet packet capture is missing or empty: " + iface)
        manifest["artifacts"] = {p.name: digest(p) for p in sorted(output.iterdir()) if p.is_file() and p != binary}
        manifest["result"] = "pass"
    finally:
        (output / "result.json").write_text(json.dumps(manifest, indent=2) + "\n")
        print("Evidence: " + str(output))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        print(str(error), file=sys.stderr)
        sys.exit(1)
