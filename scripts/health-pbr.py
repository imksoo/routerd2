#!/usr/bin/env python3
"""Exercise two independent WANs with real workers in isolated namespaces."""

import argparse
import os
from pathlib import Path
import subprocess


TEST = "TestDaemonHealthPBRFailoverAndRecovery"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/health-pbr-test"))
    args = parser.parse_args()
    os.chdir(Path(__file__).resolve().parent.parent)
    os.umask(0o022)
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    harness = output / "integration.test"
    subprocess.run([args.go, "test", "-race", "-tags", "integration", "-c", "-o", str(harness), "./internal/integration"], check=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = subprocess.check_output(root + ["mktemp", "-d", "/tmp/routerd-health-pbr.XXXXXX"], text=True).strip()
    try:
        subprocess.run(root + ["mkdir", stage + "/bin", stage + "/packets"], check=True)
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            subprocess.run(root + ["install", "-m", "755", str(args.binary_dir.resolve() / name), stage + "/bin/" + name], check=True)
        subprocess.run(root + ["install", "-m", "755", str(harness), stage + "/integration.test"], check=True)
        log_path = output / "test.log"
        with log_path.open("w") as log:
            result = subprocess.run(root + ["env", "ROUTERD2_INTEGRATION_REQUIRED=1", "ROUTERD2_TEST_BIN_DIR=" + stage + "/bin", "ROUTERD2_TEST_EVIDENCE_DIR=" + stage + "/packets", stage + "/integration.test", "-test.run=^" + TEST + "$", "-test.v", "-test.timeout=195s"], stdout=log, stderr=subprocess.STDOUT, timeout=210)
        content = log_path.read_text()
        if result.returncode or "--- PASS: " + TEST not in content or "--- SKIP:" in content:
            print(content, end="")
            raise RuntimeError(TEST + " failed; see " + str(log_path))
        for iface in ("wan0", "wan1"):
            packet = subprocess.check_output(root + ["cat", stage + "/packets/" + iface + ".pcap"])
            if len(packet) <= 24:
                raise RuntimeError("missing packet capture for " + iface)
            (output / (iface + ".pcap")).write_bytes(packet)
        print(TEST + ": PASS; " + str(log_path), flush=True)
    finally:
        subprocess.run(root + ["rm", "-rf", stage], check=True)


if __name__ == "__main__":
    main()
