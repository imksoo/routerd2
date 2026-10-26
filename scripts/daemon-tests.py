#!/usr/bin/env python3
"""Exercise native workers and independent peers in private Linux namespaces."""

import argparse
import os
from pathlib import Path
import subprocess


# Each fixture verifies its disposable user/network namespace before mutation.
# Timeout is the outer harness budget, not a protocol or client deadline.
SUITES = {
    "health-pbr": (("TestDaemonHealthPBRFailoverAndRecovery", ("wan0", "wan1"), 195),),
    "bfd": (
        ("TestDaemonBFDControlsBGPSessionAndExpiry", (), 120),
        ("TestDaemonMultihopBFDControlsBGPSessionAndExpiry", ("wan0",), 120),
        ("TestDaemonMultihopBFDIPv6", ("wan0",), 120),
        ("TestDaemonBFDVRFIsolation", ("wan0", "wan1", "wan2"), 120),
        ("TestDaemonBFDVRFIPv6Isolation", ("wan0", "wan1", "wan2"), 120),
        ("TestDaemonSinglehopBFDVRF", (), 120),
    ),
    "pd-ra": (("TestDaemonPDGeneratedRAWorkerLifetime", ("wan0", "lan0"), 120),),
    "dhcp-relay": (("TestDaemonDHCPRelayIndependentClientsAndServer", ("wan0", "lan0", "lan1"), 120),),
    "dhcp-server": (("TestDaemonDHCPDNSAndOwnershipExpiry", (), 120),),
    "aftr": (("TestDaemonAFTRResolutionAndExpiry", (), 120),),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("suite", choices=SUITES)
    parser.add_argument("--test", help="Run one named fixture from the selected suite")
    parser.add_argument("--go", default="go")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    tests = SUITES[args.suite]
    if args.test:
        tests = tuple(test for test in tests if test[0] == args.test)
        if not tests:
            parser.error("--test is not in the selected suite")
    os.chdir(Path(__file__).resolve().parent.parent)
    os.umask(0o022)
    output = (args.output or Path("dist/daemon-" + args.suite)).resolve()
    output.mkdir(parents=True, exist_ok=True)
    harness = output / "integration.test"
    subprocess.run([args.go, "test", "-race", "-tags", "integration", "-c", "-o", str(harness), "./internal/integration"], check=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = subprocess.check_output(root + ["mktemp", "-d", "/tmp/routerd-daemon.XXXXXX"], text=True).strip()
    try:
        subprocess.run(root + ["mkdir", stage + "/bin"], check=True)
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            subprocess.run(root + ["install", "-m", "755", str(args.binary_dir.resolve() / name), stage + "/bin/" + name], check=True)
        subprocess.run(root + ["install", "-m", "755", str(harness), stage + "/integration.test"], check=True)
        for name, interfaces, timeout in tests:
            packets = stage + "/" + name
            subprocess.run(root + ["mkdir", packets], check=True)
            log_path = output / (name + ".log")
            with log_path.open("w") as log:
                result = subprocess.run(root + ["env", "ROUTERD2_INTEGRATION_REQUIRED=1", "ROUTERD2_TEST_BIN_DIR=" + stage + "/bin", "ROUTERD2_TEST_EVIDENCE_DIR=" + packets, stage + "/integration.test", "-test.run=^" + name + "$", "-test.v", "-test.timeout=" + str(timeout) + "s"], stdout=log, stderr=subprocess.STDOUT, timeout=timeout + 15)
            content = log_path.read_text()
            for interface in interfaces:
                packet = subprocess.run(root + ["cat", packets + "/" + interface + ".pcap"], capture_output=True)
                if packet.returncode == 0:
                    (output / (name + "-" + interface + ".pcap")).write_bytes(packet.stdout)
                if result.returncode == 0 and (packet.returncode or len(packet.stdout) <= 24):
                    raise RuntimeError("missing packet capture for " + name + ": " + interface)
            if result.returncode or "--- PASS: " + name not in content or "--- SKIP:" in content:
                print(content, end="")
                raise RuntimeError(name + " failed; see " + str(log_path))
            print(name + ": PASS; " + str(log_path), flush=True)
    finally:
        subprocess.run(root + ["python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1])", stage], check=True)


if __name__ == "__main__":
    main()
