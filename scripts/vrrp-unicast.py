#!/usr/bin/env python3
"""Exercise real routerd workers against keepalived in isolated namespaces."""

import argparse
import os
from pathlib import Path
import shutil
import subprocess


TESTS = (
    ("TestDaemonVRRPUnicastIPv4", "ipv4"),
    ("TestDaemonVRRPRFC9568IPv4", "ipv4-rfc9568"),
    ("TestDaemonVRRPUnicastIPv6", "ipv6"),
    ("TestDaemonVRRPMulticastIPv6", "ipv6-multicast"),
    ("TestDaemonVRRPSlowInitialACK", "slow-initial-ack"),
    ("TestDaemonVRRPHealthTracking", "health-track"),
    ("TestDaemonVRRPBFDAndBGPTracking", "bfd-bgp-track"),
    ("TestDaemonVRRPv2", "v2"),
    ("TestDaemonVRRPv2PASS", "v2-pass"),
    ("TestDaemonVRRPAdditionalIdentity", "additional-identity"),
    ("TestDaemonVRRPMigratedAdditionalIdentity", "additional-import"),
)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--test", choices=[name for name, _ in TESTS])
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--dhcp6-server", type=Path, default=Path("/usr/sbin/dhcpd"))
    parser.add_argument("--output", type=Path, default=Path("dist/vrrp-unicast"))
    args = parser.parse_args()
    os.chdir(Path(__file__).resolve().parent.parent)
    os.umask(0o022)
    if shutil.which("keepalived") is None:
        raise RuntimeError("keepalived is required as the independent VRRP peer")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    harness = output / "integration.test"
    subprocess.run([args.go, "test", "-race", "-tags", "integration", "-c", "-o", str(harness), "./internal/integration"], check=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = subprocess.check_output(root + ["mktemp", "-d", "/tmp/routerd-vrrp-unicast.XXXXXX"], text=True).strip()
    try:
        subprocess.run(root + ["mkdir", stage + "/bin"], check=True)
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            subprocess.run(root + ["install", "-m", "755", str(args.binary_dir.resolve() / name), stage + "/bin/" + name], check=True)
        subprocess.run(root + ["install", "-m", "755", str(harness), stage + "/integration.test"], check=True)
        if not args.test or args.test == "TestDaemonVRRPMigratedAdditionalIdentity":
            subprocess.run(root + ["install", "-m", "755", str(args.dhcp6_server.resolve()), stage + "/dhcpd"], check=True)
        for name, log_name in TESTS:
            if args.test and name != args.test:
                continue
            log_path = output / (log_name + ".log")
            packets = stage + "/" + log_name
            with log_path.open("w") as log:
                result = subprocess.run(root + ["env", "ROUTERD2_INTEGRATION_REQUIRED=1", "ROUTERD2_DHCP6_SERVER=" + stage + "/dhcpd", "ROUTERD2_TEST_BIN_DIR=" + stage + "/bin", "ROUTERD2_TEST_EVIDENCE_DIR=" + packets, stage + "/integration.test", "-test.run=^" + name + "$", "-test.v", "-test.timeout=90s"], stdout=log, stderr=subprocess.STDOUT, timeout=100)
            content = log_path.read_text()
            captured = False
            for interface in ("lan0", "wan0"):
                packet = subprocess.run(root + ["cat", packets + "/" + interface + ".pcap"], capture_output=True)
                if packet.returncode == 0 and len(packet.stdout) > 24:
                    (output / (log_name + "-" + interface + ".pcap")).write_bytes(packet.stdout)
                    captured = True
            if result.returncode or "--- PASS: " + name not in content or "--- SKIP:" in content:
                print(content, end="")
                raise RuntimeError(name + " failed; see " + str(log_path))
            if not captured:
                raise RuntimeError("missing packet capture for " + name)
            print(name + ": PASS; " + str(log_path), flush=True)
    finally:
        subprocess.run(root + ["python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1])", stage], check=True)


if __name__ == "__main__":
    main()
