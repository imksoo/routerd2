#!/usr/bin/env python3
"""Exercise real BFD/BGP workers and routed IPv4/IPv6 peers in private namespaces."""

import argparse
import os
from pathlib import Path
import subprocess


TESTS = (
    ("TestDaemonBFDControlsBGPSessionAndExpiry", "single-hop", False),
    ("TestDaemonMultihopBFDControlsBGPSessionAndExpiry", "multihop-ipv4", True),
    ("TestDaemonMultihopBFDIPv6", "multihop-ipv6", True),
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--test", choices=[name for name, _, _ in TESTS])
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/bfd-test"))
    args = parser.parse_args()
    os.chdir(Path(__file__).resolve().parent.parent)
    os.umask(0o022)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    harness = output / "integration.test"
    subprocess.run([args.go, "test", "-race", "-tags", "integration", "-c", "-o", str(harness), "./internal/integration"], check=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = subprocess.check_output(root + ["mktemp", "-d", "/tmp/routerd-bfd.XXXXXX"], text=True).strip()
    try:
        subprocess.run(root + ["mkdir", stage + "/bin"], check=True)
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            subprocess.run(root + ["install", "-m", "755", str(args.binary_dir.resolve() / name), stage + "/bin/" + name], check=True)
        subprocess.run(root + ["install", "-m", "755", str(harness), stage + "/integration.test"], check=True)
        for name, label, capture in TESTS:
            if args.test and name != args.test:
                continue
            packets = stage + "/" + label
            subprocess.run(root + ["mkdir", packets], check=True)
            log_path = output / (label + ".log")
            with log_path.open("w") as log:
                result = subprocess.run(root + ["env", "ROUTERD2_INTEGRATION_REQUIRED=1", "ROUTERD2_TEST_BIN_DIR=" + stage + "/bin", "ROUTERD2_TEST_EVIDENCE_DIR=" + packets, stage + "/integration.test", "-test.run=^" + name + "$", "-test.v", "-test.timeout=120s"], stdout=log, stderr=subprocess.STDOUT, timeout=130)
            content = log_path.read_text()
            if result.returncode or "--- PASS: " + name not in content or "--- SKIP:" in content:
                print(content, end="")
                raise RuntimeError(name + " failed; see " + str(log_path))
            if capture:
                packet = subprocess.check_output(root + ["cat", packets + "/wan0.pcap"])
                if len(packet) <= 24:
                    raise RuntimeError("missing routed packet capture for " + name)
                (output / (label + ".pcap")).write_bytes(packet)
            print(name + ": PASS; " + str(log_path), flush=True)
    finally:
        subprocess.run(root + ["python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1])", stage], check=True)


if __name__ == "__main__":
    main()
