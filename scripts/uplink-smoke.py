#!/usr/bin/env python3
"""Exercise a locally attached access network with a separate MAC and LAN client.

The actual product owns RA, DHCPv4, DHCPv6 PD, LAN RA, DNS and DS-Lite. The host's
management addresses/routes/clock are not changed. Requires explicit uplink and
AFTR intent, root/sudo, iproute2, curl, dig and tcpdump. A bounded owner removes
its namespaces and private state on completion. This is not NIC qualification.
"""

import argparse
import json
import os
import re
from pathlib import Path
import signal
import subprocess
import time


def run(args, timeout=12):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(result.stderr.strip() or str(args[0]) + " failed")
    return result.stdout


def owner(stage, uplink, aftr, seconds, dns_upstream):
    def stop(*_):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGALRM, stop)
    signal.alarm(seconds + 180)
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
    router, client = "r2uplink-" + str(os.getpid()), "r2client-" + str(os.getpid())
    children, namespaces = [], []

    def inside(ns, command):
        return ["ip", "netns", "exec", ns] + command

    def start(ns, command, name):
        with (stage / (name + ".log")).open("w") as out:
            p = subprocess.Popen(
                inside(ns, command), stdout=out, stderr=subprocess.STDOUT
            )
        children.append(p)
        return p

    def query(sock, path):
        return json.loads(
            run(
                [
                    "curl",
                    "--max-time",
                    "4",
                    "-sS",
                    "--unix-socket",
                    str(stage / "run" / sock),
                    "http://localhost" + path,
                ]
            )
        )

    def wait(condition, seconds=90):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if condition():
                return
            time.sleep(0.3)
        raise RuntimeError("actual uplink readiness deadline")

    roles = [
        "logging",
        "network",
        "control",
        "management",
        "ra_client",
        "dhcp4",
        "dhcp6",
        "aftr",
        "ra",
        "dns",
    ]
    boot = dict(
        platform="host",
        config=str(stage / "config.json"),
        runtime_dir=str(stage / "run"),
        state_dir=str(stage / "state"),
        ssh={"user": "admin"},
        workers={
            role: str(
                stage / ("routerd-logging" if role == "logging" else "routerd-modules")
            )
            for role in roles
        },
    )
    resources = [
        {"id": "wan", "interface": {"name": "wan0"}},
        {"id": "lan", "interface": {"name": "lan0"}},
        {"id": "lan-ip", "address": {"interface": "lan0", "cidr": "198.18.0.1/24"}},
        {
            "id": "forwarding",
            "forwarding": {"ipv4": True, "ipv6": True, "ipv4_rp_filter": 0},
        },
        {
            "id": "firewall",
            "firewall": {
                "forward": [
                    {"from": "lan0", "to": "wan0"},
                    {"from": "lan0", "to": "dslite0"},
                ],
                "masquerade": [{"source": "198.18.0.0/24", "out_interface": "dslite0"}],
                "mss_clamp": [
                    {"from": a, "to": b, "family": f, "mtu": mtu}
                    for a, b, f, mtu in [
                        ("lan0", "dslite0", 4, 1460),
                        ("dslite0", "lan0", 4, 1460),
                        ("lan0", "wan0", 6, 1500),
                        ("wan0", "lan0", 6, 1500),
                    ]
                ],
                "ipv4_clear_df": [{"from": "lan0", "to": "dslite0"}],
            },
        },
        {
            "id": "wan4",
            "service": {
                "name": "wan4",
                "dhcp4": {"interface": "wan0", "default_route": False},
            },
        },
        {
            "id": "wan-ra",
            "service": {"name": "wan-ra", "ra_client": {"interface": "wan0"}},
        },
        {
            "id": "wan-pd",
            "service": {
                "name": "wan-pd",
                "dhcp6": {
                    "interface": "wan0",
                    "iaid": 1,
                    "request_prefix": True,
                    "prefix_length": 60,
                    "delegations": [
                        {
                            "name": "lan",
                            "interface": "lan0",
                            "host_id": 1,
                            "advertise": True,
                        },
                        {"name": "b4", "interface": "lan0", "host_id": 256},
                    ],
                },
            },
        },
        {
            "id": "internet",
            "ready_when": {"fact": "service.internet.active"},
            "service": {
                "name": "internet",
                "ds_lite": {
                    "interface": "wan0",
                    "tunnel": "dslite0",
                    "source_from": "wan-pd",
                    "source_delegation": "b4",
                    "aftr_from": "wan-pd",
                    "candidates": [aftr],
                    "dns_from": ["wan-pd", "wan-ra"],
                    "b4_peer": "192.0.0.1/32",
                    "route_gateway": "192.0.0.1",
                    "mtu": 1460,
                    "metric": 200,
                },
            },
        },
        {
            "id": "dns",
            "service": {
                "name": "dns",
                "dns": {
                    "spec": {
                        "listen": [
                            {"name": "lan", "addresses": ["198.18.0.1"], "port": 53}
                        ],
                        "sources": [
                            {
                                "name": "provider",
                                "kind": "forward",
                                "match": ["."],
                                "upstreamFrom": [
                                    {"resource": "DHCPv6PrefixDelegation/wan-pd"},
                                    {"resource": "IPv6RAAddress/wan-ra"},
                                ],
                                "queryTimeout": "2s",
                            }
                        ],
                    }
                },
            },
        },
    ]
    if dns_upstream:
        source = resources[-1]["service"]["dns"]["spec"]["sources"][0]
        source["upstreams"] = [dns_upstream]
        source["bootstrapResolverFrom"] = source.pop("upstreamFrom")
    (stage / "boot.json").write_text(json.dumps(boot))
    (stage / "config.json").write_text(
        json.dumps({"schema_version": 2, "resources": resources})
    )
    try:
        for ns in [router, client]:
            run(["ip", "netns", "add", ns])
            namespaces.append(ns)
            run(["ip", "-n", ns, "link", "set", "lo", "up"])
        run(
            [
                "ip",
                "link",
                "add",
                "link",
                uplink,
                "name",
                "wan0",
                "netns",
                router,
                "type",
                "macvlan",
                "mode",
                "bridge",
            ]
        )
        run(
            [
                "ip",
                "link",
                "add",
                "lan0",
                "netns",
                router,
                "type",
                "veth",
                "peer",
                "name",
                "client0",
                "netns",
                client,
            ]
        )
        run(["ip", "-n", client, "link", "set", "client0", "up"])
        run(["ip", "-n", client, "addr", "add", "198.18.0.2/24", "dev", "client0"])
        run(["ip", "-n", client, "route", "add", "default", "via", "198.18.0.1"])
        capture = start(
            router,
            [
                "tcpdump",
                "--immediate-mode",
                "-Z",
                "root",
                "-n",
                "-U",
                "-i",
                "any",
                "-C",
                "4",
                "-W",
                "2",
                "-w",
                str(stage / "wan.pcap"),
            ],
            "capture",
        )
        daemon = start(
            router,
            [str(stage / "routerd"), "boot", "--file", str(stage / "boot.json")],
            "console",
        )
        (stage / "location.json").write_text(
            json.dumps(
                {
                    "owner_pid": os.getpid(),
                    "daemon_pid": daemon.pid,
                    "router": router,
                    "client": client,
                    "stage": str(stage),
                }
            )
        )
        print("Private live owner", os.getpid(), "stage", stage, flush=True)

        def ready():
            if daemon.poll() is not None:
                raise RuntimeError("actual uplink daemon exited")
            if not (stage / "run/control.sock").exists():
                return False
            return (
                query("control.sock", "/v1/config")["healthy"]
                and query("supervisor.sock", "/v1/status")["ready"]
            )

        wait(ready, 120)
        if capture.poll() is not None:
            raise RuntimeError("uplink packet capture exited before readiness")
        wait(
            lambda: any(
                a["scope"] == "global" and not a.get("tentative")
                for link in json.loads(
                    run(
                        [
                            "ip",
                            "-n",
                            client,
                            "-j",
                            "-6",
                            "addr",
                            "show",
                            "dev",
                            "client0",
                        ]
                    )
                )
                for a in link["addr_info"]
            )
        )

        def forwarding_ready():
            observed = query("control.sock", "/v1/services/service.internet").get(
                "snapshot", {}
            )
            route = subprocess.run(
                [
                    "ip",
                    "-n",
                    router,
                    "-j",
                    "route",
                    "get",
                    "1.1.1.1",
                    "from",
                    "198.18.0.2",
                    "iif",
                    "lan0",
                ],
                capture_output=True,
                text=True,
            )
            return (
                observed.get("available", False)
                and route.returncode == 0
                and json.loads(route.stdout)[0]["dev"] == "dslite0"
            )

        wait(forwarding_ready)

        def dns_ready():
            observed = query("services/dns.dns.sock", "/v1/status")["resources"][0][
                "observed"
            ]
            return (
                "Current" in observed.get("bootstrapResolvers", "")
                if dns_upstream
                else "udp://" in observed.get("upstreams", "")
            )

        wait(dns_ready)
        initial = query("control.sock", "/v1/services/service.wan-pd")["snapshot"][
            "renew_at"
        ]
        start_time, rounds, renewals = time.monotonic(), 0, 0
        while time.monotonic() - start_time < seconds:
            for family, address in [
                ("-4", "1.1.1.1"),
                ("-6", "[2606:4700:4700::1111]"),
            ]:
                result = run(
                    inside(
                        client,
                        [
                            "curl",
                            family,
                            "--noproxy",
                            "*",
                            "--max-time",
                            "8",
                            "--resolve",
                            "one.one.one.one:443:" + address,
                            "-sS",
                            "-I",
                            "-o",
                            "/dev/null",
                            "-w",
                            "%{http_code}",
                            "https://one.one.one.one/",
                        ],
                    )
                )
                assert result in ["200", "301", "302", "308"], (family, result)
            for transport in [[], ["+tcp"]]:
                response = run(
                    inside(
                        client,
                        [
                            "dig",
                            "@198.18.0.1",
                            "example.net",
                            "A",
                            "+tries=1",
                            "+time=3",
                        ]
                        + transport,
                    )
                )
                assert "status: NOERROR" in response and "ANSWER: 0" not in response
            selected = json.loads(
                run(
                    [
                        "ip",
                        "-n",
                        router,
                        "-j",
                        "route",
                        "get",
                        "1.1.1.1",
                        "from",
                        "198.18.0.2",
                        "iif",
                        "lan0",
                    ]
                )
            )
            assert selected[0]["dev"] == "dslite0", "IPv4 traffic bypassed the AFTR"
            renewed = query("control.sock", "/v1/services/service.wan-pd")["snapshot"][
                "renew_at"
            ]
            if renewed != initial:
                initial, renewals = renewed, renewals + 1
                print("Observed actual DHCPv6 renewal", renewals, flush=True)
            rounds += 1
            if rounds % 6 == 1:
                status = query("supervisor.sock", "/v1/status")
                print(
                    "Actual uplink elapsed",
                    round(time.monotonic() - start_time),
                    "rounds",
                    rounds,
                    "ready",
                    status["ready"],
                    "generations",
                    {m["name"]: m["generation"] for m in status["modules"]},
                    flush=True,
                )
            time.sleep(min(10, max(0, seconds - (time.monotonic() - start_time))))
        returned = any(
            run(
                [
                    "tcpdump",
                    "-nn",
                    "-r",
                    str(path),
                    "-c",
                    "1",
                    "ip6 proto 4 and inbound",
                ]
            ).strip()
            for path in stage.glob("wan.pcap*")
        )
        if not returned:
            raise RuntimeError("no actual AFTR encapsulated return packet captured")
        shutdown_at = time.time()
        daemon.send_signal(signal.SIGTERM)
        daemon.wait(timeout=30)
        assert daemon.returncode == 0, "actual uplink daemon failed to stop"
        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=3)
        records = []
        for path in stage.glob("wan.pcap*"):
            packets = run(["tcpdump", "-nn", "-tt", "-vv", "-r", str(path), "ip6"])
            for line in packets.splitlines():
                if line and line[0].isdigit():
                    records.append(line)
                elif records:
                    records[-1] += " " + line.strip()
        records.sort(key=lambda line: float(line.split()[0]))
        withdrawal = None
        transaction = None
        release_at = None
        reply_port = None
        for line in records:
            stamp = float(line.split()[0])
            if stamp < shutdown_at:
                continue
            if "router advertisement" in line and "router lifetime 0s" in line:
                assert "valid time 0s, pref. time 0s" in line, "final RA retained its prefix lifetime"
                withdrawal = stamp
            match = re.search(r"dhcp6 release \(xid=([0-9a-f]+)", line)
            if match:
                assert withdrawal is not None and withdrawal < stamp, "Release preceded LAN withdrawal"
                assert re.search(r"fe80:[^ ]+\.546 >", line), "Release did not use a link-local source"
                transaction, release_at = match[1], stamp
            if transaction and "dhcp6 reply (xid=" + transaction + " " in line:
                match = re.search(r" ([^ ]+)\.(\d+) > [^ ]+\.546:", line)
                assert match, "could not identify Release Reply source port"
                reply_port = int(match[2])
                break
        if release_at is None or reply_port is None:
            print("Release diagnosis: zero RA", withdrawal is not None, "Release", release_at is not None, "Reply", reply_port is not None, flush=True)
            for path in (stage / "console.log", *sorted((stage / "state/logs").glob("*"))):
                if path.is_file():
                    for line in path.read_text(errors="replace").splitlines()[-100:]:
                        if "ERROR" in line or "withdraw" in line or "Release" in line:
                            print(line, flush=True)
            for line in records:
                if float(line.split()[0]) >= shutdown_at and ("dhcp6" in line or "router advertisement" in line):
                    print(line, flush=True)
            raise RuntimeError("actual uplink did not acknowledge Release")
        assert not any(
            a["scope"] == "global"
            for link in json.loads(run(["ip", "-n", router, "-j", "-6", "addr", "show", "dev", "lan0"]))
            for a in link.get("addr_info", [])
        ), "shutdown retained the delegated LAN address"
        print("PASS actual uplink zero-lifetime RA, kernel withdrawal and link-local Release/Reply; server port", reply_port, flush=True)
        print(
            "PASS actual uplink periodic IPv4/IPv6 HTTPS, UDP/TCP learned DNS; rounds",
            rounds,
            "renewals",
            renewals,
            flush=True,
        )
        print(
            "No continuous TCP, direct-ONU wiring or physical NIC qualification is claimed",
            flush=True,
        )
    except Exception:
        try:
            if (stage / "run/control.sock").exists():
                status = query("control.sock", "/v1/services")
                print("Failed uplink projection", status.get("phase"), status.get("last_error"), flush=True)
                for instance in status.get("instances", []):
                    print("Failed uplink service", instance.get("name"), instance.get("phase"), instance.get("snapshot"), flush=True)
            for path in (stage / "console.log", *sorted((stage / "state/logs").glob("*.ndjson"))):
                if path.is_file():
                    for line in path.read_text(errors="replace").splitlines()[-60:]:
                        if '"ERROR"' in line or '"WARN"' in line:
                            print(line, flush=True)
            for path in stage.glob("wan.pcap*"):
                packets = run(["tcpdump", "-nn", "-vv", "-r", str(path), "-c", "20", "icmp6 or udp port 546 or udp port 547 or udp port 67 or udp port 68"])
                print("Failed uplink wire", packets, flush=True)
        except Exception as diagnostic_error:
            print("Uplink diagnostics failed", diagnostic_error, flush=True)
        raise
    finally:
        for p in reversed(children):
            if p.poll() is None:
                p.send_signal(signal.SIGINT if p is capture else signal.SIGTERM)
                try:
                    p.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
        for ns in reversed(namespaces):
            for pid in run(["ip", "netns", "pids", ns]).split():
                try:
                    os.kill(int(pid), signal.SIGTERM)
                except ProcessLookupError:
                    pass
            run(["ip", "netns", "delete", ns])
        print("Owned live namespaces removed", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--uplink", required=True)
    parser.add_argument("--aftr", required=True)
    parser.add_argument(
        "--dns-upstream",
        default="",
        help="Resolve this upstream hostname using WAN-learned bootstrap DNS",
    )
    parser.add_argument("--seconds", type=int, default=7800)
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--inside", type=Path)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 10800:
        parser.error("--seconds must be 1..10800")
    if args.inside:
        owner(args.inside, args.uplink, args.aftr, args.seconds, args.dns_upstream)
        return
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = Path(run(root + ["mktemp", "-d", "/tmp/routerd-live.XXXXXX"]).strip())
    try:
        for source, name in [(Path(__file__), "owner.py")] + [
            (args.binary_dir / n, n)
            for n in ["routerd", "routerd-modules", "routerd-logging"]
        ]:
            run(
                root
                + ["install", "-m", "500", str(source.resolve()), str(stage / name)]
            )
        print("Current private live stage:", stage, flush=True)
        subprocess.run(
            root
            + [
                "unshare",
                "--mount",
                "--propagation",
                "private",
                "python3",
                "-I",
                str(stage / "owner.py"),
                "--inside",
                str(stage),
                "--uplink",
                args.uplink,
                "--aftr",
                args.aftr,
                "--seconds",
                str(args.seconds),
                "--dns-upstream",
                args.dns_upstream,
            ],
            check=True,
            timeout=args.seconds + 240,
        )
    finally:
        run(
            root
            + [
                "python3",
                "-I",
                "-c",
                "import shutil,sys;shutil.rmtree(sys.argv[1])",
                str(stage),
            ]
        )


if __name__ == "__main__":
    main()
