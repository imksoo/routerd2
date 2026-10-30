#!/usr/bin/env python3
"""Exercise native workers and independent peers in private Linux namespaces."""

import argparse
from contextlib import ExitStack
import os
from pathlib import Path
import subprocess
import tempfile


# Each fixture verifies its disposable user/network namespace before mutation.
# Timeout is the outer harness budget, not a protocol or client deadline.
SUITES = {
    "traffic-flow": (("TestDaemonTrafficFlowHistory", ("wan0", "lan0"), 85), ("TestDaemonTrafficFlowNDPI", ("wan0", "lan0"), 85)),
    "management": (("TestDaemonConfigurationAndRecovery", ("mgmt0",), 100),),
    "local-redirect": (("TestDaemonLocalDNSRedirect", ("wan0", "lan0"), 85),),
    "blackhole": (("TestDaemonBlackholeRoute", ("wan0", "lan0"), 55),),
    "firewall": (("TestDaemonFirewallSourceCIDRs", ("wan0",), 55), ("TestDaemonFirewallPacketLogs", ("wan0",), 75)),
    "host-route": (("TestDaemonImportedIPv6HostRoute", ("wan0",), 75), ("TestDaemonRAHostRoute", ("wan0",), 75), ("TestDaemonImplicitIPv6HostRoute", ("wan0",), 75)),
    "health-pbr": (
        ("TestDaemonConnectionMarkedEgressPolicy", ("wan0", "wan1", "wan2", "wan3", "wan4", "lan0"), 165),
        ("TestDaemonHealthPBRFailoverAndRecovery", ("wan0", "wan1"), 195),
        ("TestDaemonEgressRouteHealthyMultipath", ("wan0", "wan1"), 120),
    ),
    "ingress": (("TestDaemonImportedIngressHTTPS", ("lan0",), 165),),
    "bgp": (
        ("TestDaemonBGPFIBAndOwnershipExpiry", (), 120),
        ("TestDaemonBGPRecursivePrerequisiteExpiry", (), 120),
    ),
    "bgp-frr": (
        ("TestDaemonBGPWithFRR", ("wan0", "lan0"), 110),
        ("TestDaemonIPIPBGPWithFRR", ("wan0", "lan0"), 110),
        ("TestDaemonDHCP4IPIPBGPWithFRR", ("wan0", "lan0"), 260),
        ("TestDaemonBGPTwoFRRPeers", ("wan0", "wan1", "lan0"), 140),
    ),
    "bfd": (
        ("TestDaemonBFDControlsBGPSessionAndExpiry", (), 120),
        ("TestDaemonMultihopBFDControlsBGPSessionAndExpiry", ("wan0",), 120),
        ("TestDaemonMultihopBFDIPv6", ("wan0",), 120),
        ("TestDaemonBFDVRFIsolation", ("wan0", "wan1", "wan2"), 120),
        ("TestDaemonBFDVRFIPv6Isolation", ("wan0", "wan1", "wan2"), 120),
        ("TestDaemonSinglehopBFDVRF", (), 120),
    ),
    "dhcp6": (
        ("TestDaemonDHCP6RAIndependentPeers", ("wan0",), 300),
        ("TestDaemonDHCP6DuplicateAddressDecline", ("wire0",), 90),
    ),
    "dhcp6-server": (("TestDaemonDHCP6InformationIndependentClient", ("lan0",), 100),),
    "dhcp6-ra-pd": (
        ("TestDaemonDHCP6RAAddsAddressToPD", ("wire0", "lan0"), 160),
        ("TestDaemonImportedConditionalLAN", ("wan0", "lan0"), 110),
    ),
    "pd-ra": (("TestDaemonPDGeneratedRAWorkerLifetime", ("wan0", "lan0"), 120),),
    "dhcp-relay": (
        ("TestDaemonDHCPRelayIndependentClientsAndServer", ("wan0", "lan0", "lan1"), 120),
        ("TestDaemonDHCPRelayVRFIsolation", ("wan0", "lan0", "wan1", "lan1", "wan2", "lan2"), 120),
    ),
    "dhcp-server": (
        ("TestDaemonDHCPFingerprints", (), 75),
        ("TestDaemonMigratedDHCPUnmanagedLAN", (), 75),
        ("TestDaemonMigratedDHCPLogs", (), 75),
        ("TestDaemonDHCPDNSAndOwnershipExpiry", (), 120),
        ("TestDaemonMigratedDHCPDNSAndOwnershipExpiry", (), 120),
    ),
    "dhcp4": (
        ("TestDaemonDHCPv4DelayedACK", ("wan0",), 180),
        ("TestDaemonDHCPv4OriginalLeaseAuthority", ("wan0",), 180),
    ),
    "aftr": (("TestDaemonAFTRResolutionAndExpiry", ("wan0",), 120),),
    "ra-client": (("TestDaemonRAClientConfigurationAndExpiry", ("wan0",), 120),),
    "ra-aftr": (("TestDaemonRAAFTRForwardingAndOriginalExpiry", ("wan0", "lan0"), 150),),
}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("suite", choices=SUITES)
    parser.add_argument("--test", help="Run one named fixture from the selected suite")
    parser.add_argument("--dhcp6-server", type=Path, default=Path("/usr/sbin/dhcpd"))
    parser.add_argument("--frr", type=Path, default=Path("/usr/lib/frr/bgpd"))
    parser.add_argument("--go", default="go")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, help="Retain logs/captures here; otherwise remove them after inspection")
    args = parser.parse_args()
    tests = SUITES[args.suite]
    if args.test:
        tests = tuple(test for test in tests if test[0] == args.test)
        if not tests:
            parser.error("--test is not in the selected suite")
    os.chdir(Path(__file__).resolve().parent.parent)
    os.umask(0o022)
    with ExitStack() as cleanup:
        output = args.output
        if output is None:
            output = Path(cleanup.enter_context(tempfile.TemporaryDirectory(prefix="routerd-daemon-output.")))
        output = output.resolve()
        output.mkdir(parents=True, exist_ok=True)
        harness = output / "integration.test"
        subprocess.run([args.go, "test", "-race", "-tags", "integration", "-c", "-o", str(harness), "./internal/integration"], check=True)
        root = [] if os.geteuid() == 0 else ["sudo", "-n"]
        if any(name == "TestDaemonDHCPv4DelayedACK" for name, _, _ in tests):
            subprocess.run(root + ["modprobe", "-a", "sch_netem", "sch_prio", "cls_flower"], check=True)
        stage = subprocess.check_output(root + ["mktemp", "-d", "/tmp/routerd-daemon.XXXXXX"], text=True).strip()
        try:
            subprocess.run(root + ["mkdir", stage + "/bin"], check=True)
            for name in ("routerd", "routerd-modules", "routerd-logging"):
                subprocess.run(root + ["install", "-m", "755", str(args.binary_dir.resolve() / name), stage + "/bin/" + name], check=True)
            if args.suite == "dhcp-server":
                subprocess.run(root + ["install", "-m", "644", str(Path("dist/dnsmasq-engine.tar").resolve()), stage + "/bin/dnsmasq-engine.tar"], check=True)
            if args.suite == "dhcp6-ra-pd":
                subprocess.run(root + ["install", "-m", "755", str(args.dhcp6_server.resolve()), stage + "/bin/dhcpd"], check=True)
            subprocess.run(root + ["install", "-m", "755", str(harness), stage + "/integration.test"], check=True)
            for name, interfaces, timeout in tests:
                packets = stage + "/" + name
                subprocess.run(root + ["mkdir", packets], check=True)
                log_path = output / (name + ".log")
                with log_path.open("w") as log:
                    result = subprocess.run(root + ["env", "ROUTERD2_INTEGRATION_REQUIRED=1", "ROUTERD2_DHCP6_SERVER=" + stage + "/bin/dhcpd", "ROUTERD2_FRR_BGPD=" + str(args.frr.resolve()), "ROUTERD2_TEST_BIN_DIR=" + stage + "/bin", "ROUTERD2_TEST_EVIDENCE_DIR=" + packets, stage + "/integration.test", "-test.run=^" + name + "$", "-test.v", "-test.timeout=" + str(timeout) + "s"], stdout=log, stderr=subprocess.STDOUT, timeout=timeout + 15)
                content = log_path.read_text()
                for interface in interfaces:
                    packet = subprocess.run(root + ["cat", packets + "/" + interface + ".pcap"], capture_output=True)
                    if packet.returncode == 0:
                        (output / (name + "-" + interface + ".pcap")).write_bytes(packet.stdout)
                    if result.returncode == 0 and (packet.returncode or len(packet.stdout) <= 24):
                        raise RuntimeError("missing packet capture for " + name + ": " + interface)
                if result.returncode or "--- PASS: " + name not in content or "--- SKIP:" in content:
                    print(content, end="")
                    raise RuntimeError(name + " failed")
                print(name + ": PASS" + ("; " + str(log_path) if args.output else ""), flush=True)
        finally:
            subprocess.run(root + ["python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1])", stage], check=True)


if __name__ == "__main__":
    main()
