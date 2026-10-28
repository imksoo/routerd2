#!/usr/bin/env python3
"""Exercise a locally attached access network with a separate MAC and LAN client.

The actual product owns RA, DHCPv4, DHCPv6 PD, LAN RA, DNS and DS-Lite. The host's
management addresses/routes/clock are not changed. Requires explicit uplink and
AFTR intent, root/sudo, iproute2, BusyBox, dhcpcd, dnsmasq, curl, dig and tcpdump. A bounded owner removes
its namespaces on completion. An optional private state directory retains the
test router's identity and WAN leases. This is not NIC qualification.
"""

import argparse
import ipaddress
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


def owner(stage, uplink, aftr, seconds, dns_upstream, wan_mac):
    def stop(*_):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGALRM, stop)
    signal.alarm(seconds + 180)
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/var/lib/dhcpcd"])
    resolver_file = stage / "automatic-resolv.conf"
    resolver_file.write_text("")
    resolver_target = Path("/etc/resolv.conf").resolve()
    if not resolver_target.exists():
        if not resolver_target.is_relative_to("/run"):
            raise RuntimeError("resolver target is absent outside the private runtime mount")
        resolver_target.parent.mkdir(parents=True, exist_ok=True)
        resolver_target.touch()
    run(["mount", "--bind", str(resolver_file), str(resolver_target)])
    router, client, automatic = "r2uplink-" + str(os.getpid()), "r2client-" + str(os.getpid()), "r2auto-" + str(os.getpid())
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
        "dhcp_server",
        "dhcp6_server",
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
                "input": {"policy": "accept", "rules": [
                    {"match": {"in_interface": "wan0", "protocol": protocol, "destination_ports": [{"start": 53}]}, "action": "drop"}
                    for protocol in ("tcp", "udp")
                ]},
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
                            "other_config": True,
                            "rdnss_router": True,
                        },
                        {"name": "b4", "interface": "wan0", "host_id": 256, "prefix_length": 128},
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
            "id": "leases",
            "service": {"name": "leases", "dhcp_server": {
                "interface": "lan0", "server_address": "198.18.0.1", "authoritative": True,
                "primary": {"prefix": "198.18.0.0/24", "start": "198.18.0.2", "end": "198.18.0.20",
                            "lease_seconds": 60, "router": "198.18.0.1", "dns_servers": ["198.18.0.1"]},
                "reservations": [{"mac_address": "02:00:00:00:00:02", "ip_address": "198.18.0.2"}],
            }},
        },
        {
            "id": "lan-info",
            "service": {"name": "lan-info", "dhcp6_server": {
                "interface": "lan0", "dns_delegation": {"service": "wan-pd", "delegation": "lan"},
                "domain_search": ["home.arpa"], "refresh_seconds": 600,
            }},
        },
        {
            "id": "dns",
            "service": {
                "name": "dns",
                "dns": {
                    "listen_interface": "lan0",
                    "spec": {
                        "listen": [
                            {"name": "lan", "addresses": ["::"], "port": 53}
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
    resources[-1]["service"]["dns"]["spec"]["sources"].insert(0, {
        "name": "private", "kind": "forward", "match": ["split.test"],
        "upstreams": ["udp://198.18.0.2:5353"], "queryTimeout": "1s",
    })
    if dns_upstream:
        source = resources[-1]["service"]["dns"]["spec"]["sources"][-1]
        source["upstreams"] = [dns_upstream]
        source["bootstrapResolverFrom"] = source.pop("upstreamFrom")
    (stage / "boot.json").write_text(json.dumps(boot))
    (stage / "config.json").write_text(
        json.dumps({"schema_version": 2, "resources": resources})
    )
    try:
        for ns in [router, client, automatic]:
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
        # Repeated runs represent one test router. Its DUID-LL must not change
        # with each newly created macvlan and consume additional HGW leases.
        run(["ip", "-n", router, "link", "set", "wan0", "address", wan_mac])
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
        run(["ip", "-n", client, "link", "set", "client0", "address", "02:00:00:00:00:02"])
        run(["ip", "-n", client, "link", "set", "client0", "up"])
        run(["ip", "-n", client, "link", "add", "link", "client0", "name", "client1", "type", "macvlan", "mode", "bridge"])
        run(["ip", "-n", client, "link", "set", "client1", "netns", automatic])
        run(["ip", "-n", automatic, "link", "set", "client1", "up"])
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
        lease_events = stage / "client-lease-events"
        hook = stage / "client-dhcp-hook.sh"
        hook.write_text(
            '#!/bin/sh\ncase "$1" in\n'
            'bound|renew)\n'
            '  ip address replace "$ip/24" dev "$interface" || exit 1\n'
            '  ip route replace default via "$router" dev "$interface" || exit 1\n'
            '  printf "%s %s %s %s\\n" "$1" "$ip" "$router" "$dns" >> "' + str(lease_events) + '";;\n'
            'esac\n'
        )
        hook.chmod(0o500)
        dhcp_client = start(client, ["busybox", "udhcpc", "-f", "-i", "client0", "-s", str(hook), "-p", str(stage / "client-dhcp.pid"), "-t", "3", "-T", "1", "-A", "1", "-x", "hostname:uplink-client"], "client-dhcp")
        wait(lambda: lease_events.exists() and bool(lease_events.read_text().splitlines()))
        lease = lease_events.read_text().splitlines()[-1].split()
        assert lease[0] == "bound" and lease[1:] == ["198.18.0.2", "198.18.0.1", "198.18.0.1"], "LAN DHCP did not supply the expected address, gateway and DNS: " + str(lease)
        print("PASS independent LAN client acquired IPv4 address, gateway and DNS from routerd2 DHCP", flush=True)

        def start_private_dns():
            return start(client, ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-interfaces", "--listen-address=198.18.0.2", "--port=5353", "--local-ttl=1", "--address=/split.test/192.0.2.77"], "private-dns")
        private_dns = start_private_dns()
        wait(lambda: subprocess.run(inside(router, ["dig", "@198.18.0.2", "-p", "5353", "ready.split.test", "A", "+short", "+time=1", "+tries=1"]), capture_output=True, text=True).stdout.strip() == "192.0.2.77")
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

        info_hook = stage / "client-dhcp6-hook.sh"
        info_hook.write_text('#!/bin/sh\ncase "$1" in bound|renew) printf "dns=%s\\nsearch=%s\\n" "$dns" "$search";; esac\n')
        info_hook.chmod(0o500)
        information = run(inside(client, ["busybox", "udhcpc6", "-l", "-f", "-n", "-q", "-i", "client0", "-s", str(info_hook), "-t", "3", "-T", "1"]))
        options = dict(line.split("=", 1) for line in information.splitlines() if line.startswith(("dns=", "search=")))
        servers = [ipaddress.IPv6Address(value) for value in options.get("dns", "").split()]
        assert len(servers) == 1 and "home.arpa" in options.get("search", "").split(), "LAN DHCPv6 information did not supply the router DNS and search domain: " + str(options)
        print("PASS independent LAN client received router DNS and search domain via DHCPv6", flush=True)

        auto_events = stage / "automatic-client-events"
        auto_hook = stage / "automatic-client-hook.sh"
        auto_hook.write_text(
            '#!/bin/sh\ncase "$reason" in\n'
            'BOUND|RENEW) printf "v4|%s|%s|%s|%s\\n" "$reason" "$new_ip_address" "$new_routers" "$new_domain_name_servers" >> "' + str(auto_events) + '"; '
            ': > "' + str(resolver_file) + '"; for server in $new_domain_name_servers; do printf "nameserver %s\\n" "$server" >> "' + str(resolver_file) + '"; done;;\n'
            'ROUTERADVERT) printf "ra|%s\\n" "$nd1_rdnss1_servers" >> "' + str(auto_events) + '";;\n'
            'INFORM6) printf "v6|%s|%s\\n" "$new_dhcp6_name_servers" "$new_dhcp6_domain_search" >> "' + str(auto_events) + '"; '
            ': > "' + str(resolver_file) + '"; for server in $new_dhcp6_name_servers; do printf "nameserver %s\\n" "$server" >> "' + str(resolver_file) + '"; done;;\n'
            'esac\n'
        )
        auto_hook.chmod(0o500)
        auto_config = stage / "automatic-client.conf"
        auto_config.write_text("ipv6rs\nslaac hwaddr\nnoipv4ll\noption domain_name_servers, domain_name, domain_search\n")
        automatic_client = start(automatic, ["dhcpcd", "-B", "-d", "-f", str(auto_config), "-c", str(auto_hook), "client1"], "automatic-client")

        def automatic_ready():
            if automatic_client.poll() is not None:
                raise RuntimeError("automatic independent LAN client exited")
            if not auto_events.exists():
                return False
            events = [line.split("|") for line in auto_events.read_text().splitlines()]
            return any(len(event) == 5 and event[0] == "v4" and event[3:] == ["198.18.0.1", "198.18.0.1"] for event in events) and any(len(event) == 3 and event[0] == "v6" and event[1] and "home.arpa" in event[2].split() for event in events) and any(event[0] == "ra" and len(event) == 2 and event[1] for event in events)
        wait(automatic_ready, 60)
        auto_options = [line.split("|") for line in auto_events.read_text().splitlines()]
        auto_dns4 = next(event[4] for event in reversed(auto_options) if event[0] == "v4")
        auto_dns6 = next(event[1] for event in reversed(auto_options) if event[0] == "v6")
        auto_addresses = json.loads(run(["ip", "-n", automatic, "-j", "address", "show", "dev", "client1"]))
        delegated = [ipaddress.IPv6Network(p["prefix"]) for p in query("control.sock", "/v1/services/service.wan-pd")["snapshot"]["prefixes"]]
        router_dns = ipaddress.IPv6Address(next(event[1] for event in reversed(auto_options) if event[0] == "ra" and len(event) == 2 and event[1]))
        assert servers == [router_dns] and ipaddress.IPv6Address(auto_dns6) == router_dns, "RA and DHCPv6 DNS disagree"
        router_addresses = json.loads(run(["ip", "-n", router, "-j", "-6", "address", "show", "dev", "lan0"]))
        assert any(ipaddress.IPv6Address(a["local"]) == router_dns and any(router_dns in prefix for prefix in delegated) for link in router_addresses for a in link["addr_info"]), "RA DNS is not the delegated LAN router address"
        assert any(a["family"] == "inet6" and not a.get("tentative") and any(ipaddress.IPv6Address(a["local"]) in prefix for prefix in delegated) for link in auto_addresses for a in link["addr_info"]), "automatic LAN client has no usable delegated SLAAC address"
        print("PASS independent dhcpcd automatically acquired DHCPv4 and RA-triggered DHCPv6 with delegated SLAAC", flush=True)

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
            wan = json.loads(run(["ip", "-n", router, "-j", "-6", "addr", "show", "dev", "wan0"]))
            return (
                observed.get("available", False)
                and route.returncode == 0
                and json.loads(route.stdout)[0]["dev"] == "dslite0"
                and any(
                    a["local"] == observed.get("local") and a["prefixlen"] == 128
                    for link in wan for a in link["addr_info"]
                )
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
        for server, family in ((lease[3], "-4"), (str(router_dns), "-6")):
            answer = run(inside(client, ["dig", family, "@" + server, "available.split.test", "A", "+short", "+time=3", "+tries=1"]))
            assert answer.strip() == "192.0.2.77", "conditional DNS query reached the wrong upstream: " + answer
        private_dns.terminate()
        private_dns.wait(timeout=3)
        failed = run(inside(client, ["dig", "@" + lease[3], "offline.split.test", "A", "+time=3", "+tries=1"]))
        assert "status: SERVFAIL" in failed, "unavailable conditional DNS source fell through to the default upstream"
        private_dns = start_private_dns()
        wait(lambda: subprocess.run(inside(client, ["dig", "@" + lease[3], "recovered.split.test", "A", "+short", "+time=2", "+tries=1"]), capture_output=True, text=True).stdout.strip() == "192.0.2.77")
        print("PASS independent conditional DNS source, failure isolation and recovery over LAN IPv4/IPv6", flush=True)

        def check_traffic():
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
                            "@" + lease[3],
                            "example.net",
                            "A",
                            "+tries=1",
                            "+time=3",
                        ]
                        + transport,
                    )
                )
                assert "status: NOERROR" in response and "ANSWER: 0" not in response, (transport, response)
            for transport in [[], ["+tcp"]]:
                response = run(inside(client, ["dig", "-6", "@" + str(router_dns), "example.net", "AAAA", "+tries=1", "+time=3"] + transport))
                assert "status: NOERROR" in response and "ANSWER: 0" not in response, (transport, response)
            response = run(inside(automatic, ["dig", "-6", "@" + str(router_dns), "example.net", "AAAA", "+tries=1", "+time=3"]))
            assert "status: NOERROR" in response and "ANSWER: 0" not in response, "delegated router DNS failed: " + response
            for family, server in (("-4", auto_dns4), ("-6", auto_dns6)):
                result = run(inside(automatic, ["curl", family, "--noproxy", "*", "--max-time", "8", "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
                assert result in ["200", "301", "302", "308"], (family, result)
                response = run(inside(automatic, ["dig", family, "@" + server, "example.net", "A", "+tries=1", "+time=3"]))
                assert "status: NOERROR" in response and "ANSWER: 0" not in response, (family, response)
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

        initial = query("control.sock", "/v1/services/service.wan-pd")["snapshot"][
            "renew_at"
        ]
        renew_due = initial["nanos"] / 1e9 - time.clock_gettime(time.CLOCK_BOOTTIME)
        started_at = time.time()
        start_time, rounds, updates = time.monotonic(), 0, 0
        while time.monotonic() - start_time < seconds:
            if dhcp_client.poll() is not None or automatic_client.poll() is not None:
                raise RuntimeError("independent LAN DHCP client exited")
            check_traffic()
            renewed = query("control.sock", "/v1/services/service.wan-pd")["snapshot"][
                "renew_at"
            ]
            if renewed != initial:
                initial, updates = renewed, updates + 1
                print("Observed actual DHCPv6 grant update", updates, flush=True)
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
        events = [line.split() for line in lease_events.read_text().splitlines()]
        if seconds >= 75:
            assert any(event == ["renew", "198.18.0.2", "198.18.0.1", "198.18.0.1"] for event in events), "LAN DHCP client did not renew its address, gateway and DNS"
            assert any(line.startswith("v4|RENEW|") for line in auto_events.read_text().splitlines()), "automatic LAN client did not naturally renew DHCPv4"
            print("PASS both independent LAN DHCP clients naturally renewed their addresses, gateway and DNS", flush=True)
        before = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
        previous = next(
            m for m in query("supervisor.sock", "/v1/status")["modules"]
            if m["name"] == "service.wan-pd"
        )
        restarted_at = time.time()
        os.kill(previous["pid"], signal.SIGKILL)

        def recovered():
            instance = query("control.sock", "/v1/services/service.wan-pd")
            return (
                instance["generation"] > previous["generation"]
                and instance.get("snapshot", {}).get("state") == "bound"
            )

        wait(recovered, 30)
        after = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
        assert (after["client_duid"], after["iaid"]) == (before["client_duid"], before["iaid"]), "PD worker restart lost client identity"
        wait(forwarding_ready)
        wait(dns_ready)
        check_traffic()
        print("PASS actual uplink PD worker SIGKILL recovery, retained DUID/IAID and restored LAN communication", flush=True)
        previous = next(
            m for m in query("supervisor.sock", "/v1/status")["modules"]
            if m["name"] == "service.wan-pd"
        )
        graceful_at = time.time()
        os.kill(previous["pid"], signal.SIGTERM)
        # The old AFTR observation can briefly remain available while the PD
        # worker withdraws its source. Observe withdrawal before waiting for the
        # replacement, so a stale pre-stop snapshot cannot satisfy readiness.
        wait(lambda: not query("control.sock", "/v1/services/service.internet").get("snapshot", {}).get("available", False), 30)
        wait(recovered, 30)
        graceful = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
        assert (graceful["client_duid"], graceful["iaid"]) == (before["client_duid"], before["iaid"]), "graceful restart lost PD identity"
        assert {p["prefix"] for p in graceful["prefixes"]} == {p["prefix"] for p in before["prefixes"]}, "graceful restart changed the delegated prefix"
        wait(forwarding_ready)
        wait(dns_ready)
        check_traffic()
        print("PASS actual uplink graceful PD restart, retained DUID/IAID and prefix, restored LAN communication", flush=True)
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
        assert not any(float(line.split()[0]) >= graceful_at and "dhcp6 release (xid=" in line for line in records), "graceful stop released the upstream grant"
        renews, rebinds, replies = set(), set(), set()
        for line in records:
            stamp = float(line.split()[0])
            if not started_at <= stamp < shutdown_at:
                continue
            match = re.search(r"dhcp6 (renew|rebind|reply) \(xid=([0-9a-f]+)", line)
            if not match:
                continue
            kind, xid = match.groups()
            if kind == "renew" and stamp < restarted_at:
                renews.add(xid)
            if kind == "rebind" and stamp >= restarted_at:
                rebinds.add(xid)
            if kind == "reply":
                replies.add(xid)
        natural_renewals = len(renews & replies)
        assert rebinds & replies, "PD worker restart had no captured Rebind/Reply"
        if renew_due < seconds:
            assert natural_renewals, "natural Renew/Reply was not captured before worker restart"
        withdrawal = None
        for line in records:
            stamp = float(line.split()[0])
            if stamp < shutdown_at:
                continue
            if "router advertisement" in line and "router lifetime 0s" in line:
                assert "valid time 0s, pref. time 0s" in line, "final RA retained its prefix lifetime"
                withdrawal = stamp
            assert "dhcp6 release (xid=" not in line, "normal shutdown released the upstream grant"
        assert withdrawal is not None, "missing zero-lifetime RA at shutdown"
        delegated = [ipaddress.IPv6Network(prefix["prefix"]) for prefix in before["prefixes"]]
        remaining = [
            a["local"]
            for link in json.loads(run(["ip", "-n", router, "-j", "-6", "addr", "show", "dev", "lan0"]))
            for a in link.get("addr_info", [])
            if any(ipaddress.IPv6Address(a["local"]) in prefix for prefix in delegated)
        ]
        assert not remaining, "shutdown retained delegated LAN addresses: " + str(remaining)
        print("PASS actual uplink zero-lifetime RA and kernel withdrawal without releasing the upstream grant", flush=True)
        print(
            "PASS actual uplink periodic IPv4/IPv6 HTTPS, UDP/TCP learned DNS; rounds",
            rounds,
            "captured natural Renew/Reply",
            natural_renewals,
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
    parser.add_argument("--state-dir", type=Path, help="Private persistent test identity and lease directory")
    parser.add_argument("--wan-mac", default="02:72:6f:75:74:02")
    parser.add_argument("--inside", type=Path)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 10800:
        parser.error("--seconds must be 1..10800")
    if args.inside:
        owner(args.inside, args.uplink, args.aftr, args.seconds, args.dns_upstream, args.wan_mac)
        return
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    persistent = args.state_dir.resolve() if args.state_dir else None
    lease_files = ("wan-pd.dhcp6.json", "wan-pd.dhcp6.json.duid", "wan4.dhcp4.json", "lan-info.dhcp6-server-duid")
    if persistent:
        (persistent / "leases").mkdir(parents=True, mode=0o700, exist_ok=True)
        mac_file = persistent / "wan-mac"
        if mac_file.exists():
            args.wan_mac = mac_file.read_text().strip()
        else:
            mac_file.write_text(args.wan_mac + "\n")
            mac_file.chmod(0o600)
    stage = Path(run(root + ["mktemp", "-d", "/tmp/routerd-live.XXXXXX"]).strip())
    try:
        if persistent:
            for name in lease_files:
                source = persistent / "leases" / name
                if source.exists():
                    run(root + ["install", "-D", "-m", "600", str(source), str(stage / "state/leases" / name)])
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
                "--wan-mac",
                args.wan_mac,
            ],
            check=True,
            timeout=args.seconds + 240,
        )
    finally:
        try:
            if persistent:
                for name in lease_files:
                    source = stage / "state/leases" / name
                    if subprocess.run(root + ["test", "-f", str(source)]).returncode == 0:
                        run(root + ["install", "-m", "600", "-o", str(os.getuid()), "-g", str(os.getgid()), str(source), str(persistent / "leases" / name)])
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
