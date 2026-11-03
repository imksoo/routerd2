#!/usr/bin/env python3
"""Exercise a locally attached access network with a separate MAC and LAN clients.

The actual product owns RA, DHCPv4, DHCPv6 PD, LAN RA, DNS and DS-Lite. The host's
management addresses/routes/clock are not changed. Requires an explicit uplink
and either a native ResourceProgram or an AFTR for the generated fixture, plus
root/sudo, iproute2, BusyBox, dhcpcd, dnsmasq, curl, dig and tcpdump. Native-profile
checks also use independent bundled chronyd peers/clients with clock adjustment disabled,
and installed FRR/Zebra/vtysh as an independent routing peer. A bounded
owner removes its namespaces on completion. An optional private state directory
retains the test router's identity and WAN leases. This is not NIC qualification.
"""

import argparse
import copy
import ipaddress
import json
import os
import re
import runpy
from pathlib import Path
import signal
import sqlite3
import subprocess
import tarfile
import time


def run(args, timeout=12):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"{args[0]} exited {result.returncode}: stderr={result.stderr.strip()!r}; stdout={result.stdout.strip()!r}")
    return result.stdout


def owner(stage, uplink, aftr, seconds, dns_upstream, wan_mac, wan_outage, full_restart, tailnet, config_path, retention_check):
    def stop(*_):
        raise KeyboardInterrupt()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGALRM, stop)
    if seconds:
        signal.alarm(seconds + 600)
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
    dns_peer_address = "198.18.0.254"
    children, namespaces = [], []
    capture = None
    pd_capture = None

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

    def wait(condition, seconds=90, diagnostic=None):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if condition():
                return
            time.sleep(0.3)
        detail = ": " + diagnostic() if diagnostic else ""
        raise RuntimeError("actual uplink readiness deadline" + detail)

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
    if config_path is not None:
        roles.extend(["ntp", "web_console", "bgp", "health", "syslog", "otlp_logs", "traffic_flow", "firewall_log"])
    boot = dict(
        platform="host",
        config=str(config_path or stage / "config.json"),
        runtime_dir=str(stage / "run"),
        state_dir=str(stage / "state"),
        ssh={"user": "admin"},
        engines={"dhcp_server": str(stage / "dnsmasq-engine.tar")},
        workers={
            role: str(
                stage / ("routerd-logging" if role == "logging" else "routerd-modules")
            )
            for role in roles
        },
    )
    if config_path is not None:
        boot["engines"]["ntp"] = str(stage / "chrony-engine.tar")
        management_key = stage / "management-key"
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(management_key)])
        boot["ssh"] = {"listen": "198.19.0.1:2222", "user": "admin", "authorized_keys": [management_key.with_suffix(".pub").read_text().strip()]}
    (stage / "boot.json").write_text(json.dumps(boot))
    if config_path is None:
        nat_rules = [{"type": "masquerade", "match": {"family": 4, "source": "198.18.0.0/24", "out_interface": "dslite0"}}]
        resources = [
            {"id": "wan", "interface": {"name": "wan0", "ipv4_rp_filter": 0}},
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
                    "mss_clamp": [
                        {"from": a, "to": b, "family": f, "mtu": mtu}
                        for a, b, f, mtu in [
                            ("lan0", "dslite0", 4, 1454),
                            ("dslite0", "lan0", 4, 1454),
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
                            },
                            {"name": "b4", "interface": "wan0", "host_id": 256, "prefix_length": 128},
                        ],
                    },
                },
            },
            {"id": "wan-pd-lan-ra", "when": {"fact": "service.wan-pd.active"}, "service": {"name": "wan-pd-lan-ra", "ra": {"interface": "lan0", "other_config": True, "router_lifetime_seconds": 1800, "dns_lifetime_seconds": 1800, "min_interval_seconds": 3, "max_interval_seconds": 10, "from": {"service": "wan-pd", "delegation": "lan", "rdnss_router": True}}}},
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
                        "mtu": 1454,
                        "metric": 200,
                    },
                },
            },
            {
                "id": "leases",
                "service": {"name": "leases", "dhcp_server": {
                    "interface": "lan0", "server_address": "198.18.0.1", "sticky_hold_days": 3, "authoritative": True,
                    "primary": {"prefix": "198.18.0.0/24", "start": "198.18.0.2", "end": "198.18.0.20",
                                "lease_seconds": 60, "router": "198.18.0.1", "dns_servers": ["198.18.0.1"]},
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
                            "queryLog": {"enabled": True},
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
            "upstreams": [f"udp://{dns_peer_address}:5353"], "queryTimeout": "1s",
        })
        if dns_upstream:
            source = resources[-1]["service"]["dns"]["spec"]["sources"][-1]
            source["upstreams"] = [dns_upstream]
            source["bootstrapResolverFrom"] = source.pop("upstreamFrom")
        resources.append({"id": "nat", "nat": nat_rules})
        resources.append({"id": "host-wan", "host_route": {
            "interface": "wan0", "router_from": "service.wan-ra", "table": 120, "priority": 10120,
            "bypasses": [{"service": "service.internet", "priority": 10100}],
        }})
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
        run(["ip", "-n", client, "address", "add", dns_peer_address + "/24", "dev", "client0"])

        if config_path is not None:
            run(["ip", "link", "add", "mgmt0", "netns", router, "type", "veth", "peer", "name", "mgmt-peer", "netns", client])
            run(["ip", "-n", client, "address", "add", "198.19.0.2/24", "dev", "mgmt-peer"])
            run(["ip", "-n", client, "link", "set", "mgmt-peer", "up"])

        if config_path is not None:
            # Independent LAN gateway, sharing the authorized HGW attachment.
            # Its DHCP hook owns only this namespace's address/default route.
            gateway = "r2gateway-" + str(os.getpid())
            run(["ip", "netns", "add", gateway])
            namespaces.append(gateway)
            run(["ip", "-n", gateway, "link", "set", "lo", "up"])
            run(inside(client, ["ip", "link", "add", "link", "client0", "name", "gateway-lan", "netns", gateway, "type", "macvlan", "mode", "bridge"]))
            run(["ip", "-n", gateway, "address", "add", "198.18.0.253/24", "dev", "gateway-lan"])
            run(["ip", "-n", gateway, "link", "set", "gateway-lan", "up"])
            run(["ip", "link", "add", "link", uplink, "name", "gateway-wan", "netns", gateway, "type", "macvlan", "mode", "bridge"])
            gateway_mac = (int(wan_mac.replace(":", ""), 16) + 1).to_bytes(6, "big")
            run(["ip", "-n", gateway, "link", "set", "gateway-wan", "address", ":".join(f"{b:02x}" for b in gateway_mac), "up"])
            gateway_hook = stage / "gateway-dhcp-hook.sh"
            gateway_hook.write_text('#!/bin/sh\ncase "$1" in bound|renew)\n'
                'ip address replace "$ip/$subnet" dev "$interface" || exit 1\n'
                'set -- $router; ip route replace default via "$1" dev "$interface" || exit 1\n'
                'esac\n')
            gateway_hook.chmod(0o500)
            start(gateway, ["busybox", "udhcpc", "-f", "-i", "gateway-wan", "-s", str(gateway_hook),
                "-p", str(stage / "gateway-dhcp.pid"), "-t", "3", "-T", "1", "-A", "1"], "gateway-dhcp")
            wait(lambda: bool(json.loads(run(["ip", "-n", gateway, "-j", "route", "show", "default"]))))
            run(inside(gateway, ["sysctl", "-qw", "net.ipv4.ip_forward=1"]))
            gateway_nat = stage / "gateway-nat.nft"
            gateway_nat.write_text('table ip independent_gateway {\n chain nat {\n type nat hook postrouting priority srcnat;\n ip saddr 198.18.0.0/24 oifname "gateway-wan" masquerade\n }\n}\n')
            run(inside(gateway, ["nft", "-f", str(gateway_nat)]))
            start(gateway, ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv",
                "--bind-interfaces", "--listen-address=198.18.0.253", "--server=1.1.1.1", "--pid-file=" + str(stage / "gateway-dns.pid")], "gateway-dns")
            wait(lambda: bool(run(inside(gateway, ["ss", "-H", "-lun", "sport = :53"]))))

        def start_private_dns(port, answer):
            process = start(
                client,
                [
                    "dnsmasq",
                    "--keep-in-foreground",
                    "--conf-file=/dev/null",
                    "--no-hosts",
                    "--no-resolv",
                    "--bind-interfaces",
                    "--listen-address=" + dns_peer_address,
                    "--port=" + str(port),
                    "--local-ttl=1",
                    "--address=/#/" + answer,
                ],
                "private-dns-" + str(port),
            )
            def responding():
                if process.poll() is not None:
                    raise RuntimeError((stage / ("private-dns-" + str(port) + ".log")).read_text())
                response = subprocess.run(inside(client, ["dig", "@" + dns_peer_address, "-p", str(port), "ready.split.test", "A", "+short", "+time=1", "+tries=1"]), capture_output=True, text=True)
                return response.stdout.strip() == answer
            wait(responding, 10)
            return process

        if config_path is not None:
            bgp_peers, bgp_processes = [], {}
            echo = runpy.run_path(str(stage / "peer-fixture.py"))

            def start_bgp(namespace, address, root):
                return start(namespace, ["/usr/lib/frr/bgpd", "-S", "-z", str(root / "zserv.api"),
                    "--vty_socket", str(root), "-i", str(root / "bgpd.pid"),
                    "-f", str(root / "bgpd.conf"), "--log", "stdout"], "bgp-peer-" + address)

            for index, address in enumerate(("192.0.2.2", "192.0.2.3")):
                namespace = "r2bgp" + str(index) + "-" + str(os.getpid())
                run(["ip", "netns", "add", namespace])
                namespaces.append(namespace)
                run(["ip", "-n", namespace, "link", "set", "lo", "up"])
                if index == 0:
                    run(["ip", "link", "add", "bgp0", "netns", router, "type", "veth", "peer", "name", "underlay0", "netns", namespace])
                    peer_interface = "underlay0"
                else:
                    peer_interface = "peer0"
                    run(inside(bgp_peers[0][0], ["ip", "link", "add", "link", "underlay0", "name", peer_interface,
                        "netns", namespace, "type", "macvlan", "mode", "bridge"]))
                run(["ip", "-n", namespace, "address", "add", address + "/24", "dev", peer_interface])
                run(["ip", "-n", namespace, "link", "set", peer_interface, "up"])
                run(["ip", "-n", namespace, "address", "add", "203.0.113.1/24", "dev", "lo"])
                # FRR drops privileges. Its public test config lives in the
                # owner's private /run mount, outside the private key stage.
                root = Path("/run/routerd-bgp-peer" + str(index))
                root.mkdir(mode=0o755)
                run(["chown", "frr:frr", str(root)])
                (root / "zebra.conf").write_text("hostname independent\nlog stdout\n")
                (root / "bgpd.conf").write_text(
                    "hostname independent\nlog stdout\nrouter bgp 64512\n bgp router-id " + address + "\n"
                    " no bgp ebgp-requires-policy\n neighbor 192.0.2.1 remote-as 64500\n"
                    " address-family ipv4 unicast\n  network 203.0.113.0/24\n  neighbor 192.0.2.1 activate\n exit-address-family\n"
                )
                start(namespace, ["/usr/lib/frr/zebra", "-u", "frr", "-g", "frr", "-z", str(root / "zserv.api"),
                    "--vty_socket", str(root), "-i", str(root / "zebra.pid"), "-f", str(root / "zebra.conf"), "--log", "stdout"], "bgp-zebra-" + address)
                wait(lambda: (root / "zserv.api").exists())
                bgp_processes[address] = start_bgp(namespace, address, root)
                bgp_peers.append((namespace, address, root))
                name = "bgp-echo-" + address
                start(namespace, ["python3", "-u", "-c", echo["ECHO_SERVER"], "203.0.113.1"], name)
                wait(lambda: "READY" in (stage / (name + ".log")).read_text())

        if config_path is not None:
            log_peer = bgp_peers[0][0]
            syslog_rows = stage / "received-syslog.jsonl"
            start(log_peer, ["python3", "-I", str(stage / "syslog-peer.py"), "--mode", "receive",
                "--port", "5514", "--output", str(syslog_rows)], "syslog-receiver")
            wait(lambda: syslog_rows.exists() and bool(syslog_rows.read_text().strip()))
            collector_config = stage / "collector.json"
            telemetry_rows = stage / "received-telemetry.jsonl"
            collector_config.write_text(json.dumps({
                "receivers": {"otlp": {"protocols": {"grpc": {"endpoint": "192.0.2.2:4317"}}}},
                "exporters": {"file": {"path": str(telemetry_rows), "flush_interval": "100ms", "rotation": {"max_megabytes": 8, "max_backups": 1}}},
                "service": {"telemetry": {"logs": {"level": "error"}, "metrics": {"level": "none"}},
                    "pipelines": {signal: {"receivers": ["otlp"], "exporters": ["file"]} for signal in ("logs", "metrics", "traces")}}
            }))
            collector = start(log_peer, ["/usr/local/bin/otelcol-contrib", "--config", str(collector_config)], "otlp-receiver")
            def collector_listening():
                if collector.poll() is not None:
                    raise RuntimeError((stage / "otlp-receiver.log").read_text())
                return bool(run(inside(log_peer, ["ss", "-H", "-ltn", "sport = :4317"])))
            wait(collector_listening, 10)

        private_dns = start_private_dns(5353, "192.0.2.77")
        if config_path is not None:
            backup_dns = start_private_dns(5354, "192.0.2.78")
        if config_path is not None:
            chrony_root = stage / "chrony-peer"
            chrony_root.mkdir()
            with tarfile.open(stage / "chrony-engine.tar") as archive:
                archive.extractall(chrony_root, filter="data")
            chrony_loader = next((chrony_root / "lib").glob("ld-musl-*.so.1"))
            chrony = [str(chrony_loader), "--library-path", str(chrony_root / "lib") + ":" + str(chrony_root / "usr/lib"), str(chrony_root / "usr/sbin/chronyd")]
            ntp_source = stage / "ntp-source.conf"
            ntp_source.write_text(
                "local stratum 8\n"
                f"bindaddress {dns_peer_address}\nallow 198.18.0.0/24\ncmdport 0\n"
                f"bindcmdaddress {stage}/ntp-source.sock\npidfile {stage}/ntp-source.pid\n"
                f"driftfile {stage}/ntp-source.drift\n"
            )
            start(client, chrony + ["-d", "-x", "-u", "root", "-f", str(ntp_source)], "ntp-source")
            ntp_client = stage / "ntp-client.conf"
            ntp_client.write_text("server 198.18.0.1 iburst\ncmdport 0\n")
        run(["ip", "-n", client, "link", "add", "link", "client0", "name", "client1", "type", "macvlan", "mode", "bridge"])
        run(["ip", "-n", client, "link", "set", "client1", "netns", automatic])
        run(["ip", "-n", automatic, "link", "set", "client1", "address", "02:00:00:00:00:03"])
        run(["ip", "-n", automatic, "link", "set", "client1", "up"])
        if tailnet:
            # A separate, explicitly fixed test underlay reaches the independent
            # control/peer. Product Internet traffic still uses the real HGW.
            remote = "r2tail-" + str(os.getpid())
            run(["ip", "netns", "add", remote])
            namespaces.append(remote)
            run(["ip", "-n", remote, "link", "set", "lo", "up"])
            run(["ip", "link", "add", "peer0", "netns", router, "type", "veth", "peer", "name", "underlay0", "netns", remote])
            control_address = "198.51.100.1" if config_path is not None else "192.0.2.1"
            run(["ip", "-n", remote, "addr", "add", control_address + "/24", "dev", "underlay0"])
            run(["ip", "-n", remote, "link", "set", "underlay0", "up"])
            peerbin = stage / "tailnet"
            cert, key = peerbin / "cert.pem", peerbin / "key.pem"
            run(["openssl", "req", "-config", "/dev/null", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-subj", "/CN=routerd-uplink", "-addext", "subjectAltName=IP:" + control_address])
            hsconfig = peerbin / "headscale.json"
            hsconfig.write_text(json.dumps({
                "server_url": "https://" + control_address + ":8443", "listen_addr": control_address + ":8443",
                "noise": {"private_key_path": str(peerbin / "noise.key")},
                "prefixes": {"v4": "100.64.0.0/10", "v6": "fd7a:115c:a1e0::/48"},
                "database": {"type": "sqlite", "sqlite": {"path": str(peerbin / "db.sqlite")}},
                "derp": {"urls": [], "paths": [], "auto_update_enabled": False, "server": {
                    "enabled": True, "region_id": 999, "region_code": "fixture", "region_name": "isolated DERP",
                    "stun_listen_addr": "0.0.0.0:3478", "private_key_path": str(peerbin / "derp.key"),
                    "automatically_add_embedded_derp_region": True, "ipv4": control_address}},
                "dns": {"override_local_dns": False, "magic_dns": True, "base_domain": "test.invalid"},
                "unix_socket": str(peerbin / "headscale.sock"), "tls_cert_path": str(cert), "tls_key_path": str(key),
                "disable_check_updates": True, "log": {"level": "warn"},
            }))
            head = [str(peerbin / "headscale"), "-c", str(hsconfig)]
            start(remote, head + ["serve"], "tailnet-control")
            wait(lambda: (peerbin / "headscale.sock").exists())
            user = json.loads(run(inside(remote, head + ["users", "create", "fixture", "-o", "json"])))
            auth = json.loads(run(inside(remote, head + ["preauthkeys", "create", "--user", str(user["id"]), "--reusable", "-o", "json"])))
            authfile = Path("/run/routerd-uplink/tailscale.key") if config_path is not None else peerbin / "auth.key"
            authfile.parent.mkdir(mode=0o700, exist_ok=True)
            if config_path is not None:
                system_roots = Path("/etc/ssl/certs/ca-certificates.crt")
                trusted = peerbin / "system-ca.pem"
                trusted.write_bytes(system_roots.read_bytes() + cert.read_bytes())
                run(["mount", "--bind", str(trusted), str(system_roots)])
            authfile.write_text(auth["key"])
            authfile.chmod(0o600)
            boot["workers"]["tailscale"] = str(stage / "routerd-modules")
            boot["engines"]["tailscale"] = str(peerbin / "tailscale-engine.tar")
            if config_path is None:
                resources += [
                    {"id": "tailnet-underlay", "interface": {"name": "peer0"}},
                    {"id": "tailnet-underlay-ip", "address": {"interface": "peer0", "cidr": "192.0.2.2/24"}},
                    {"id": "tailnet", "service": {"name": "tailnet", "tailscale": {
                        "interface": "tailscale0", "server_url": "https://192.0.2.1:8443", "hostname": "hgw-router",
                        "auth_key_file": str(authfile), "control_ca_file": str(cert),
                        "advertise_routes": ["198.18.0.0/24"], "advertise_exit_node": True,
                    }}},
                ]
                firewall = next(r["firewall"] for r in resources if "firewall" in r)
                firewall["forward"] += [{"from": "tailscale0", "to": name} for name in ("lan0", "dslite0", "wan0")]
                firewall["forward"].append({"from": "lan0", "to": "tailscale0"})
                nat_rules += [
                    {"type": "masquerade", "match": {"family": 4, "source": "100.64.0.0/10", "out_interface": "dslite0"}},
                    {"type": "masquerade", "match": {"family": 6, "source": "fd7a:115c:a1e0::/48", "out_interface": "wan0"}},
                ]
                # No delegated IPv6 subnet is advertised in this profile. Make
                # outbound LAN IPv6 use the router's tailnet identity explicitly;
                # upstream's local-destination filter otherwise drops its replies.
                nat_rules.append({"type": "masquerade", "match": {
                    "family": 6, "in_interface": "lan0", "out_interface": "tailscale0"}})
                (stage / "config.json").write_text(json.dumps({"schema_version": 2, "resources": resources}))
            (stage / "boot.json").write_text(json.dumps(boot))
            peer_socket = peerbin / "peer.sock"
            start(remote, ["tcpdump", "--immediate-mode", "-Z", "root", "-U", "-n", "-i", "any", "-C", "4", "-W", "2", "-w", str(stage / "peer.pcap"), "ip6 or udp port 41641"], "tailnet-capture")
            start(remote, ["env", "SSL_CERT_FILE=" + str(cert), "TS_LOGS_DIR=" + str(peerbin), str(peerbin / "tailscaled"), "--state=" + str(peerbin / "peer.state"), "--statedir=" + str(peerbin), "--socket=" + str(peer_socket), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "tailnet-peer")
            wait(peer_socket.exists)
            peer_cli = [str(peerbin / "tailscale"), "--socket=" + str(peer_socket)]
            run(inside(remote, ["env", "SSL_CERT_FILE=" + str(cert)] + peer_cli + ["up", "--timeout=15s", "--login-server=https://" + control_address + ":8443", "--hostname=independent-peer", "--auth-key=file:" + str(authfile), "--accept-dns=false", "--accept-routes=true", "--netfilter-mode=off"]), timeout=20)
            echo = runpy.run_path(str(peerbin / "peer-fixture.py"))
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
        pd_capture = start(router, ["tcpdump", "--immediate-mode", "-Z", "root", "-n", "-U",
                                   "-i", "any", "-w", str(stage / "pd.pcap"), "udp port 546"], "pd-capture")
        def pd_capture_ready():
            if pd_capture.poll() is not None:
                raise RuntimeError((stage / "pd-capture.log").read_text())
            path = stage / "pd.pcap"
            return path.exists() and path.stat().st_size >= 24
        wait(pd_capture_ready)
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
        if config_path is not None:
            management = next(m for m in query("supervisor.sock", "/v1/status")["modules"] if m["name"] == "management")
            assert management["generation"] == 1 and management["restarts"] == 0, "SSH restarted while waiting for its configured address: " + json.dumps(management)
            assert run(inside(router, ["sysctl", "-n", "net.netfilter.nf_conntrack_tcp_be_liberal"])).strip() == "1", "configured conntrack TCP liberal mode was not applied"
            wait(lambda: query("control.sock", "/v1/services/service.wan4").get("snapshot", {}).get("active", False))
            wan4 = query("control.sock", "/v1/services/service.wan4")["snapshot"]
            assert ipaddress.ip_address(wan4["currentAddress"]).version == 4, "WAN DHCPv4 did not publish an IPv4 lease"
            print("PASS actual HGW DHCPv4 client received and applied its observed WAN lease", flush=True)
        lan_domain = "home.arpa"
        if config_path is not None:
            configured = query("control.sock", "/v1/config")["config"]
            core = next(r["service"]["dns"] for r in configured["resources"] if r["id"] == "service/dns-core")
            lan_domain = core["zones"][0]["spec"]["zone"].rstrip(".")
        lease_events = stage / "client-lease-events"
        client_options = stage / "client-dhcp-options"
        hook = stage / "client-dhcp-hook.sh"
        hook.write_text(
            '#!/bin/sh\ncase "$1" in\n'
            'bound|renew)\n'
            '  ip address replace "$ip/24" dev "$interface" || exit 1\n'
            '  ip route replace default via "$router" dev "$interface" || exit 1\n'
            '  printf "%s|%s\\n" "$ntpsrv" "$search" > "' + str(client_options) + '"\n'
            '  printf "%s %s %s %s\\n" "$1" "$ip" "$router" "$dns" >> "' + str(lease_events) + '";;\n'
            'esac\n'
        )
        hook.chmod(0o500)
        dhcp_client = start(client, ["busybox", "udhcpc", "-f", "-i", "client0", "-s", str(hook), "-p", str(stage / "client-dhcp.pid"), "-t", "3", "-T", "1", "-A", "1", "-x", "hostname:uplink-client", "-O", "42", "-O", "119"], "client-dhcp")
        wait(lambda: lease_events.exists() and bool(lease_events.read_text().splitlines()))
        lease = lease_events.read_text().splitlines()[-1].split()
        ipaddress.IPv4Address(lease[1])
        assert lease[0] == "bound" and lease[2:] == ["198.18.0.1", "198.18.0.1"], "LAN DHCP did not supply an address, gateway and DNS: " + str(lease)
        if config_path is not None:
            assert ipaddress.IPv4Address(lease[1]) == ipaddress.IPv4Address("198.18.0.11"), "reserved client did not receive its configured reservation: " + str(lease)
        print("PASS independent LAN client acquired IPv4 address, gateway and DNS from routerd2 DHCP", flush=True)

        wait(lambda: subprocess.run(inside(router, ["dig", "@" + dns_peer_address, "-p", "5353", "ready.split.test", "A", "+short", "+time=1", "+tries=1"]), capture_output=True, text=True).stdout.strip() == "192.0.2.77")
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

        def current_lease():
            return lease_events.read_text().splitlines()[-1].split()

        info_hook = stage / "client-dhcp6-hook.sh"
        info_hook.write_text('#!/bin/sh\ncase "$1" in bound|renew) printf "dns=%s\\nsearch=%s\\n" "$dns" "$search";; esac\n')
        info_hook.chmod(0o500)
        information = run(inside(client, ["busybox", "udhcpc6", "-l", "-f", "-n", "-q", "-i", "client0", "-s", str(info_hook), "-t", "3", "-T", "1"]))
        options = dict(line.split("=", 1) for line in information.splitlines() if line.startswith(("dns=", "search=")))
        servers = [ipaddress.IPv6Address(value) for value in options.get("dns", "").split()]
        assert servers and lan_domain in options.get("search", "").split(), "LAN DHCPv6 information did not supply DNS and the configured search domain: " + str(options)
        print("PASS independent LAN client received router DNS and search domain via DHCPv6", flush=True)

        auto_events = stage / "automatic-client-events"
        auto_dhcp_options = stage / "automatic-dhcp-options"
        auto_hook = stage / "automatic-client-hook.sh"
        auto_hook.write_text(
            '#!/bin/sh\ncase "$reason" in\n'
            'BOUND|RENEW) printf "%s|%s\\n" "$new_ntp_servers" "$new_domain_search" > "' + str(auto_dhcp_options) + '"\n'
            'printf "v4|%s|%s|%s|%s\\n" "$reason" "$new_ip_address" "$new_routers" "$new_domain_name_servers" >> "' + str(auto_events) + '";;\n'
            'ROUTERADVERT) printf "ra|%s\\n" "$nd1_rdnss1_servers" >> "' + str(auto_events) + '";;\n'
            'INFORM6) printf "v6|%s|%s\\n" "$new_dhcp6_name_servers" "$new_dhcp6_domain_search" >> "' + str(auto_events) + '";;\n'
            'esac\n'
            'resolvconf=/nonexistent; skip_hooks="01-test 30-hostname 50-ntp.conf 50-timesyncd.conf dhcpcd.enter-hook dhcpcd.exit-hook"\n'
            'export resolvconf skip_hooks\n'
            'exec /usr/lib/dhcpcd/dhcpcd-run-hooks "$@"\n'
        )
        auto_hook.chmod(0o500)
        auto_config = stage / "automatic-client.conf"
        auto_config.write_text("ipv6rs\nslaac hwaddr\nnoipv4ll\noption domain_name_servers, domain_name, domain_search, ntp_servers\n")
        automatic_client = start(automatic, ["dhcpcd", "-B", "-d", "-f", str(auto_config), "-c", str(auto_hook), "client1"], "automatic-client")

        def automatic_ready():
            if automatic_client.poll() is not None:
                raise RuntimeError("automatic independent LAN client exited")
            if not auto_events.exists():
                return False
            events = [line.split("|") for line in auto_events.read_text().splitlines()]
            return any(len(event) == 5 and event[0] == "v4" and event[3:] == ["198.18.0.1", "198.18.0.1"] for event in events) and any(len(event) == 3 and event[0] == "v6" and event[1] and lan_domain in event[2].split() for event in events) and any(event[0] == "ra" and len(event) == 2 and event[1] for event in events)
        wait(automatic_ready, 60)
        auto_options = [line.split("|") for line in auto_events.read_text().splitlines()]
        auto_dns4 = next(event[4] for event in reversed(auto_options) if event[0] == "v4")
        if config_path is not None:
            auto_v4 = next(event for event in reversed(auto_options) if event[0] == "v4")
            auto_address = ipaddress.IPv4Address(auto_v4[2])
            assert ipaddress.IPv4Address("198.18.0.64") <= auto_address <= ipaddress.IPv4Address("198.18.0.191"), "unmatched client was outside the primary scope: " + str(auto_v4)
            assert auto_v4[3:] == ["198.18.0.1", "198.18.0.1"], "primary scope did not supply its configured gateway and DNS: " + str(auto_v4)
            print("PASS independent unmatched client received a dynamic lease from the primary scope", auto_v4[2], flush=True)
        auto_dns6 = [ipaddress.IPv6Address(address) for address in next(event[1] for event in reversed(auto_options) if event[0] == "v6").split()]
        auto_addresses = json.loads(run(["ip", "-n", automatic, "-j", "address", "show", "dev", "client1"]))
        delegated = [ipaddress.IPv6Network(p["prefix"]) for p in query("control.sock", "/v1/services/service.wan-pd")["snapshot"]["prefixes"]]
        ra_dns = [ipaddress.IPv6Address(address) for address in next(event[1] for event in reversed(auto_options) if event[0] == "ra" and len(event) == 2 and event[1]).split()]
        router_addresses = json.loads(run(["ip", "-n", router, "-j", "-6", "address", "show", "dev", "lan0"]))
        listening_addresses = {ipaddress.IPv6Address(a["local"]) for link in router_addresses for a in link["addr_info"] if a["family"] == "inet6"}
        router_dns = next((address for address in ra_dns if address in listening_addresses and any(address in prefix for prefix in delegated)), None)
        assert router_dns is not None, "RA did not advertise the delegated router DNS listener"
        assert router_dns in servers and router_dns in auto_dns6, "DHCPv6 information and RA did not both supply the router DNS listener"
        assert any(a["family"] == "inet6" and not a.get("tentative") and any(ipaddress.IPv6Address(a["local"]) in prefix for prefix in delegated) for link in auto_addresses for a in link["addr_info"]), "automatic LAN client has no usable delegated SLAAC address"
        print("PASS independent dhcpcd automatically acquired DHCPv4 and RA-triggered DHCPv6 with delegated SLAAC", flush=True)

        router_lan_ll = next(a["local"] for link in router_addresses for a in link["addr_info"] if a.get("scope") == "link")

        def forwarding_state():
            clients_ready = True
            for namespace, interface in ((client, "client0"), (automatic, "client1")):
                links = json.loads(run(["ip", "-n", namespace, "-j", "-6", "addr", "show", "dev", interface]))
                if not any(a.get("scope") == "global" and not a.get("tentative") and not a.get("deprecated") for link in links for a in link["addr_info"]):
                    clients_ready = False
                if not json.loads(run(["ip", "-n", namespace, "-j", "-6", "route", "show", "default"])):
                    clients_ready = False
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
                    current_lease()[1],
                    "iif",
                    "lan0",
                ],
                capture_output=True,
                text=True,
            )
            wan = json.loads(run(["ip", "-n", router, "-j", "-6", "addr", "show", "dev", "wan0"]))
            route_ok = route.returncode == 0 and json.loads(route.stdout)[0]["dev"] == "dslite0"
            b4_on_wan = any(
                a["local"] == observed.get("local") and a["prefixlen"] == 128
                for link in wan for a in link["addr_info"]
            )
            return {
                "lan_clients_ready": clients_ready,
                "internet_available": observed.get("available", False),
                "lan_ipv4_route_uses_dslite": route_ok,
                "b4_source_is_wan_host_address": b4_on_wan,
            }

        def forwarding_ready():
            state = forwarding_state()
            return all(state.values())

        def forwarding_diagnostic():
            return json.dumps(forwarding_state(), sort_keys=True)

        wait(forwarding_ready, diagnostic=forwarding_diagnostic)

        def wait_dns():
            started = time.clock_gettime_ns(time.CLOCK_BOOTTIME)

            def listening():
                snapshot = query("control.sock", "/v1/services/service.dns").get("snapshot", {})
                now = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
                if not started < snapshot.get("observed_nanos", 0) <= now < snapshot.get("expires_nanos", 0):
                    return False
                listeners = set(snapshot.get("listeners", []))
                return "[::]:53" in listeners or {lease[3] + ":53", "[" + str(router_dns) + "]:53"} <= listeners

            wait(listening)
            print("Observed fresh IPv4/IPv6 DNS listener readiness; seconds", round((time.clock_gettime_ns(time.CLOCK_BOOTTIME) - started) / 1e9, 2), flush=True)

        wait_dns()
        if config_path is not None:
            for name, expected in (("router." + lan_domain, "198.18.0.1"), ("storage." + lan_domain, "198.18.0.11")):
                answer = run(inside(client, ["dig", "@" + lease[3], name, "A", "+short", "+time=3", "+tries=1"]))
                assert answer.strip() == expected, f"LAN DNS {name} resolved to {answer.strip()!r}, expected configured address {expected}"
            answer = run(inside(client, ["dig", "-6", "@" + str(router_dns), "router." + lan_domain, "AAAA", "+short", "+time=3", "+tries=1"]))
            assert ipaddress.IPv6Address(answer.strip()) == router_dns, "LAN DNS router AAAA does not match the router's observed delegated address: " + answer
            print("PASS independent LAN client resolved router IPv4/IPv6 and its DHCP reservation through routerd2 DNS", flush=True)
        if tailnet:
            def tailnet_observation():
                return query("control.sock", "/v1/services/service.tailnet").get("snapshot", {})

            wait(lambda: tailnet_observation().get("backend_state") == "Running")
            tailnet_identity = tailnet_observation()["node_id"]
            product_ips = tailnet_observation()["addresses"]
            peer_status = json.loads(run(inside(remote, peer_cli + ["status", "--json"])))
            peer_ips = {ipaddress.ip_address(a).version: a for a in peer_status["TailscaleIPs"]}
            nodes = json.loads(run(inside(remote, head + ["nodes", "list", "-o", "json"])))
            node = next(n for n in nodes if n["name"] == "hgw-router")
            run(inside(remote, head + ["nodes", "approve-routes", "-i", str(node["id"]), "-r", "198.18.0.0/24,0.0.0.0/0,::/0"]))
            wait(lambda: any(product_ips[0] in p.get("TailscaleIPs", []) and p.get("ExitNodeOption")
                             for p in json.loads(run(inside(remote, peer_cli + ["status", "--json"]))).get("Peer", {}).values()))
            run(inside(remote, peer_cli + ["set", "--exit-node=" + product_ips[0], "--exit-node-allow-lan-access=true"]))
            auto4 = next(a["local"] for link in auto_addresses for a in link["addr_info"] if a["family"] == "inet")
            auto6 = next(a["local"] for link in auto_addresses for a in link["addr_info"] if a["family"] == "inet6" and any(ipaddress.IPv6Address(a["local"]) in p for p in delegated))
            for ns, addresses in ((automatic, (auto4,)), (remote, tuple(peer_ips.values()))):
                for address in addresses:
                    name = "tailnet-echo-" + address.replace(":", "_")
                    start(ns, ["python3", "-u", "-c", echo["ECHO_SERVER"], address], name)
                    wait(lambda: "READY" in (stage / (name + ".log")).read_text())

            def check_tailnet():
                assert tailnet_observation().get("node_id") == tailnet_identity, "router restart changed tailnet identity"
                for family, address in ((4, auto4), (6, auto6)):
                    # homert advertises IPv4 LANs only. Exit routes deliberately
                    # exclude unadvertised local LANs in upstream Tailscale.
                    source = next(a for a in product_ips if ipaddress.ip_address(a).version == 6) if family == 6 else address
                    flows = [(automatic, peer_ips[family], source)]
                    if family == 4:
                        # Upstream defaults to SNAT for traffic entering an advertised
                        # subnet; old routerd's tailscale up kept that default.
                        flows.append((remote, address, "198.18.0.1"))
                    for ns, destination, source in flows:
                        try:
                            run(inside(ns, ["python3", "-c", echo["ECHO_CLIENT"], destination, source]))
                        except RuntimeError:
                            print("Failed tailnet direction", "peer-to-LAN" if ns == remote else "LAN-to-peer", "family", family, flush=True)
                            for target in (remote, router, automatic):
                                print("Failed tailnet routes", target, run(inside(target, ["ip", "-6" if family == 6 else "-4", "route", "show", "table", "all"])), flush=True)
                            status = json.loads(run(inside(remote, peer_cli + ["status", "--json"])))
                            print("Failed tailnet peer state", {k: status.get(k) for k in ("BackendState", "Health")},
                                  [{k: p.get(k) for k in ("HostName", "ExitNode", "ExitNodeOption", "AllowedIPs", "PrimaryRoutes", "RxBytes", "TxBytes")} for p in status.get("Peer", {}).values()], flush=True)
                            for path in (*stage.glob("wan.pcap*"), *stage.glob("peer.pcap*")):
                                print("Failed tailnet packets", run(["tcpdump", "-nn", "-r", str(path), ("ip6" if family == 6 else "ip") + " and (tcp port 18080 or udp port 18081)"]).splitlines()[-40:], flush=True)
                            print("Failed tailnet peer log", "\n".join((stage / "tailnet-peer.log").read_text().splitlines()[-20:]), flush=True)
                            raise
                for family, address in (("-4", "1.1.1.1"), ("-6", "[2606:4700:4700::1111]")):
                    response = run(inside(remote, ["curl", family, "--noproxy", "*", "--max-time", "8", "--resolve", "one.one.one.one:443:" + address, "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
                    assert response in ("200", "301", "302", "308"), (family, response)
                    server = address.strip("[]")
                    try:
                        response = run(inside(remote, ["dig", family, "@" + server, "example.net", "A", "+tries=1", "+time=3"]))
                        assert "status: NOERROR" in response, (family, response)
                    except (RuntimeError, AssertionError):
                        packet_filter = ("ip6" if family == "-6" else "ip") + " and host " + server + " and udp port 53"
                        for path in (*stage.glob("wan.pcap*"), *stage.glob("peer.pcap*")):
                            capture = subprocess.run(["tcpdump", "-nn", "-r", str(path), packet_filter], capture_output=True, text=True, timeout=10)
                            print("Failed tailnet exit DNS packets", str(path.name), capture.returncode, capture.stdout.splitlines()[-30:], capture.stderr.strip(), flush=True)
                        for namespace in (remote, router):
                            for command in (["ip", "-6", "route", "get", server], ["conntrack", "-L", "-f", "ipv6", "-p", "udp", "--dport", "53"], ["nft", "list", "ruleset"]):
                                observed = subprocess.run(inside(namespace, command), capture_output=True, text=True, timeout=10)
                                print("Failed tailnet exit DNS state", namespace, command, observed.returncode, observed.stdout[-12000:], observed.stderr[-1000:], flush=True)
                        raise
            check_tailnet()
            print("PASS actual HGW automatic LAN -> Tailscale peer IPv4/IPv6 (explicit IPv6 NAT), advertised IPv4 LAN <- peer TCP/UDP; IPv4/IPv6 exit HTTPS/UDP DNS", flush=True)
        def check_dslite_paths():
            endpoints = []
            for index in range(4):
                service = "service.internet" + (str(index) if index else "")
                wait(lambda: query("control.sock", "/v1/services/" + service).get("snapshot", {}).get("available", False))
                endpoints.append(query("control.sock", "/v1/services/" + service)["snapshot"])
            assert len({e["local"] for e in endpoints}) == 4, "DS-Lite sources were not independent delegated addresses"
            for interface in ("dslite0", "dslite1", "dslite2", "dslite3"):
                response = run(inside(router, ["curl", "-4", "--interface", interface, "--noproxy", "*", "--max-time", "8", "--resolve", "one.one.one.one:443:1.1.1.1", "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
                assert response in ("200", "301", "302", "308"), (interface, response)
                run(inside(router, ["python3", "-c", """import socket,struct,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(3)
s.setsockopt(socket.SOL_SOCKET,socket.SO_BINDTODEVICE,sys.argv[1].encode()+bytes([0]))
s.connect(('1.1.1.1',53));s.send(bytes.fromhex('4d2101000001000000000000076578616d706c65036e65740000010001'))
r=s.recv(4096);ident,flags,questions,answers,_,_=struct.unpack('!6H',r[:12])
assert ident==0x4d21 and flags&0x800f==0x8000 and answers>0,(sys.argv[1],r.hex())
s.close()
""", interface]))
                print("PASS actual TCP/HTTPS and UDP/DNS on", interface, flush=True)

        def check_selected_upstream(marks):
            for membership in query("network.sock", "/v1/address-sets")["sets"]:
                if membership["name"] == "public-dns":
                    assert "9.9.9.10" not in membership.get("addresses", []), "WAN DNS probe is selected for local interception"
            def selection_applied():
                rules = run(inside(router, ["nft", "list", "chain", "inet", "routerd2", "flow_routes"]))
                if len(marks) > 1:
                    return "@flow_buckets_0" in rules
                assigned = {int(value, 0) for value in re.findall(r"meta mark set (0x[0-9a-f]+|[0-9]+)", rules)}
                return assigned == marks
            wait(selection_applied)
            # Use a DNS endpoint outside the configured local redirect sets.
            # Observe the mark on a new real UDP exchange; do not reproduce
            # the product's rendezvous algorithm in the test.
            for namespace in (client, automatic):
                address, port = run(inside(namespace, ["python3", "-c", """import socket,struct
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(4)
s.connect(('9.9.9.10',53));s.send(bytes.fromhex('4d2101000001000000000000076578616d706c65036e65740000010001'))
r=s.recv(4096);ident,flags,questions,answers,_,_=struct.unpack('!6H',r[:12])
assert ident==0x4d21 and flags&0x800f==0x8000 and answers>0,r.hex()
print(*s.getsockname())
"""])).split()
                connection = run(inside(router, ["conntrack", "-L", "-f", "ipv4", "-p", "udp", "--orig-src", address,
                    "--orig-dst", "9.9.9.10", "--sport", port, "--dport", "53"]))
                selected = re.search(r"\bmark=(\d+)", connection)
                assert selected and int(selected[1]) in marks, (marks, connection)
                mark = int(selected[1])
                route = json.loads(run(["ip", "-n", router, "-j", "route", "get", "9.9.9.10", "from", address, "iif", "lan0", "mark", str(mark)]))[0]
                expected = "lan0" if mark == 276 else "dslite" + str(mark - 272)
                assert route["dev"] == expected, route
                if mark == 276:
                    assert route["gateway"] == "198.18.0.253", route
                status = run(inside(namespace, ["curl", "-4", "--noproxy", "*", "--max-time", "8", "--resolve",
                    "one.one.one.one:443:1.1.1.1", "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
                assert status in ("200", "301", "302", "308"), status
                print("PASS LAN new UDP/DNS and TCP/HTTPS selected", expected, "mark", mark, flush=True)

        def check_upstream_recovery():
            names = ["dslite0", "dslite1", "dslite2", "dslite3", "static"]
            def health(name):
                return query("control.sock", "/v1/services/service.health-" + name).get("snapshot", {}).get("active", False)
            wait(lambda: all(health(name) for name in names))
            check_selected_upstream({272, 273, 274})
            sources = [query("control.sock", "/v1/services/service.internet" + (str(i) if i else ""))["snapshot"]["local"] for i in range(4)]
            rules = stage / "upstream-outage.nft"
            rules.write_text('table inet r2_upstream_outage {\n chain output {\n type filter hook output priority -10;\n ip6 saddr { ' + ', '.join(sources[:3]) + ' } meta l4proto 4 drop\n }\n}\n')
            run(inside(router, ["nft", "-f", str(rules)]))
            try:
                wait(lambda: not any(health(name) for name in names[:3]))
                check_selected_upstream({275})
                run(inside(router, ["nft", "add", "rule", "inet", "r2_upstream_outage", "output", "ip6", "saddr", sources[3], "meta", "l4proto", "4", "drop"]))
                wait(lambda: not health("dslite3"))
                check_selected_upstream({276})
            finally:
                run(inside(router, ["nft", "delete", "table", "inet", "r2_upstream_outage"]))
            wait(lambda: all(health(name) for name in names))
            check_selected_upstream({272, 273, 274})
            print("PASS upstream priority 120 -> 80 -> 60 -> 120 with actual Health, kernel marks/routes and both LAN clients", flush=True)

        if config_path is not None:
            check_dslite_paths()
            check_upstream_recovery()

        cached_answers = {}
        for server, family in ((lease[3], "-4"), (str(router_dns), "-6")):
            answer = run(inside(client, ["dig", family, "@" + server, "available.split.test", "A", "+nocookie", "+short", "+time=3", "+tries=1"])).strip()
            assert answer in ({"192.0.2.77", "192.0.2.78"} if config_path is not None else {"192.0.2.77"}), "conditional DNS reached the wrong upstream: " + answer
            cached_answers[server] = answer
        private_dns.terminate()
        private_dns.wait(timeout=3)
        if config_path is not None:
            for server, family in ((lease[3], "-4"), (str(router_dns), "-6")):
                answer = run(inside(client, ["dig", family, "@" + server, "failover.split.test", "A", "+short", "+time=3", "+tries=1"]))
                assert answer.strip() == "192.0.2.78", "conditional DNS did not use its remaining upstream: " + answer
            backup_dns.terminate()
            backup_dns.wait(timeout=3)
            for server, family in ((lease[3], "-4"), (str(router_dns), "-6")):
                cached = run(inside(client, ["dig", family, "@" + server, "available.split.test", "A", "+nocookie", "+short", "+time=3", "+tries=1"]))
                assert cached.strip() == cached_answers[server], "cached answer was lost when its upstreams stopped: " + cached
        failed = run(inside(client, ["dig", "@" + lease[3], "offline.split.test", "A", "+time=3", "+tries=1"]))
        assert "status: SERVFAIL" in failed, "unavailable conditional DNS source fell through to the default upstream"
        private_dns = start_private_dns(5353, "192.0.2.77")
        if config_path is not None:
            backup_dns = start_private_dns(5354, "192.0.2.78")
        wait(lambda: subprocess.run(inside(client, ["dig", "@" + lease[3], "recovered.split.test", "A", "+short", "+time=2", "+tries=1"]), capture_output=True, text=True).stdout.strip() in ({"192.0.2.77", "192.0.2.78"} if config_path is not None else {"192.0.2.77"}))
        print("PASS independent conditional DNS upstream availability, failure isolation and recovery over LAN IPv4/IPv6", flush=True)

        def check_dns_sources():
            for server, family in ((lease[3], "-4"), (str(router_dns), "-6")):
                for name in ("answer.branch.split.test", "answer.corp.test"):
                    answer = run(inside(client, ["dig", family, "@" + server, name, "A", "+short", "+time=3", "+tries=1"]))
                    assert answer.strip() == "192.0.2.78", "DNS suffix selected the wrong source: " + answer
                answer = run(inside(client, ["dig", family, "@" + server, "www.example.net", "A", "+time=3", "+tries=1"]))
                assert "status: NOERROR" in answer and re.search(r"\sIN\s+A\s+", answer), "WAN learned DNS did not resolve the public name: " + answer
            wait(lambda: any(q["upstream"] == "wan-learned" and q["responseCode"] == "NOERROR" for q in query("control.sock", "/v1/services/service.dns-core/dns-history?name=www.example.net&limit=20")["queries"]))
            configured = query("control.sock", "/v1/config")["config"]
            core = next(r["service"]["dns"] for r in configured["resources"] if r["id"] == "service/dns-core")
            endpoints = [url.strip() for source in core["spec"]["sources"] for url in source.get("upstreams", []) if url.strip().startswith("https://")]
            assert endpoints, "native DNS profile has no DoH upstream to exercise"
            def doh_ready():
                observed = query("services/dns-core.dns.sock", "/v1/status")["resources"][0]["observed"]["upstreams"]
                return all(endpoint + ":Healthy" in observed for endpoint in endpoints)
            wait(doh_ready)
            print("PASS multiple conditional suffixes, nested source precedence, WAN-learned DNS answers and all configured public DoH upstream probes", flush=True)

        def bgp_peer_states():
            snapshot = query("control.sock", "/v1/services/service.routing").get("snapshot", {})
            return {p["address"]: p["state"] for p in snapshot.get("peers", [])}

        def selected_bgp_peer():
            paths = json.loads(run(["ip", "-n", router, "-j", "route", "show", "203.0.113.0/24"]))
            return next((r.get("gateway", "") for r in paths if r.get("dev") == "bgp0"), "")

        def bgp_traffic():
            for namespace, interface in ((client, "client0"), (automatic, "client1")):
                addresses = json.loads(run(["ip", "-n", namespace, "-j", "-4", "address", "show", "dev", interface]))
                source = next(a["local"] for link in addresses for a in link["addr_info"] if a["local"] != dns_peer_address)
                run(inside(namespace, ["python3", "-c", echo["ECHO_CLIENT"], "203.0.113.1", source, source]))

        def check_bgp():
            wait(lambda: bgp_peer_states() == {p[1]: "SESSION_STATE_ESTABLISHED" for p in bgp_peers})
            wait(lambda: selected_bgp_peer() in bgp_processes)
            for namespace, address, root in bgp_peers:
                wait(lambda: any(r.get("gateway") == "192.0.2.1" for r in json.loads(run(["ip", "-n", namespace, "-j", "route", "show", "198.18.0.0/24"]))))
                peer_view = json.loads(run(inside(namespace, ["vtysh", "--vty_socket", str(root), "-d", "bgpd", "-c", "show bgp ipv4 unicast 198.18.0.0/24 json"])))
                assert peer_view.get("paths"), "independent FRR did not receive the LAN advertisement: " + address
            bgp_traffic()
            print("PASS two independent FRR peers received LAN routes; BGP selected a learned kernel path and both LAN clients completed TCP/UDP without NAT", flush=True)

        def check_bgp_peer_recovery():
            for namespace, address, root in bgp_peers:
                other = next(p[1] for p in bgp_peers if p[1] != address)
                stopped_at = time.monotonic()
                bgp_processes[address].terminate()
                bgp_processes[address].wait(timeout=5)
                def remaining_ready():
                    states = bgp_peer_states()
                    return states.get(address) != "SESSION_STATE_ESTABLISHED" and states.get(other) == "SESSION_STATE_ESTABLISHED" and selected_bgp_peer() == other
                wait(remaining_ready, 30)
                bgp_traffic()
                print("PASS BGP peer stopped; remaining peer carried both clients' TCP/UDP; seconds", round(time.monotonic() - stopped_at, 2), flush=True)
                bgp_processes[address] = start_bgp(namespace, address, root)
                check_bgp()
            print("PASS each independent BGP peer stopped and rejoined with routes and client traffic restored", flush=True)

        def check_management():
            for namespace, server in ((router, "127.0.0.1"), (client, "198.19.0.1")):
                answer = run(inside(namespace, ["dig", "@" + server, "router." + lan_domain, "A", "+short", "+time=3", "+tries=1"]))
                assert answer.strip() == "198.18.0.1", "loopback/management DNS did not resolve the configured LAN name: " + answer
            status = json.loads(run(inside(client, [
                "ssh", "-F", "/dev/null", "-i", str(management_key), "-o", "BatchMode=yes",
                "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=accept-new",
                "-o", "UserKnownHostsFile=" + str(stage / "known-hosts"), "-o", "ConnectTimeout=3",
                "-p", "2222", "admin@198.19.0.1", "show status",
            ])))
            assert status["ready"], "SSH reported the router unavailable"
            # LAN and core caches have separate owners. Exercise the core's own
            # loopback listener twice to observe a real core-cache hit in history.
            for _ in range(2):
                answer = run(inside(router, ["dig", "@127.0.0.1", "available.split.test", "A", "+nocookie", "+short", "+time=3", "+tries=1"]))
                assert answer.strip() in {"192.0.2.77", "192.0.2.78"}, answer
            history_path = "/v1/services/service.dns-core/dns-history?name=available.split.test.&limit=20"
            wait(lambda: any(q["cacheHit"] for q in query("control.sock", history_path)["queries"]))
            page = run(inside(client, ["curl", "--noproxy", "*", "--fail", "--max-time", "4", "-sS",
                "http://198.19.0.1:8080/dns-queries?service=service.dns-core&name=available.split.test.&limit=20"]))
            assert 'data-query="' in page and any(address in page for address in ("192.0.2.77", "192.0.2.78")), "Web console omitted the recorded DNS answer"
            denied = subprocess.run(inside(client, ["curl", "--interface", "client0", "--noproxy", "*", "--max-time", "2", "-sS", "http://198.19.0.1:8080/"]), capture_output=True)
            assert denied.returncode != 0, "management console was reachable through the LAN interface"
            print("PASS loopback/management DNS, cache during upstream outage, persistent query history, SSH host identity and Web access restricted to management interface", flush=True)

        special_clients = []
        def start_special_client(index, mac, requested=""):
            namespace = "r2special" + str(index) + "-" + str(os.getpid())
            run(["ip", "netns", "add", namespace])
            namespaces.append(namespace)
            run(["ip", "-n", namespace, "link", "set", "lo", "up"])
            run(inside(client, ["ip", "link", "add", "link", "client0", "name", "special0", "netns", namespace, "type", "macvlan", "mode", "bridge"]))
            run(["ip", "-n", namespace, "link", "set", "special0", "address", mac, "up"])
            events = stage / ("special" + str(index) + "-leases")
            hook = stage / ("special" + str(index) + "-hook.sh")
            hook.write_text('#!/bin/sh\ncase "$1" in bound|renew)\n'
                'ip address replace "$ip/$subnet" dev "$interface" || exit 1\n'
                'ip route replace default via "$router" dev "$interface" || exit 1\n'
                'printf "%s|%s|%s|%s\\n" "$1" "$ip" "$router" "$dns" >> "' + str(events) + '";;\n'
                'deconfig) ip -4 address flush dev "$interface" scope global; ip -4 route flush dev "$interface";;\nesac\n')
            hook.chmod(0o500)
            command = ["busybox", "udhcpc", "-f", "-i", "special0", "-s", str(hook), "-p", str(stage / ("special" + str(index) + ".pid")),
                "-t", "3", "-T", "1", "-A", "1", "-x", "hostname:special-" + str(index)]
            if requested:
                command += ["-r", requested]
            process = start(namespace, command, "special" + str(index) + "-dhcp")
            wait(lambda: events.exists() and bool(events.read_text().strip()))
            result = (namespace, process, events, mac)
            special_clients.append(result)
            return result

        def check_special_clients():
            for namespace, process, events, mac in special_clients:
                lease = events.read_text().splitlines()[-1].split("|")
                address = ipaddress.IPv4Address(lease[1])
                assert ipaddress.IPv4Address("198.18.0.32") <= address <= ipaddress.IPv4Address("198.18.0.63"), lease
                assert lease[2:] == ["198.18.0.253", "198.18.0.253"], lease
                default = json.loads(run(["ip", "-n", namespace, "-j", "route", "show", "default"]))
                assert default[0]["gateway"] == lease[2], default
                answer = run(inside(namespace, ["dig", "@" + lease[3], "example.net", "A", "+short", "+time=3", "+tries=1"]))
                assert answer.strip(), "special scope DNS did not resolve through its own gateway"
                run(inside(namespace, ["curl", "-4", "--noproxy", "*", "--fail", "--max-time", "8", "--resolve",
                    "one.one.one.one:443:1.1.1.1", "-sS", "-I", "https://one.one.one.one/"]))
                wait(lambda: bool(query("control.sock", "/v1/services/service.lan-leases/dhcp-fingerprints?mac=" + mac + "&limit=10").get("entries")))
            for options in (client_options, auto_dhcp_options):
                ntp, search = options.read_text().strip().split("|")
                assert set(ntp.split()) == {"198.18.0.1", "198.18.0.254"}, (options.name, ntp)
                assert lan_domain in search.split(), (options.name, search)
            print("PASS primary/reserved and OUI/exact-MAC scopes supplied their configured pool, gateway, DNS, NTP/search; special clients reached public DNS/HTTPS via the independent LAN gateway; DHCP fingerprints persisted", flush=True)

        def check_lan_distribution():
            first = start_special_client(0, "02:77:00:00:00:04")
            start_special_client(1, "02:77:01:00:00:05")
            original = first[2].read_text().splitlines()[-1].split("|")[1]
            first[1].send_signal(signal.SIGUSR2)
            def held():
                path = stage / "state/services/lan-leases/dhcp-sticky.db"
                if not path.exists():
                    return False
                with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as database:
                    row = database.execute("SELECT released_at, sticky_until FROM dhcp_sticky_leases WHERE mac=? AND ip=?", (first[3], original)).fetchone()
                if not row or not row[0]:
                    return False
                assert row[1] - row[0] == 3 * 86400 * 1_000_000_000, row
                return True
            wait(held)
            wait(lambda: not any(a.get("scope") == "global" for link in json.loads(run(["ip", "-n", first[0], "-j", "-4", "address", "show", "dev", "special0"])) for a in link["addr_info"]))
            third = start_special_client(2, "02:77:01:00:00:06", original)
            assert third[2].read_text().splitlines()[-1].split("|")[1] != original, "a different MAC acquired the released address during its configured sticky hold"
            previous = len(first[2].read_text().splitlines())
            first[1].send_signal(signal.SIGUSR1)
            wait(lambda: len(first[2].read_text().splitlines()) > previous)
            assert first[2].read_text().splitlines()[-1].split("|")[1] == original, "returning MAC did not recover its held address"
            check_special_clients()
            print("PASS actual DHCP release created a 3-day hold; another MAC requesting that address received a different lease, and the original MAC recovered its held address", flush=True)

        def check_operations():
            since = time.time_ns()
            run(inside(automatic, ["curl", "-4", "--noproxy", "*", "--fail", "--max-time", "8", "--resolve",
                "one.one.one.one:443:1.1.1.1", "-sS", "-I", "https://one.one.one.one/"]))
            answer = run(inside(automatic, ["dig", "@1.1.1.1", "example.net", "A", "+short", "+time=3", "+tries=1"]))
            assert answer.strip(), "independent client received no public DNS answer"
            observed_flows = []
            def classified():
                observed_flows[:] = query("control.sock", "/v1/services/service.flows/traffic-history?client=" + str(auto_address) + "&peer=1.1.1.1&limit=100")["flows"] or []
                return any(f.get("engine") == "ndpi" and f.get("source") == "libndpi" and f.get("accounting") and f.get("bytesOut", 0) > 0 and f.get("bytesIn", 0) > 0 for f in observed_flows) and any(f.get("tlsSNI") == "one.one.one.one" for f in observed_flows)
            wait(classified, diagnostic=lambda: json.dumps(observed_flows))
            wait(lambda: any(e.get("action") == "drop" and e.get("dstPort") == 8080 and e.get("inIface") == "lan0" for e in (query("control.sock", "/v1/services/service.packets/firewall-history?action=drop&limit=100")["entries"] or [])))
            wait(lambda: any(row.get("time", 0) * 1e9 >= since and "health probe completed" in row.get("message", "") for row in (json.loads(line) for line in syslog_rows.read_text().splitlines(keepends=True) if line.endswith("\n"))))
            def signals_received():
                if not telemetry_rows.exists():
                    return False
                batches = [json.loads(line) for line in telemetry_rows.read_text().splitlines(keepends=True) if line.endswith("\n")]
                logs = [record for batch in batches for resource in batch.get("resourceLogs", []) for scope in resource.get("scopeLogs", []) for record in scope.get("logRecords", [])]
                metrics = [metric for batch in batches for resource in batch.get("resourceMetrics", []) for scope in resource.get("scopeMetrics", []) for metric in scope.get("metrics", [])]
                spans = [span for batch in batches for resource in batch.get("resourceSpans", []) for scope in resource.get("scopeSpans", []) for span in scope.get("spans", [])]
                return (any(int(record.get("timeUnixNano", 0)) >= since and record.get("body", {}).get("stringValue") == "health probe completed" for record in logs)
                    and any(metric.get("name") == "routerd.healthcheck.probes" and any(int(point.get("timeUnixNano", 0)) >= since for point in metric.get("sum", {}).get("dataPoints", [])) for metric in metrics)
                    and any(span.get("name") == "healthcheck.probe" and int(span.get("startTimeUnixNano", 0)) >= since for span in spans))
            wait(signals_received)
            print("PASS public peer traffic has measured counters, native nDPI classification and TLS SNI; LAN denial persisted; actual product syslog and OTLP logs/metrics/traces reached independent receivers", flush=True)

        def check_time():
            after = time.clock_gettime_ns(time.CLOCK_BOOTTIME)

            def synchronized():
                snapshot = query("control.sock", "/v1/services/service.time").get("snapshot", {})
                return snapshot.get("synchronized", False) and snapshot.get("observed_nanos", 0) > after

            wait(synchronized)
            snapshot = query("control.sock", "/v1/services/service.time")["snapshot"]
            assert not snapshot["adjusting_clock"], "host-mode time service attempted to adjust the host clock"
            print("Observed NTP source", "fixture fallback" if snapshot.get("source") == dns_peer_address else "WAN learned", "stratum", snapshot["stratum"], flush=True)
            for namespace in (client, automatic):
                run(inside(namespace, chrony + ["-Q", "-t", "8", "-u", "root", "-f", str(ntp_client)]), timeout=12)
            print("PASS NTP synchronized and independent LAN chronyd clients received time; host clock adjustment disabled", flush=True)

        def check_local_redirects():
            configured = query("control.sock", "/v1/config")["config"]
            if not any(r.get("firewall", {}).get("local_redirects") for r in configured["resources"]):
                return
            sets = {row["name"]: row for row in query("network.sock", "/v1/address-sets")["sets"]}
            dns_addresses = [ipaddress.ip_address(value) for value in sets["public-dns"]["addresses"]]
            for family in (4, 6):
                destination = next(value for value in dns_addresses if value.version == family)
                for namespace in (client, automatic):
                    for transport in ("+notcp", "+tcp"):
                        answer = run(inside(namespace, ["dig", "@" + str(destination), "router." + lan_domain,
                            "A", "+short", "+time=3", "+tries=1", transport]))
                        assert answer.strip() == "198.18.0.1", (family, transport, answer)
            destination = next(value for value in sets["public-ntp"]["addresses"] if ipaddress.ip_address(value).version == 4)
            redirected_ntp = stage / "ntp-redirect-client.conf"
            redirected_ntp.write_text("server " + destination + " iburst\ncmdport 0\n")
            for namespace in (client, automatic):
                route = json.loads(run(["ip", "-n", namespace, "-j", "route", "get", destination]))[0]
                address = route["prefsrc"]
                run(inside(namespace, chrony + ["-Q", "-t", "8", "-u", "root", "-f", str(redirected_ntp)]), timeout=12)
                connection = run(inside(router, ["conntrack", "-L", "-f", "ipv4", "-p", "udp",
                    "--orig-src", address, "--orig-dst", destination, "--dport", "123"]))
                assert re.search(r"\bsrc=198\.18\.0\.1\b", connection), (address, destination, connection)
            print("PASS both LAN clients' public DNS IPv4/IPv6 UDP/TCP reached the local zone; public NTP reached the local server with conntrack destination translation", flush=True)

        if config_path is not None:
            check_dns_sources()
            check_lan_distribution()
            check_management()
            check_bgp()
            check_bgp_peer_recovery()
            check_operations()
            check_time()
            check_local_redirects()

        start(router, ["ip", "-ts", "monitor", "route"], "router-routes")

        def check_traffic():
            host_routes = json.loads(run(["ip", "-n", router, "-j", "-6", "route", "show", "table", "120", "default"]))
            assert len(host_routes) == 1 and host_routes[0].get("prefsrc"), "IPv6 host route/source is unavailable: " + json.dumps(host_routes)
            learned = query("control.sock", "/v1/services/service.wan-ra")["snapshot"]["addresses"]
            host_source = ipaddress.IPv6Address(host_routes[0]["prefsrc"])
            assert any(ipaddress.ip_interface(a["address"]).ip == host_source and not a.get("deprecated") and not a.get("dad_failed") for a in learned), "host policy guessed an address instead of using its RA owner"
            endpoint = query("control.sock", "/v1/services/service.internet")["snapshot"]
            bypass = json.loads(run(["ip", "-n", router, "-j", "-6", "route", "get", endpoint["remote"], "from", endpoint["local"]]))
            assert bypass[0]["dev"] == "wan0" and bypass[0].get("table", "main") in (254, "main"), "delegated DS-Lite source did not bypass the physical host policy"
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
            for family, server in (("-4", auto_dns4), ("-6", str(router_dns))):
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
                        current_lease()[1],
                        "iif",
                        "lan0",
                    ]
                )
            )
            assert selected[0]["dev"] == "dslite0", "IPv4 traffic bypassed the AFTR"

        if config_path is not None:
            # Disable the candidate LAN cache as well as changing the core zone:
            # both applications and rollback must be visible to the same client.
            for recovery in ("timeout", "restart"):
                wan_workers = {m["name"]: m["pid"] for m in query("supervisor.sock", "/v1/status")["modules"]
                    if m["name"] in ("service.wan4", "service.wan-ra", "service.wan-pd")}
                original = query("control.sock", "/v1/config")
                candidate = copy.deepcopy(original["config"])
                core = next(r["service"]["dns"] for r in candidate["resources"] if r["id"] == "service/dns-core")
                record = next(r for r in core["zones"][0]["spec"]["records"] if r["hostname"] == "router")
                original_answer = str(ipaddress.IPv4Address(record["ipv4"]))
                record["ipv4"] = "192.0.2.99"
                lan = next(r["service"]["dns"] for r in candidate["resources"] if r["id"] == "service/dns")
                lan["spec"]["cache"]["enabled"] = False
                request_id = "uplink-confirmed-" + recovery
                request = stage / "confirmed-request.json"
                request.write_text(json.dumps({"expected_revision": original["revision"],
                    "request_id": request_id, "config": candidate, "confirm_timeout_seconds": 30}))
                applied = json.loads(run(["curl", "--fail", "--max-time", "30", "-sS",
                    "--unix-socket", str(stage / "run/control.sock"), "-H", "Content-Type: application/json",
                    "--data-binary", "@" + str(request), "http://localhost/v1/config/commit"], timeout=35))
                assert applied["confirmation"]["request_id"] == request_id, applied

                def client_answers(expected):
                    for server in (lease[3], str(router_dns)):
                        for transport in ("+notcp", "+tcp"):
                            answer = subprocess.run(inside(client, ["dig", "@" + server,
                                "router." + lan_domain, "A", "+short", "+time=2", "+tries=1", transport]),
                                capture_output=True, text=True, timeout=4)
                            if answer.returncode or answer.stdout.strip() != expected:
                                return False
                    return True

                wait(lambda: client_answers("192.0.2.99"), 20)
                current_wan = {m["name"]: m["pid"] for m in query("supervisor.sock", "/v1/status")["modules"] if m["name"] in wan_workers}
                assert current_wan == wan_workers, ("DNS edit restarted unrelated WAN workers", wan_workers, current_wan)
                check_traffic()
                started = time.monotonic()
                if recovery == "restart":
                    control = next(m for m in query("supervisor.sock", "/v1/status")["modules"] if m["name"] == "control")
                    os.kill(control["pid"], signal.SIGKILL)

                def rolled_back():
                    response = subprocess.run(["curl", "--fail", "--max-time", "2", "-sS",
                        "--unix-socket", str(stage / "run/control.sock"), "http://localhost/v1/config"],
                        capture_output=True, text=True, timeout=4)
                    if response.returncode:
                        return False
                    state = json.loads(response.stdout)
                    return state["revision"] > applied["revision"] and not state.get("confirmation") and state["healthy"]

                wait(rolled_back, 45)
                restored = query("control.sock", "/v1/config")
                assert restored["config"] == original["config"], "rollback did not restore the complete original intent"
                changes = query("control.sock", "/v1/events?producer_epoch=" + restored["producer_epoch"] + "&after_sequence=0")
                assert any(e["type"] == "rolled_back_" + recovery and e.get("request_id") == request_id for e in changes["events"]), changes
                wait(lambda: client_answers(original_answer), 20)
                if recovery == "timeout":
                    current_wan = {m["name"]: m["pid"] for m in query("supervisor.sock", "/v1/status")["modules"] if m["name"] in wan_workers}
                    assert current_wan == wan_workers, ("DNS rollback restarted unrelated WAN workers", wan_workers, current_wan)
                check_traffic()
                print("PASS confirmed configuration rollback", recovery,
                    "restored full intent and client IPv4/IPv6 UDP/TCP DNS/Internet; seconds",
                    round(time.monotonic() - started, 2), flush=True)

        if wan_outage:
            check_traffic()
            if tailnet:
                check_tailnet()
            before_outage = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
            rules = (
                'add table inet r2_outage\n'
                'add chain inet r2_outage input { type filter hook input priority -300; policy accept; }\n'
                'add chain inet r2_outage output { type filter hook output priority -300; policy accept; }\n'
                'add rule inet r2_outage input iifname "wan0" drop\n'
                'add rule inet r2_outage output oifname "wan0" drop\n'
            )
            subprocess.run(inside(router, ["nft", "-f", "-"]), input=rules, text=True, capture_output=True, check=True, timeout=10)
            try:
                for family in ("-4", "-6"):
                    blocked = subprocess.run(inside(automatic, ["curl", family, "--noproxy", "*", "--max-time", "4", "-sS", "-I", "https://one.one.one.one/"]), capture_output=True, text=True)
                    assert blocked.returncode != 0, family + " communication survived the isolated WAN blackhole"
            finally:
                run(inside(router, ["nft", "delete", "table", "inet", "r2_outage"]))
            recovered_at = time.monotonic()
            def usable():
                for family in ("-4", "-6"):
                    result = subprocess.run(inside(automatic, ["curl", family, "--noproxy", "*", "--max-time", "4", "-sS", "-I", "-o", "/dev/null", "https://one.one.one.one/"]), capture_output=True, text=True)
                    if result.returncode:
                        return False
                return True
            wait(usable, 60)
            after_outage = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
            assert (after_outage["client_duid"], after_outage["iaid"]) == (before_outage["client_duid"], before_outage["iaid"]), "WAN recovery changed PD identity"
            check_traffic()
            print("PASS actual HGW WAN blackhole and IPv4/IPv6 recovery; retained PD identity; seconds", round(time.monotonic() - recovered_at, 2), flush=True)

        initial = query("control.sock", "/v1/services/service.wan-pd")["snapshot"][
            "renew_at"
        ]
        renew_due = initial["nanos"] / 1e9 - time.clock_gettime(time.CLOCK_BOOTTIME)
        print("Observed natural PD renewal due in seconds", round(renew_due), flush=True)
        if seconds == 0:
            seconds = max(75, int(renew_due) + 90)
            signal.alarm(seconds + 600)
            print("Waiting through observed PD T1 and 90 seconds for the renewal exchange", seconds, flush=True)
        started_at = time.time()
        start_time, rounds, updates = time.monotonic(), 0, 0
        while time.monotonic() - start_time < seconds:
            if dhcp_client.poll() is not None or automatic_client.poll() is not None:
                raise RuntimeError("independent LAN DHCP client exited")
            check_traffic()
            if tailnet:
                check_tailnet()
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
        if seconds >= 75 and config_path is None:
            assert any(event[0] == "renew" and event[2:] == ["198.18.0.1", "198.18.0.1"] for event in events), "LAN DHCP client did not renew its address, gateway and DNS"
            assert any(line.startswith("v4|RENEW|") for line in auto_events.read_text().splitlines()), "automatic LAN client did not naturally renew DHCPv4"
            print("PASS both independent LAN DHCP clients naturally renewed their addresses, gateway and DNS", flush=True)
        elif config_path is not None:
            print("Short native-profile run does not claim DHCPv4 T1 renewal", flush=True)
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
                instance.get("generation", 0) > previous["generation"]
                and instance.get("snapshot", {}).get("state") == "bound"
            )

        wait(recovered, 30)
        after = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
        assert (after["client_duid"], after["iaid"]) == (before["client_duid"], before["iaid"]), "PD worker restart lost client identity"
        wait(forwarding_ready, diagnostic=forwarding_diagnostic)
        wait_dns()
        check_traffic()
        print("PASS actual uplink PD worker SIGKILL recovery, retained DUID/IAID and restored LAN communication; seconds", round(time.time() - restarted_at, 2), flush=True)
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
        wait(forwarding_ready, diagnostic=forwarding_diagnostic)
        wait_dns()
        check_traffic()
        print("PASS actual uplink graceful PD restart, retained DUID/IAID and prefix, restored LAN communication; seconds", round(time.time() - graceful_at, 2), flush=True)
        if full_restart:
            restart_at = time.monotonic()
            daemon.send_signal(signal.SIGTERM)
            daemon.wait(timeout=30)
            assert daemon.returncode == 0, "router daemon failed to shut down before restart"
            daemon = start(router, [str(stage / "routerd"), "boot", "--file", str(stage / "boot.json")], "console")
            def control_available():
                return subprocess.run(["curl", "--max-time", "2", "-sS", "--unix-socket", str(stage / "run/control.sock"), "http://localhost/v1/config"], capture_output=True).returncode == 0
            wait(control_available, 30)
            wait(ready, 120)
            restored = query("control.sock", "/v1/services/service.wan-pd")["snapshot"]
            assert (restored["client_duid"], restored["iaid"]) == (before["client_duid"], before["iaid"]), "router restart changed PD identity"
            assert {p["prefix"] for p in restored["prefixes"]} == {p["prefix"] for p in before["prefixes"]}, "router restart changed the retained delegation"
            wait(forwarding_ready, diagnostic=forwarding_diagnostic)
            wait_dns()
            check_traffic()
            if config_path is not None:
                check_dns_sources()
                check_management()
                check_bgp()
                check_dslite_paths()
                wait(lambda: all(query("control.sock", "/v1/services/service.health-dslite" + str(i)).get("snapshot", {}).get("active", False) for i in range(3)))
                check_selected_upstream({272, 273, 274})
                check_operations()
                for namespace, process, events, mac in special_clients:
                    count = len(events.read_text().splitlines())
                    process.send_signal(signal.SIGUSR1)
                    wait(lambda: len(events.read_text().splitlines()) > count)
                check_special_clients()
                check_time()
            check_local_redirects()
            print("PASS actual HGW full daemon restart retained identity and restored both clients; seconds", round(time.monotonic() - restart_at, 2), flush=True)
            if tailnet:
                wait(lambda: tailnet_observation().get("backend_state") == "Running")
                check_tailnet()
                print("PASS combined WAN/PD/LAN/tailnet whole-daemon restart retained identity and restored bidirectional TCP/UDP and Internet exit", flush=True)
        if retention_check:
            print(run(["python3", "-I", str(stage / "retention-check.py"), "--stage", str(stage), "--router", router, "--client", client], timeout=180), end="", flush=True)
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
        pd_capture.send_signal(signal.SIGINT)
        pd_capture.wait(timeout=3)
        records = []
        for path in (*stage.glob("wan.pcap*"), stage / "pd.pcap"):
            packets = run(["tcpdump", "-nn", "-tt", "-vv", "-r", str(path), "ip6"])
            for line in packets.splitlines():
                if line and line[0].isdigit():
                    records.append(line)
                elif records:
                    records[-1] += " " + line.strip()
        records.sort(key=lambda line: float(line.split()[0]))
        if config_path is not None:
            normal_ra = [line for line in records if "router advertisement" in line and router_lan_ll + " >" in line and "router lifetime 0s" not in line]
            assert normal_ra and any("pref high" in line and re.search(r"mtu option [^:]+:\s*1500", line) for line in normal_ra), "configured RA preference/MTU not observed: " + str(normal_ra[-2:])
            lifetimes = [tuple(map(int, match)) for line in normal_ra for match in re.findall(r"valid time (\d+)s, pref\. time (\d+)s", line)]
            assert lifetimes and all(0 <= preferred <= 3600 and preferred <= valid <= 7200 for valid, preferred in lifetimes), lifetimes
            print("PASS actual LAN RA high preference, MTU1500 and configured prefix lifetime bounds", flush=True)
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
            for name in ("automatic-client-events", "automatic-resolv.conf", "router-routes.log", *(p.name for p in stage.glob("bgp-peer-*.log")), *(p.name for p in stage.glob("bgp-zebra-*.log"))):
                path = stage / name
                if path.exists():
                    print("Failed uplink", name, "\n".join(path.read_text(errors="replace").splitlines()[-25:]), flush=True)
            if automatic in namespaces:
                print("Failed uplink client IPv6", run(["ip", "-n", automatic, "-j", "-6", "address", "show", "dev", "client1"]), flush=True)
                print("Failed uplink client routes", run(["ip", "-n", automatic, "-j", "-6", "route", "show"]), flush=True)
            if (stage / "run/control.sock").exists():
                for endpoint in ("/v1/status", "/v1/events"):
                    try:
                        data = query("services/dns.dns.sock", endpoint)
                        if endpoint == "/v1/events":
                            data["events"] = data.get("events", [])[-20:]
                        print("Failed uplink DNS", endpoint, json.dumps(data), flush=True)
                    except Exception as diagnostic_error:
                        print("DNS inspection failed", endpoint, diagnostic_error, flush=True)
                for endpoint in ("/v1/network", "/v1/network/routes?table=120&limit=64"):
                    try:
                        print("Failed uplink network", endpoint, json.dumps(query("control.sock", endpoint)), flush=True)
                    except Exception as diagnostic_error:
                        print("Network inspection failed", endpoint, diagnostic_error, flush=True)
                status = query("control.sock", "/v1/services")
                print("Failed uplink projection", status.get("phase"), status.get("last_error"), flush=True)
                for instance in status.get("instances", []):
                    print("Failed uplink service", instance.get("name"), instance.get("phase"), instance.get("snapshot"), flush=True)
            for path in (stage / "console.log", *sorted((stage / "state/logs").glob("*.ndjson"))):
                if path.is_file():
                    for line in path.read_text(errors="replace").splitlines()[-60:]:
                        if '"ERROR"' in line or '"WARN"' in line or "dnsmasq" in line:
                            print(line, flush=True)
            for path in stage.glob("wan.pcap*"):
                packets = run(["tcpdump", "-nn", "-vv", "-r", str(path), "-c", "20", "icmp6 or udp port 546 or udp port 547 or udp port 67 or udp port 68"])
                print("Failed uplink wire", packets, flush=True)
                packets = run(["tcpdump", "-nn", "-tttt", "-r", str(path), "tcp port 443 or port 53"])
                print("Failed uplink DNS/HTTPS packets", "\n".join(packets.splitlines()[-30:]), flush=True)
            path = stage / "state/services/dns-core/dns-queries.db"
            if path.exists():
                with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as database:
                    rows = database.execute("SELECT ts, question_name, question_type, response_code, upstream, duration_us FROM dns_queries ORDER BY id DESC LIMIT 10").fetchall()
                print("Failed uplink DNS query history", rows, flush=True)
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
    parser.add_argument("--aftr", default="", help="AFTR address for the generated fixture; --config supplies its own AFTR intent")
    parser.add_argument("--config", type=Path, help="native ResourceProgram applied by the actual routerd process")
    parser.add_argument(
        "--dns-upstream",
        default="",
        help="Resolve this upstream hostname using WAN-learned bootstrap DNS",
    )
    parser.add_argument("--seconds", type=int, default=7800, help="observation duration; 0 waits through the live DHCPv6 renewal timer")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--state-dir", type=Path, help="Private persistent test identity and lease directory")
    parser.add_argument("--wan-mac", default="02:72:6f:75:74:02")
    parser.add_argument("--wan-outage", action="store_true", help="Blackhole this isolated router WAN and verify recovery")
    parser.add_argument("--full-restart", action="store_true", help="Restart the entire router with the same state and clients")
    parser.add_argument("--tailnet", action="store_true", help="Add independent Headscale/Tailscale with a fixed private underlay; exercise automatic LAN and real WAN exit together")
    parser.add_argument("--retention-check", action="store_true", help="Check real history archive failure/recovery after normal communication trials")
    parser.add_argument("--inside", type=Path)
    args = parser.parse_args()
    if args.seconds < 0:
        parser.error("--seconds cannot be negative")
    if args.config and args.aftr:
        parser.error("--aftr belongs to the generated fixture; the native profile owns AFTR intent")
    if args.config and args.dns_upstream:
        parser.error("--dns-upstream belongs to the generated fixture; the native profile owns DNS intent")
    if not args.config and not args.aftr:
        parser.error("--aftr is required by the generated fixture")
    if args.retention_check and not args.config:
        parser.error("--retention-check uses the native profile datasets")
    if args.inside:
        owner(args.inside, args.uplink, args.aftr, args.seconds, args.dns_upstream, args.wan_mac, args.wan_outage, args.full_restart, args.tailnet, args.config, args.retention_check)
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
        profile = None
        if args.config:
            profile = stage / "resource-program.yaml"
            run(root + ["install", "-m", "600", str(args.config.resolve()), str(profile)])
            run(root + ["install", "-m", "500", str(Path("dist/chrony-engine.tar").resolve()), str(stage / "chrony-engine.tar")])
        for source, name in ([(Path("scripts/tailscale-peer.py"), "peer-fixture.py"), (Path("scripts/syslog-peer.py"), "syslog-peer.py"), (Path("scripts/uplink-retention.py"), "retention-check.py")] if args.config else []) + [(Path(__file__), "owner.py"), (Path("dist/dnsmasq-engine.tar"), "dnsmasq-engine.tar")] + [
            (args.binary_dir / n, n)
            for n in ["routerd", "routerd-modules", "routerd-logging"]
        ]:
            run(
                root
                + ["install", "-m", "500", str(source.resolve()), str(stage / name)]
            )
        if args.tailnet:
            run(root + ["mkdir", "-m", "700", str(stage / "tailnet")])
            sources = [(Path("dist/peer-env/tailscale") / name, name) for name in ("headscale", "tailscale", "tailscaled")]
            sources += [(Path("dist/tailscale-engine.tar"), "tailscale-engine.tar"), (Path("scripts/tailscale-peer.py"), "peer-fixture.py")]
            for source, name in sources:
                run(root + ["install", "-m", "500", str(source.resolve()), str(stage / "tailnet" / name)])
        print("Current private live stage:", stage, flush=True)
        command = root + [
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
            ]
        if profile:
            command += ["--config", str(profile)]
        if args.wan_outage:
            command.append("--wan-outage")
        if args.full_restart:
            command.append("--full-restart")
        if args.tailnet:
            command.append("--tailnet")
        if args.retention_check:
            command.append("--retention-check")
        subprocess.run(
            command,
            check=True,
            timeout=args.seconds + 660 if args.seconds else None,
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
