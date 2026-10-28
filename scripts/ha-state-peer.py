#!/usr/bin/env python3
"""Check two actual routers preserving DHCP leases, TCP and UDP NAT state.

All links, sysctls, peers and packet captures live in private namespaces.
The TCP client never reconnects. tcp_loose=0 prevents picking up a midstream
flow without the replicated ESTABLISHED state; conntrack_tcp_liberal intent matches homert.
Completed diagnostics are deleted with the private stage.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import re
import sqlite3


def run(command):
    return subprocess.run(command, check=True, capture_output=True, text=True, timeout=10).stdout


def peer(stage, dhcp6_server, pd_dslite, stale_owner, preparation, delegated_dns, release_holds):
    def deadline(*_):
        raise TimeoutError("HA connection-state communication deadline")
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(360)
    # Keep iproute2's named-namespace files off the host filesystem too.
    Path("/run/netns").mkdir(parents=True, exist_ok=True)
    run(["mount", "-t", "tmpfs", "-o", "mode=700", "tmpfs", "/run/netns"])
    namespaces = ["ha-a", "ha-b", "ha-client", "ha-server"]
    if pd_dslite:
        namespaces.append("ha-backup")
    segments = ("lan", "wan", "sync", "backup") if pd_dslite else ("lan", "wan", "sync")
    children = []
    captures = []

    def inside(ns, command):
        return ["ip", "netns", "exec", ns] + command

    def start(ns, command, name):
        log = (stage / (name + ".log")).open("a")
        process = subprocess.Popen(inside(ns, command), stdout=log, stderr=log, text=True, bufsize=1)
        log.close()
        children.append(process)
        return process

    def wait(condition, timeout=15):
        until = time.monotonic() + timeout
        while time.monotonic() < until:
            if condition():
                return
            time.sleep(0.05)
        raise TimeoutError("HA peer condition did not become true")

    def api(node, socket, path, body=None):
        command = ["curl", "--max-time", "3", "--fail-with-body", "-sS", "--unix-socket", str(stage / node / "run" / socket), "http://localhost" + path]
        if body is not None:
            command += ["-X", "POST", "-H", "Content-Type: application/json", "--data-binary", json.dumps(body)]
        output = run(command)
        return json.loads(output) if output.strip() else None

    def modules(node):
        return {module["name"]: module for module in (api(node, "supervisor.sock", "/v1/status")["modules"] or [])}

    def daemon_ready(process, node):
        if process.poll() is not None:
            raise RuntimeError("restarted product exited before readiness")
        try:
            return modules(node).get("service.leases", {}).get("ready")
        except subprocess.CalledProcessError as error:
            if error.returncode == 7:
                return False
            raise

    def product_pid(process):
        path = Path("/proc") / str(process.pid) / "task" / str(process.pid) / "children"
        children = path.read_text().split()
        assert len(children) == 1, "unshare must own exactly one product PID 1"
        pid = int(children[0])
        fields = Path("/proc/" + str(pid) + "/status").read_text().splitlines()
        assert any(line.startswith("NSpid:") and line.split()[-1] == "1" for line in fields)
        return pid

    def host_pid(node, local_pid):
        candidates = []
        for value in run(["ip", "netns", "pids", "ha-" + node]).split():
            fields = Path("/proc/" + value + "/status").read_text().splitlines()
            if any(line.startswith("NSpid:") and int(line.split()[-1]) == local_pid for line in fields):
                candidates.append(int(value))
        assert len(candidates) == 1, "control process mapping is not unique"
        return candidates[0]

    def active(node):
        output = run(inside("ha-" + node, ["ip", "-j", "address", "show"])) if (stage / node / "run" / "supervisor.sock").exists() else "[]"
        addresses = {address.get("local") for link in json.loads(output) for address in link.get("addr_info", [])}
        return {"203.0.113.1", "198.18.0.1"} <= addresses

    try:
        for ns in namespaces:
            run(["ip", "netns", "add", ns])
            run(inside(ns, ["ip", "link", "set", "lo", "up"]))
        # The independent DHCP clients share this fixture namespace. Prevent
        # Linux's weak-host ARP behavior from answering on another client's
        # interface for an address owned by a sleeping client.
        run(inside("ha-client", ["sysctl", "-w", "net.ipv4.conf.all.arp_ignore=1"]))
        for bridge in segments:
            run(["ip", "link", "add", bridge, "type", "bridge"])
            run(["ip", "link", "set", bridge, "up"])
        for index, ns in enumerate(namespaces):
            attachments = segments if index < 2 else (("lan",) if index == 2 else ("wan",) if index == 3 else ("backup",))
            for segment in attachments:
                port = segment + str(index)
                run(["ip", "link", "add", port, "type", "veth", "peer", "name", "wire"])
                run(["ip", "link", "set", "wire", "netns", ns])
                run(["ip", "link", "set", port, "master", segment])
                run(["ip", "link", "set", port, "up"])
                run(inside(ns, ["ip", "link", "set", "wire", "name", segment + "0"]))
                run(inside(ns, ["ip", "link", "set", segment + "0", "up"]))
        key = stage / "key.pem"
        run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(key)])
        key.chmod(0o600)
        publisher = str(stage / "routerd-bundle")
        (stage / "trust.json").write_text(run([publisher, "trust", "--private-key-file", str(key)]))
        (stage / "trust.json").chmod(0o600)
        roles = ("logging", "network", "control", "management", "vrrp", "dhcp_server") + (("dhcp6", "dhcp6_server", "ra") if dhcp6_server else ()) + (("aftr", "health") if pd_dslite else ()) + (("dns",) if delegated_dns else ())
        layout = {"boot_abi": 1, "worker_abi": 5, "entrypoint": "routerd",
                  "roles": [{"role": role, "artifact": "routerd-logging" if role == "logging" else "routerd-modules"} for role in roles],
                  "engines": [{"role": "vrrp", "artifact": "ct-image", "sources": "ct-sources"}, {"role": "dhcp_server", "artifact": "dhcp-image", "sources": "dhcp-sources"}],
                  "artifacts": [{"name": name, "kind": "native", "command": name, "file": str(stage / name)} for name in ("routerd", "routerd-modules", "routerd-logging")] +
                               [{"name": "ct-image", "kind": "data", "file": str(stage / "conntrack-engine.tar")}, {"name": "ct-sources", "kind": "data", "file": str(stage / "conntrack-sources.tar")}, {"name": "dhcp-image", "kind": "data", "file": str(stage / "dnsmasq-engine.tar")}, {"name": "dhcp-sources", "kind": "data", "file": str(stage / "dnsmasq-sources.tar")}]}
        (stage / "layout.json").write_text(json.dumps(layout))
        version = json.loads(run([str(stage / "routerd"), "version"]))
        packed = json.loads(run([publisher, "pack", "--private-key-file", str(key), "--layout", str(stage / "layout.json"), "--release-id", version["release_id"], "--sequence", "1", "--target-arch", "amd64", "--output", str(stage / "bundle.tar")]))
        for index, node in enumerate(("a", "b")):
            ns = "ha-" + node
            run(["openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-keyout", str(stage / (node + ".key")), "-out", str(stage / (node + ".crt")), "-days", "1", "-subj", "/CN=" + node, "-addext", "subjectAltName=IP:169.254.42." + str(index + 2), "-addext", "extendedKeyUsage=serverAuth,clientAuth"])
            (stage / (node + ".key")).chmod(0o600)
            for key, value in (("net.netfilter.nf_conntrack_tcp_loose", "0"), ("net.netfilter.nf_conntrack_tcp_be_liberal", "0")):
                run(inside(ns, ["sysctl", "-w", key + "=" + value]))
            directory = stage / node
            directory.mkdir(mode=0o700)
            resources = [
                {"id": "lan", "interface": {"name": "lan0"}},
                {"id": "wan", "interface": {"name": "wan0"}},
                {"id": "sync", "interface": {"name": "sync0"}},
                {"id": "lan-base", "address": {"interface": "lan0", "cidr": "198.18.0." + str(index + 2) + "/24"}},
                {"id": "wan-base", "address": {"interface": "wan0", "cidr": "203.0.113." + str(index + 2) + "/24"}},
                {"id": "sync-base", "address": {"interface": "sync0", "cidr": "169.254.42." + str(index + 2) + "/16"}},
                {"id": "forward", "forwarding": {"ipv4": True, "ipv4_rp_filter": 0, "conntrack_tcp_liberal": True}},
                {"id": "ha", "service": {"name": "ha", "vrrp": {
                    "interface": "lan0", "family": "ipv4", "source_address": "198.18.0." + str(index + 2), "unicast_peers": ["198.18.0." + str(3 - index)],
                    "vip_interface": "lan-ha", "virtual_router_id": 42, "priority": 150 - index * 50, "preempt": False, "advert_interval_centiseconds": 100,
                    "addresses": ["198.18.0.1/24"], "accept_mode": True,
                    "conntrack_sync": {"interface": "sync0", "local_address": "169.254.42." + str(index + 2), "peer_address": "169.254.42." + str(3 - index)}
                }}},
                {"id": "leases", "service": {"name": "leases", "dhcp_server": {
                    "interface": "lan-ha", "server_address": "198.18.0.1", "authoritative": True, "sticky_hold_days": 3,
                    "primary": {"prefix": "198.18.0.0/24", "start": "198.18.0.100", "end": "198.18.0.103", "lease_seconds": 60, "router": "198.18.0.1"},
                    "ha": {"owner": "ha", "bootstrap": node == "a", "replication": {"listen_address": "169.254.42." + str(index + 2) + ":8443", "peer_address": "169.254.42." + str(3 - index) + ":8443", "certificate_file": str(stage / (node + ".crt")), "private_key_file": str(stage / (node + ".key")), "peer_certificate_file": str(stage / (("b" if node == "a" else "a") + ".crt"))}}
                }}},
                {"id": "wan-identity", "when": {"fact": "service.ha.elected"}, "link": {"name": "wan-ha", "kind": "macvlan", "parent": "wan0", "hardware_addr": "02:00:5e:00:01:43", "ipv6_addr_gen_mode": "none"}},
                {"id": "wan-interface", "interface": {"name": "wan-ha"}},
                {"id": "wan-address", "address": {"interface": "wan-ha", "cidr": "203.0.113.1/24"}},
                {"id": "firewall", "firewall": {"forward_chain": {"policy": "accept"}, "nat": [{"type": "snat", "match": {"family": 4, "source": "198.18.0.0/24", "out_interface": "wan-ha"}, "address": "203.0.113.1"}]}},
            ]
            # Select the shared WAN identity rather than the physical-port
            # connected route installed by the product's physical-port intent.
            resources.append({"id": "wan-route", "route": {"destination": "203.0.113.50/32", "interface": "wan-ha"}})
            if dhcp6_server:
                resources[6]["forwarding"]["ipv6"] = True
                resources[7]["service"]["vrrp"]["graceful_activation"] = {"ready_when": {"fact": "service.pd.active"}, "timeout": "1s"}
                pd_peer = {"listen_address": "169.254.42." + str(index + 2) + ":8444", "peer_address": "169.254.42." + str(3 - index) + ":8444", "certificate_file": str(stage / (node + ".crt")), "private_key_file": str(stage / (node + ".key")), "peer_certificate_file": str(stage / (("b" if node == "a" else "a") + ".crt"))}
                resources += [
                    {"id": "wan-linklocal", "when": {"fact": "service.ha.elected"}, "address": {"interface": "wan-ha", "cidr": "fe80::1/64"}},
                    {"id": "wan6-route", "when": {"fact": "service.ha.elected"}, "route": {"destination": "2001:db8:ffff::/64", "interface": "wan-ha", "gateway": "fe80::2"}},
                    {"id": "pd", "service": {"name": "pd", "dhcp6": {"interface": "wan-ha", "request_prefix": True, "prefix_length": 60, "client_duid": "0003000102005e000143", "iaid": 7,
                        "ha": {"owner": "ha", "replication": pd_peer}, "delegations": [{"name": "lan", "interface": "lan0", "subnet_id": 1, "advertise": True, "other_config": True, "router_lifetime_seconds": 10}]}}}
                ]
                resources.append({"id": "lan-info", "when": {"fact": "service.ha.active"}, "service": {"name": "lan-info", "dhcp6_server": {
                    "interface": "lan0", "server_duid": "000400112233445546778899aabbccddeeff", "dns_from": ["pd"], "search_from": ["pd"], "domain_search": ["local.test"], "refresh_seconds": 600
                }}})
                if delegated_dns:
                    pd = next(r["service"]["dhcp6"] for r in resources if r["id"] == "pd")
                    pd["delegations"][0]["rdnss_router"] = True
                    info = resources[-1]["service"]["dhcp6_server"]
                    info.pop("dns_from")
                    info["dns_delegation"] = {"service": "pd", "delegation": "lan"}
                    resources.append({"id": "dns", "when": {"fact": "service.ha.active"}, "service": {"name": "dns", "dns": {
                        "listen_interface": "lan0", "spec": {"listen": [{"name": "lan", "addresses": ["::"], "port": 53}],
                        "sources": [{"name": "upstream", "kind": "forward", "match": ["."], "upstreams": ["udp://[2001:db8:ffff::20]:53"]}]}
                    }}})
            if pd_dslite:
                # Match homert's default one-second advertisement interval.
                # The existing direct/PD profiles retain their 20cs checks.
                resources[7]["service"]["vrrp"]["advert_interval_centiseconds"] = 100
                resources[6]["forwarding"]["ipv4_ecmp_hash"] = "source"
                # Publish only when a complete primary path or the independent
                # backup path is usable. The backup has a different identity.
                resources[7]["service"]["vrrp"]["graceful_activation"]["ready_when"] = {"any": [
                    {"all": [{"fact": "service.pd.active"}, {"fact": "service.health-primary.active"}]},
                    {"fact": "service.health-backup.active"}]}
                pd = next(r["service"]["dhcp6"] for r in resources if r["id"] == "pd")
                pd["delegations"].append({"name": "b4", "interface": "wan-ha", "subnet_id": 1, "host_id": 33, "prefix_length": 128})
                resources += [
                    {"id": "backup", "interface": {"name": "backup0"}},
                    {"id": "backup-identity", "when": {"fact": "service.ha.elected"}, "link": {"name": "backup-ha", "kind": "macvlan", "parent": "backup0", "hardware_addr": "02:00:5e:00:01:44"}},
                    {"id": "backup-interface", "interface": {"name": "backup-ha"}},
                    {"id": "backup-address", "address": {"interface": "backup-ha", "cidr": "192.0.2.1/24"}},
                    {"id": "internet", "when": {"fact": "service.pd.active"}, "ready_when": {"fact": "service.internet.active"}, "service": {"name": "internet", "ds_lite": {
                        "interface": "wan-ha", "tunnel": "dslite0", "source_from": "pd", "source_delegation": "b4", "candidates": ["2001:db8:ffff::20"],
                        "b4_peer": "192.0.0.1/32", "route_gateway": "192.0.0.1", "table": 110, "mtu": 1460}}},
                    {"id": "backup-default", "route": {"destination": "0.0.0.0/0", "gateway": "192.0.2.50", "interface": "backup-ha", "table": 120}},
                    {"id": "probe-primary", "policy_rule": {"family": 4, "priority": 110, "table": 110, "mark": 41}},
                    {"id": "probe-backup", "policy_rule": {"family": 4, "priority": 120, "table": 120, "mark": 42}},
                    {"id": "health-primary", "depends_on": ["internet", "probe-primary"], "when": {"fact": "service.internet.active"}, "service": {"name": "health-primary", "health": {
                        "interface": "dslite0", "source_address": "192.0.0.2", "target": "203.0.113.50", "protocol": "tcp", "port": 18082, "mark": 41,
                        "interval_ms": 200, "timeout_ms": 200, "healthy_threshold": 2, "unhealthy_threshold": 2}}},
                    {"id": "health-backup", "depends_on": ["probe-backup"], "service": {"name": "health-backup", "health": {
                        "interface": "backup-ha", "source_address": "192.0.2.1", "target": "203.0.113.50", "protocol": "tcp", "port": 18082, "mark": 42,
                        "interval_ms": 200, "timeout_ms": 200, "healthy_threshold": 2, "unhealthy_threshold": 2}}},
                    {"id": "egress", "egress_route": {"destination": "0.0.0.0/0", "table": 100, "metric": 100, "candidates": [
                        {"preference": 120, "when": {"fact": "service.health-primary.active"}, "next_hop": {"interface": "dslite0", "gateway": "192.0.0.1", "weight": 1}},
                        {"preference": 60, "when": {"fact": "service.health-backup.active"}, "next_hop": {"interface": "backup-ha", "gateway": "192.0.2.50", "weight": 1}}]}},
                    {"id": "lan-egress", "policy_rule": {"family": 4, "priority": 1000, "table": 100, "source": "198.18.0.0/24"}},
                ]
                resources.append({"id": "backup-parent-address", "address": {"interface": "backup0", "cidr": "192.0.2." + str(2 + index) + "/24"}})
                firewall = next(r["firewall"] for r in resources if r["id"] == "firewall")
                firewall["nat"] = [{"type": "snat", "match": {"family": 4, "source": "0.0.0.0/0", "out_interface": name}, "address": address, "connection_mark": mark}
                                   for name, address, mark in (("dslite0", "203.0.113.1", 41), ("backup-ha", "192.0.2.1", 42))]
                # The AFTR underlay is IPv6-only. Do not inherit the direct
                # profile's IPv4 route around the tunnel during handoff.
                resources = [r for r in resources if r["id"] not in ("wan-base", "wan-route")]
                next(r["address"] for r in resources if r["id"] == "wan-address")["cidr"] = "203.0.113.1/32"
            (directory / "config.json").write_text(json.dumps({"schema_version": 2, "resources": resources}))
            workers = {role: str(stage / ("routerd-logging" if role == "logging" else "routerd-modules")) for role in roles}
            boot = {"platform": "host", "config": str(directory / "config.json"), "runtime_dir": str(directory / "run"), "state_dir": str(directory / "state"), "workers": workers, "engines": {"vrrp": str(stage / "conntrack-engine.tar")}, "ssh": {"user": "admin"}}
            selector = ["--root", str(directory / "releases"), "--trust-file", str(stage / "trust.json"), "--target-arch", "amd64"]
            run([str(stage / "routerd"), "release", "stage", *selector, "--file", str(stage / "bundle.tar")])
            run([str(stage / "routerd"), "release", "initialize", *selector, "--digest", packed["digest"]])
            boot.update(platform="appliance", workers={}, engines={}, release={"root": str(directory / "releases"), "trust_file": str(stage / "trust.json")})
            (directory / "boot.json").write_text(json.dumps(boot))
        run(inside("ha-client", ["ip", "address", "add", "198.18.0.50/24", "dev", "lan0"]))
        run(inside("ha-client", ["ip", "route", "add", "default", "via", "198.18.0.1"]))
        run(inside("ha-server", ["ip", "address", "add", "203.0.113.50/32" if pd_dslite else "203.0.113.50/24", "dev", "lo" if pd_dslite else "wan0"]))
        server = r'''
import socket, threading
def udp():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('203.0.113.50',18081))
 while True:
  data,peer=s.recvfrom(2048);s.sendto(data+b' '+str(peer).encode(),peer)
threading.Thread(target=udp,daemon=True).start()
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('203.0.113.50',18080));s.listen()
while True:
 c,peer=s.accept()
 print('accepted',peer,flush=True)
 f=c.makefile('rwb',buffering=0)
 for line in f: f.write(line.rstrip()+b' '+str(peer).encode()+b'\n')
'''
        start("ha-server", ["python3", "-u", "-c", server], "server")
        if pd_dslite:
            run(inside("ha-backup", ["ip", "address", "add", "192.0.2.50/24", "dev", "backup0"]))
            run(inside("ha-backup", ["ip", "address", "add", "203.0.113.50/32", "dev", "lo"]))
            egress_server = r'''
import socket,threading,sys
value=sys.argv[1].encode()
def udp():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('203.0.113.50',18083))
 while True:
  data,peer=s.recvfrom(2048);s.sendto(value,peer)
threading.Thread(target=udp,daemon=True).start()
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('203.0.113.50',18082));s.listen()
while True:
 c,_=s.accept();c.sendall(value);c.close()
'''
            start("ha-server", ["python3", "-u", "-c", egress_server, "DSLite"], "egress-primary")
            start("ha-backup", ["python3", "-u", "-c", egress_server, "backup"], "egress-backup")
        if dhcp6_server:
            run(inside("ha-server", ["ip", "-6", "address", "add", "fe80::2/64", "dev", "wan0"]))
            run(inside("ha-server", ["ip", "-6", "address", "add", "2001:db8:ffff::20/64", "dev", "wan0"]))
            run(inside("ha-server", ["ip", "-6", "route", "add", "2001:db8:80::/56", "via", "fe80::1", "dev", "wan0"]))
            run(inside("ha-client", ["sysctl", "-w", "net.ipv6.conf.lan0.accept_ra=2"]))
            mac = json.loads(run(inside("ha-server", ["ip", "-j", "link", "show", "dev", "wan0"])))[0]["address"]
            (stage / "dhcp6.conf").write_text("default-lease-time 60;\npreferred-lifetime 40;\noption dhcp-renewal-time 20;\noption dhcp-rebinding-time 40;\noption dhcp6.name-servers 2001:db8:ffff::20;\noption dhcp6.domain-search \"wan.test\";\nsubnet6 2001:db8:ffff::/64 { prefix6 2001:db8:80:: 2001:db8:80:f0:: /60; }\nserver-duid LL ethernet " + mac + ";\n")
            (stage / "dhcp6.leases").touch()
            wait(lambda: all(not a.get("tentative") and not a.get("dadfailed") for link in json.loads(run(inside("ha-server", ["ip", "-j", "-6", "address", "show", "dev", "wan0"]))) for a in link["addr_info"]))
            start("ha-server", ["dnsmasq", "--no-daemon", "--conf-file=/dev/null", "--interface=wan0", "--bind-interfaces", "--no-resolv", "--no-hosts", "--pid-file=", "--address=/wan.test/2001:db8:ffff::20"], "dns6")
            def pd_start():
                return start("ha-server", [str(dhcp6_server), "-6", "-f", "-d", "-cf", str(stage / "dhcp6.conf"), "-lf", str(stage / "dhcp6.leases"), "-pf", str(stage / "dhcp6.pid"), "wan0"], "dhcp6")
            pd_server = pd_start()
            start("ha-server", ["python3", "-u", "-m", "http.server", "18080", "--bind", "2001:db8:ffff::20", "--directory", str(stage)], "http6")
            start("ha-server", ["python3", "-u", "-c", 'import socket\ns=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.bind(("2001:db8:ffff::20",18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)'], "udp6")

        def pd_saved(node):
            path = stage / node / "state/leases/pd.dhcp6.json"
            return json.loads(path.read_text()) if path.exists() else {}

        def pd_observed(node):
            return api(node, "control.sock", "/v1/services/service.pd").get("snapshot", {})

        def pd_traffic(node):
            def usable():
                observation = pd_observed(node)
                routes = json.loads(run(inside("ha-client", ["ip", "-j", "-6", "route", "show", "default"])))
                addresses = json.loads(run(inside("ha-client", ["ip", "-j", "-6", "addr", "show", "dev", "lan0"])))
                return observation.get("prefixes") and observation["state"] in ("bound", "renewing", "rebinding") and any(route.get("dev") == "lan0" for route in routes) and any(a["scope"] == "global" and not a.get("tentative") for link in addresses for a in link["addr_info"]) and modules(node).get("service.pd", {}).get("ready")
            wait(usable)
            assert run(inside("ha-client", ["curl", "--noproxy", "*", "--max-time", "3", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "http://[2001:db8:ffff::20]:18080/"])) == "200"
            run(inside("ha-client", ["python3", "-c", 'import socket;s=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.settimeout(3);s.connect(("2001:db8:ffff::20",18081));s.send(b"ha-pd");assert s.recv(100)==b"ha-pd"']))

        info_hook = stage / "info-hook.sh"
        info_hook.write_text('#!/bin/sh\ncase "$1" in bound|renew) printf "dns=%s\\nsearch=%s\\n" "$dns" "$search";; esac\n')
        info_hook.chmod(0o500)

        def information_request():
            return subprocess.run(inside("ha-client", ["busybox", "udhcpc6", "-l", "-f", "-n", "-q", "-i", "lan0", "-s", str(info_hook), "-t", "1", "-T", "1"]), capture_output=True, text=True, timeout=3)

        def direct_information(target, expect_reply):
            # Probe one actual node. A multicast BusyBox request can legitimately
            # receive the new master's Reply while the old control is paused.
            command = r'''
import socket,sys
s=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.settimeout(0.75);s.bind(('::',546))
s.sendto(bytes.fromhex('0b0102030006000400170018'),(sys.argv[1],547,0,socket.if_nametoindex('lan0')))
try:
 data,peer=s.recvfrom(4096)
 assert data[:4]==bytes.fromhex('07010203') and peer[0]==sys.argv[1],(data.hex(),peer)
 assert sys.argv[2]=='reply','expired node still returned a unicast Reply'
except socket.timeout:
 assert sys.argv[2]=='silent','active node did not reply'
'''
            result = subprocess.run(inside("ha-client", ["python3", "-c", command, target, "reply" if expect_reply else "silent"]), capture_output=True, text=True, timeout=3)
            if result.returncode:
                raise RuntimeError("direct Information-request: " + result.stderr)

        def information(owner):
            import ipaddress
            def expected_dns():
                if not delegated_dns:
                    return "2001:db8:ffff::20"
                observed = api(owner, "control.sock", "/v1/services/service.pd")["snapshot"]
                prefixes = [ipaddress.IPv6Network(p["prefix"]) for p in observed.get("prefixes", [])]
                addresses = json.loads(run(inside("ha-" + owner, ["ip", "-j", "-6", "addr", "show", "dev", "lan0"])))
                return next((a["local"] for link in addresses for a in link["addr_info"] if any(ipaddress.IPv6Address(a["local"]) in p for p in prefixes)), "")
            def received():
                reply = information_request()
                if reply.returncode:
                    return False
                options = dict(line.split("=", 1) for line in reply.stdout.splitlines() if line.startswith(("dns=", "search=")))
                return [str(ipaddress.IPv6Address(ip)) for ip in options.get("dns", "").split()] == [expected_dns()] and set(options.get("search", "").split()) == {"wan.test", "local.test"}
            wait(received, timeout=6)
            assert modules(owner).get("service.lan-info", {}).get("ready"), "master information server absent"
            standby = "b" if owner == "a" else "a"
            assert "service.lan-info" not in modules(standby), "standby information server still active"
            for transport in ("+notcp", "+tcp"):
                query = subprocess.run(inside("ha-client", ["dig", "-6", "@" + expected_dns(), "host.wan.test", "AAAA", "+short", "+time=2", "+tries=1", transport]), capture_output=True, text=True)
                if query.returncode or "2001:db8:ffff::20" not in query.stdout:
                    for command in (["ip", "-6", "address", "show", "dev", "lan0"], ["ip", "-6", "route", "show"], ["ip", "-6", "neigh", "show", "dev", "lan0"]):
                        print("client", *command, run(inside("ha-client", command)), flush=True)
                    print("DNS query", query.stdout, query.stderr, flush=True)
                    print("DNS packets", run(["tcpdump", "-nn", "-tt", "-r", str(stage / "lan.pcap"), "udp port 53 or tcp port 53"]), flush=True)
                    nd = run(["tcpdump", "-nn", "-tt", "-vv", "-r", str(stage / "lan.pcap"), "icmp6 and (ip6[40] == 135 or ip6[40] == 136)"])
                    print("Neighbor discovery", "\n".join(nd.splitlines()[-50:]), flush=True)
                    raise RuntimeError("delegated DNS query failed")
            return expected_dns()

        for segment in segments:
            log = (stage / (segment + "-capture.log")).open("w")
            process = subprocess.Popen(["tcpdump", "--immediate-mode", "-U", "-n", "-i", segment, "-w", str(stage / (segment + ".pcap"))], stdout=log, stderr=log)
            children.append(process)
            captures.append(process)
            log.close()
        if dhcp6_server:
            pd_server.send_signal(signal.SIGSTOP)
        if pd_dslite:
            run(inside("ha-backup", ["ip", "link", "set", "backup0", "down"]))
        start("ha-a", ["unshare", "--pid", "--fork", "--mount-proc", "--kill-child=SIGTERM", str(stage / "routerd"), "boot", "--file", str(stage / "a/boot.json")], "a")
        wait(lambda: (stage / "a/run/supervisor.sock").exists())
        wait(lambda: modules("a").get("service.ha", {}).get("ready") is True)
        if dhcp6_server:
            if pd_dslite:
                def admission_timed_out():
                    services = api("a", "control.sock", "/v1/services")["instances"]
                    ha = next(i for i in services if i["name"] == "service.ha")["snapshot"]
                    if ha.get("active"):
                        raise AssertionError("VIP published without an available WAN")
                    return ha.get("activation_state") == "ReadinessTimeout"
                wait(admission_timed_out)
            else:
                wait(lambda: api("a", "control.sock", "/v1/services/service.ha")["snapshot"].get("activation_state") == "ReadinessTimeout")
            wait(lambda: pd_observed("a").get("state") == "soliciting")
            addresses = {a["local"] for link in json.loads(run(inside("ha-a", ["ip", "-j", "address", "show"]))) for a in link.get("addr_info", [])}
            assert "203.0.113.1" in addresses and "198.18.0.1" not in addresses, "PD preparation did not retain WAN election separately from VIP publication"
            pd_server.send_signal(signal.SIGCONT)
            if pd_dslite:
                run(inside("ha-backup", ["ip", "link", "set", "backup0", "up"]))
        if pd_dslite:
            wait(lambda: pd_observed("a").get("prefixes"))
            def b4_addresses():
                addresses = json.loads(run(inside("ha-a", ["ip", "-j", "-6", "address", "show", "dev", "wan-ha"])))
                return [a["local"] for link in addresses for a in link["addr_info"] if a["scope"] == "global" and a["prefixlen"] == 128]
            wait(lambda: len(b4_addresses()) == 1)
            b4 = b4_addresses()
            run(inside("ha-server", ["ip", "-6", "tunnel", "add", "aftr0", "mode", "ipip6", "local", "2001:db8:ffff::20", "remote", b4[0], "dev", "wan0", "encaplimit", "none"]))
            run(inside("ha-server", ["ip", "link", "set", "aftr0", "up"]))
            run(inside("ha-server", ["ip", "address", "add", "192.0.0.1/32", "peer", "192.0.0.2/32", "dev", "aftr0"]))
            run(inside("ha-server", ["ip", "route", "add", "203.0.113.1/32", "dev", "aftr0"]))

            def egress_traffic(expected, node="a"):
                code = r'''
import socket,sys
for protocol,port in ((socket.SOCK_STREAM,18082),(socket.SOCK_DGRAM,18083)):
 s=socket.socket(socket.AF_INET,protocol);s.settimeout(2);s.connect(('203.0.113.50',port))
 if protocol==socket.SOCK_DGRAM:s.send(b'egress')
 assert s.recv(100)==sys.argv[1].encode()
 s.close()
'''
                run(inside("ha-client", ["python3", "-c", code, expected]))
                mark = 41 if expected == "DSLite" else 42
                def marked():
                    entries = run(inside("ha-" + node, ["conntrack", "-L", "-p", "tcp", "--orig-src", "198.18.0.50", "--dport", "18082"]))
                    return any("mark=" + str(mark) + " " in line for line in entries.splitlines())
                wait(marked)

            def egress_path(node, interface):
                routes = json.loads(run(inside("ha-" + node, ["ip", "-j", "route", "show", "table", "100"])))
                return len(routes) == 1 and routes[0].get("dev") == interface

            wait(lambda: active("a"))
            wait(lambda: egress_path("a", "dslite0"))
            egress_traffic("DSLite")
            run(inside("ha-server", ["ip", "-6", "route", "add", "blackhole", b4[0] + "/128"]))
            wait(lambda: egress_path("a", "backup-ha"))
            egress_traffic("backup")
            run(inside("ha-server", ["ip", "-6", "route", "del", "blackhole", b4[0] + "/128"]))
            wait(lambda: egress_path("a", "dslite0"))
            egress_traffic("DSLite")
            print("PASS PD-derived B4/independent Linux AFTR and independent backup WAN: real TCP/UDP failure/recovery", flush=True)
        wait(lambda: active("a"))
        if dhcp6_server:
            assert pd_observed("a").get("prefixes"), "PD did not recover after WAN startup"
            if pd_dslite:
                print("PASS both WANs unavailable withheld VIP; PD and forwarding recovered after peers resumed", flush=True)
            else:
                print("PASS elected WAN acquired PD before graceful VIP publication; timeout withheld VIP until ISC resumed", flush=True)
        hook = stage / "dhcp-hook.sh"
        hook.write_text('#!/bin/sh\ncase "$1" in\n bound|renew) ip address replace "$ip/24" dev "$interface"; printf "%s %s\\n" "$1" "$ip" >> "' + str(stage) + '/$interface.progress";;\nesac\n')
        hook.chmod(0o500)

        def lease_client(number, requested=None, await_lease=True):
            name = "lease" + str(number)
            port = "dhcp" + str(number)
            run(["ip", "link", "add", port, "type", "veth", "peer", "name", name])
            run(["ip", "link", "set", port, "master", "lan"])
            run(["ip", "link", "set", port, "up"])
            run(["ip", "link", "set", name, "netns", "ha-client"])
            # These are IPv4-only extra clients in the same namespace. Their
            # RA routes must not compete with the separate IPv6 test client.
            run(inside("ha-client", ["sysctl", "-w", "net.ipv6.conf." + name + ".accept_ra=0"]))
            run(inside("ha-client", ["ip", "link", "set", name, "up"]))
            command = ["busybox", "udhcpc", "-f", "-i", name, "-s", str(hook), "-p", str(stage / (name + ".pid")), "-t", "3", "-T", "1", "-A", "1"]
            if requested:
                command += ["-r", requested]
            process = start("ha-client", command, name)
            if await_lease:
                wait(lambda: (stage / (name + ".progress")).exists())
            return process

        def lease_events(number):
            return (stage / ("lease" + str(number) + ".progress")).read_text().splitlines()

        def saved(node):
            path = stage / node / "state/services/leases/dnsmasq.leases"
            if not path.exists():
                return {"leases": []}
            return {"leases": sorted([line.split() for line in path.read_text().splitlines() if len(line.split()) == 5 and int(line.split()[0]) > time.time()])}

        wait(lambda: modules("a").get("service.leases", {}).get("ready") is True and api("a", "control.sock", "/v1/services/service.leases")["snapshot"]["active"] is True)
        first_client = lease_client(0)
        first_address = lease_events(0)[-1].split()[1]
        assert first_address in ("198.18.0.100", "198.18.0.101", "198.18.0.102", "198.18.0.103")
        print("PASS elected DHCP server started and allocated a lease while its partner was absent", flush=True)
        b_command = ["unshare"] + (["--time", "--boottime", "7200"] if dhcp6_server else []) + ["--pid", "--fork", "--mount-proc", "--kill-child=SIGTERM", str(stage / "routerd"), "boot", "--file", str(stage / "b/boot.json")]
        b_daemon = start("ha-b", b_command, "b")
        wait(lambda: (stage / "b/run/supervisor.sock").exists())
        wait(lambda: modules("b").get("service.ha", {}).get("ready") is True)
        wait(lambda: modules("a").get("service.leases", {}).get("ready") is True and modules("b").get("service.leases", {}).get("ready") is True)
        if dhcp6_server:
            wait(lambda: modules("a").get("service.pd", {}).get("ready") and modules("b").get("service.pd", {}).get("ready"))
            pd_traffic("a")
            dns_address = information("a")
            if delegated_dns:
                advertisements = run(["tcpdump", "-nn", "-v", "-r", str(stage / "lan.pcap"), "icmp6 and ip6[40] == 134"])
                assert "rdnss" in advertisements.lower() and dns_address in advertisements, "RA did not advertise the delegated router DNS"
                run(inside("ha-server", ["ping", "-6", "-c", "1", "-W", "2", dns_address]))
                for transport in ("+notcp", "+tcp"):
                    wan_query = subprocess.run(inside("ha-server", ["dig", "-6", "@" + dns_address, "host.wan.test", "AAAA", "+time=1", "+tries=1", transport]), capture_output=True, text=True)
                    assert wan_query.returncode != 0, "WAN client reached the LAN-bound DNS listener"
                print("PASS routed WAN peer reached the router address but not its LAN-bound UDP/TCP DNS listener", flush=True)
                print("PASS delegated LAN DNS in RA and DHCPv6, queried by an independent client over UDP/TCP", flush=True)
                original = api("a", "control.sock", "/v1/services/service.pd")["snapshot"]["prefixes"][0]["valid_until"]["nanos"]
                pd_server.terminate()
                pd_server.wait(timeout=5)
                remaining = max(0, (original - time.clock_gettime_ns(time.CLOCK_BOOTTIME)) / 1_000_000_000)
                def withdrawn():
                    addresses = json.loads(run(inside("ha-a", ["ip", "-j", "-6", "addr", "show", "dev", "lan0"])))
                    return all(a["local"] != dns_address for link in addresses for a in link["addr_info"])
                wait(withdrawn, timeout=remaining + 8)
                reply = information_request()
                assert reply.returncode == 0 and not any(line.startswith("dns=") and line[4:].strip() for line in reply.stdout.splitlines()), "expired delegated DNS was still supplied"
                old_query = subprocess.run(inside("ha-client", ["dig", "-6", "@" + dns_address, "host.wan.test", "AAAA", "+time=1", "+tries=1"]), capture_output=True, text=True)
                assert old_query.returncode != 0, "expired delegated DNS still answered"
                print("PASS expired delegation removed DNS from the interface and DHCPv6 replies", flush=True)
                config_path = stage / "dhcp6.conf"
                config_path.write_text(config_path.read_text().replace("prefix6 2001:db8:80:: 2001:db8:80:f0:: /60", "prefix6 2001:db8:80:40:: 2001:db8:80:40:: /60"))
                pd_server = pd_start()
                wait(lambda: any(p["prefix"].startswith("2001:db8:80:40:") for p in api("a", "control.sock", "/v1/services/service.pd")["snapshot"].get("prefixes", [])), timeout=60)
                new_dns = information("a")
                assert new_dns != dns_address, "delegated DNS did not change with the new prefix"
                # A changed RA is sent no sooner than three seconds after
                # the preceding advertisement, so observe that packet.
                wait(lambda: new_dns in run(["tcpdump", "-nn", "-v", "-r", str(stage / "lan.pcap"), "icmp6 and ip6[40] == 134"]), timeout=6)
                print("PASS changed delegation updated RA, DHCPv6 and UDP/TCP DNS", flush=True)
                return
            wait(lambda: pd_saved("a").get("revision") == pd_saved("b").get("revision") and pd_saved("a").get("snapshot", {}).get("prefixes"))
            original = pd_saved("b")
            before = modules("b")["service.pd"]["generation"]
            api("b", "control.sock", "/v1/services/service.pd/restart", {})
            wait(lambda: modules("b").get("service.pd", {}).get("generation", 0) > before and modules("b").get("service.pd", {}).get("ready"))
            assert pd_saved("b") == original, "standby restart rebased PD or lost the source revision"
            print("PASS ISC PD, independent SLAAC TCP/UDP, authenticated standby checkpoint with BOOTTIME +7200s and restart with original mapping", flush=True)
        wait(lambda: saved("a")["leases"] == saved("b")["leases"] and bool(saved("a")["leases"]))
        print("PASS independent BusyBox DORA; authenticated standby has the original dnsmasq leases", flush=True)
        client_code = r'''
import socket,time,json
s=socket.create_connection(('203.0.113.50',18080),timeout=5);f=s.makefile('rwb',buffering=0)
s.settimeout(15)
u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);u.bind(('198.18.0.50',30001));u.connect(('203.0.113.50',18081))
sequence=0;udp_sequence=0;udp_peer='';lost=0;longest=0
while True:
 sequence+=1;data=str(sequence).encode();prefix=data+b' '
 began=time.monotonic();f.write(data+b'\n');tcp=f.readline().strip();longest=max(longest,time.monotonic()-began)
 if not tcp.startswith(prefix):raise RuntimeError('TCP stream closed or changed')
 u.send(data);end=time.monotonic()+0.2
 while True:
  remaining=end-time.monotonic()
  if remaining<=0:lost+=1;break
  u.settimeout(remaining)
  try:reply=u.recv(2048)
  except socket.timeout:lost+=1;break
  if reply.startswith(prefix):udp_peer=reply[len(prefix):].decode();udp_sequence=sequence;break
 print(json.dumps({'sequence':sequence,'udp_sequence':udp_sequence,'tcp_peer':tcp[len(prefix):].decode(),'udp_peer':udp_peer,'udp_lost':lost,'longest_tcp_reply_seconds':longest}),flush=True)
 time.sleep(0.05)
'''
        with (stage / "client.progress").open("w") as output, (stage / "client.log").open("w") as diagnostic:
            client = subprocess.Popen(inside("ha-client",["python3","-u","-c",client_code]),stdout=output,stderr=diagnostic)
        children.append(client)

        def latest():
            if client.poll() is not None:
                raise RuntimeError("continuous same-socket client exited")
            complete = (stage / "client.progress").read_text().rpartition("\n")[0].splitlines()
            return json.loads(complete[-1]) if complete else {"sequence": 0, "udp_sequence": 0}

        def progress(previous):
            wait(lambda: latest()["sequence"] > previous + 3 and latest()["udp_sequence"] > previous)
            observed = latest()
            assert "203.0.113.1" in observed["tcp_peer"] and "203.0.113.1" in observed["udp_peer"]
            return (observed["tcp_peer"], observed["udp_peer"])

        for node in ("a","b"):
            assert run(inside("ha-"+node,["sysctl","-n","net.netfilter.nf_conntrack_tcp_be_liberal"])).strip() == "1", "product did not apply conntrack handoff intent"
        mapping = progress(0)
        if pd_dslite:
            entries = run(inside("ha-a", ["conntrack", "-L", "-p", "tcp", "--orig-src", "198.18.0.50", "--dport", "18080"]))
            assert "mark=41 " in entries, "established primary TCP has no primary connection mark"
        run(inside("ha-client", ["ip", "-4", "neigh", "flush", "to", "198.18.0.1", "dev", "lan0"]))
        assert progress(latest()["sequence"]) == mapping
        neighbor = run(inside("ha-client", ["ip", "-4", "neigh", "show", "198.18.0.1", "dev", "lan0"]))
        assert "00:00:5e:00:01:2a" in neighbor.lower(), "client learned a physical MAC for the shared VIP: " + neighbor
        for link in ("lan0", "lan-ha"):
            assert run(inside("ha-a", ["sysctl", "-n", "net.ipv4.conf." + link + ".arp_ignore"])).strip() == "1"
        def check_backup_arp(owner):
            for link in ("backup0", "backup-ha"):
                assert run(inside("ha-" + owner, ["sysctl", "-n", "net.ipv4.conf." + link + ".arp_ignore"])).strip() == "1"
                assert run(inside("ha-" + owner, ["sysctl", "-n", "net.ipv4.conf." + link + ".arp_announce"])).strip() == "2"
            run(inside("ha-backup", ["ip", "-4", "neigh", "flush", "to", "192.0.2.1", "dev", "backup0"]))
            run(inside("ha-backup", ["python3", "-c", "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.sendto(b'arp',('192.0.2.1',9))"]))
            def neighbor():
                return run(inside("ha-backup", ["ip", "-4", "neigh", "show", "192.0.2.1", "dev", "backup0"]))
            wait(lambda: "lladdr" in neighbor(), timeout=5)
            packets = run(["tcpdump", "-nn", "-e", "-r", str(stage / "backup.pcap"), "arp and host 192.0.2.1"])
            replies = [line for line in packets.splitlines() if "Reply 192.0.2.1 is-at" in line]
            assert "02:00:5e:00:01:44" in neighbor().lower(), "backup WAN learned a physical MAC: " + neighbor() + "\n" + packets
            assert replies and all("is-at 02:00:5e:00:01:44" in line for line in replies), "backup WAN saw a physical-MAC ARP reply: " + packets
            requests = [line for line in packets.splitlines() if "tell 192.0.2.1" in line]
            assert all("02:00:5e:00:01:44 >" in line for line in requests), "backup WAN saw a physical-MAC ARP request for the shared address: " + packets
        if pd_dslite:
            check_backup_arp("a")
        if preparation:
            api("a", "control.sock", "/v1/services/service.ha/restart", {})
            def preparing():
                try:
                    return api("b", "control.sock", "/v1/services/service.pd")["snapshot"].get("state") == "preparing"
                except (subprocess.CalledProcessError, KeyError):
                    return False
            wait(preparing, timeout=30)
            assert "service.pd-lan-ra" not in modules("b"), "preparation started the LAN RA service"
            def prepared_addresses():
                output = subprocess.run(inside("ha-b", ["ip", "-j", "-6", "address", "show", "dev", "wan-ha"]), capture_output=True, text=True)
                return json.loads(output.stdout) if output.returncode == 0 else []
            wait(lambda: any(a.get("local") == "2001:db8:80:f1::21" for link in prepared_addresses() for a in link.get("addr_info", [])), timeout=3)
            assert preparing(), "B4 was added only after DHCP serving began"
            addresses = {a.get("local"): a for link in prepared_addresses() for a in link.get("addr_info", [])}
            assert addresses.get("fe80::1", {}).get("tentative") and addresses["2001:db8:80:f1::21"].get("tentative"), "link-local and B4 DAD did not overlap"
            api("b", "control.sock", "/v1/services/service.ha/restart", {})
            wait(lambda: active("a"), timeout=30)
            wait(lambda: modules("b").get("service.pd", {}).get("ready") is True, timeout=30)
            pd_traffic("a")
            assert not any(a.get("local") == "2001:db8:80:f1::21" for link in prepared_addresses() for a in link.get("addr_info", [])), "cancelled preparation retained the B4 address"
            assert "service.pd-lan-ra" not in modules("b"), "cancelled preparation left an RA worker"
            assert progress(latest()["sequence"]) == mapping
            print("PASS overlapping DAD, interrupted preparation, B4 withdrawal and real TCP/UDP recovery", flush=True)

            api("a", "control.sock", "/v1/services/service.ha/restart", {})
            wait(preparing, timeout=30)
            wait(lambda: any(a.get("local") == "2001:db8:80:f1::21" for link in prepared_addresses() for a in link.get("addr_info", [])), timeout=3)
            assert preparing() and "service.pd-lan-ra" not in modules("b"), "preparation started serving before the control-stop test"
            authority = api("b", "control.sock", "/v1/services/service.ha")["snapshot"]["expires_nanos"]
            controller = host_pid("b", modules("b")["control"]["pid"])
            os.kill(controller, signal.SIGSTOP)
            try:
                time.sleep(max(0, (authority - 7_200_000_000_000 - time.clock_gettime_ns(time.CLOCK_BOOTTIME)) / 1_000_000_000) + 0.5)
                assert not any(a.get("local") == "2001:db8:80:f1::21" for link in prepared_addresses() for a in link.get("addr_info", [])), "expired preparation retained the B4 address while control was stopped"
            finally:
                try:
                    os.kill(controller, signal.SIGCONT)
                except ProcessLookupError:
                    pass
            print("PASS prepared B4 authority expired while control was stopped", flush=True)
            return
        if dhcp6_server:
            information("a")
            controller = host_pid("a", modules("a")["control"]["pid"])
            addresses = json.loads(run(inside("ha-a", ["ip", "-j", "-6", "address", "show", "dev", "lan0"])))
            target = next(a["local"] for link in addresses for a in link["addr_info"] if a["scope"] == "link" and not a.get("tentative"))
            direct_information(target, True)
            authority = api("a", "control.sock", "/v1/services/service.ha")["snapshot"]["expires_nanos"]
            os.kill(controller, signal.SIGSTOP)
            try:
                time.sleep(max(0, (authority - time.clock_gettime_ns(time.CLOCK_BOOTTIME)) / 1_000_000_000) + 0.3)
                direct_information(target, False)
            finally:
                os.kill(controller, signal.SIGCONT)
            wait(lambda: active("a") or active("b"))
            owner = "a" if active("a") else "b"
            pd_traffic(owner)
            information(owner)
            sequence = latest()["sequence"]
            assert progress(sequence) == mapping, "control loss replaced the established connection"
            if owner != "a":
                api("b", "control.sock", "/v1/services/service.ha/restart", {})
                wait(lambda: active("a"))
                pd_traffic("a")
                information("a")
                sequence = latest()["sequence"]
                assert progress(sequence) == mapping, "return to the original owner did not restore the established connection"
            print("PASS original conditional serving authority expired on the stopped-control node; peer serving and the same TCP/UDP mapping recovered", flush=True)
        print("PASS continuous actual VRRP/NAT TCP and UDP; tcp_loose=0", flush=True)
        for previous, owner in (("a", "b"), ("b", "a")):
            if dhcp6_server:
                pd_server.send_signal(signal.SIGSTOP)
                time.sleep(0.2)
                wait(lambda: pd_saved(previous).get("epoch") == pd_saved(owner).get("epoch") and pd_saved(previous).get("revision") == pd_saved(owner).get("revision"))
                retained = pd_saved(previous)
                old_pd_record = api(previous, "control.sock", "/v1/services/service.pd")
                old_information = api(previous, "control.sock", "/v1/services/service.lan-info")
            before = modules(previous)["service.ha"]["generation"]
            api(previous, "control.sock", "/v1/services/service.ha/restart", {})
            wait(lambda: active(owner))
            wait(lambda: modules(previous).get("service.ha", {}).get("generation", 0) > before and modules(previous).get("service.ha", {}).get("ready"))
            if dhcp6_server:
                pd_traffic(owner)
                information(owner)
                recovered = pd_saved(owner)
                assert recovered["epoch"] > retained["epoch"]
                assert recovered["snapshot"]["client_duid"] == retained["snapshot"]["client_duid"] and recovered["snapshot"]["iaid"] == retained["snapshot"]["iaid"]
                assert [p["prefix"] for p in recovered["snapshot"]["prefixes"]] == [p["prefix"] for p in retained["snapshot"]["prefixes"]]
                for old, new in zip(retained["snapshot"]["prefixes"], recovered["snapshot"]["prefixes"]):
                    assert 0 <= (old["valid_until"]["nanos"] - (7_200_000_000_000 if previous == "b" else 0)) - (new["valid_until"]["nanos"] - (7_200_000_000_000 if owner == "b" else 0)) < 3_000_000_000, "PD takeover extended or lost the original deadline"
                stale = {key: old_pd_record[key] for key in ("supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot")}
                status = run(["curl", "--max-time", "3", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--unix-socket", str(stage / previous / "run/control.sock"), "-H", "Content-Type: application/json", "--data-binary", json.dumps(stale), "http://localhost/v1/services/state"])
                assert status == "409", "old PD owner notification survived demotion"
                stale = {key: old_information[key] for key in ("supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot")}
                status = run(["curl", "--max-time", "3", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--unix-socket", str(stage / previous / "run/control.sock"), "-H", "Content-Type: application/json", "--data-binary", json.dumps(stale), "http://localhost/v1/services/state"])
                assert status == "409", "retired information server notification survived demotion"

                pd_server.send_signal(signal.SIGCONT)
                wait(lambda: pd_observed(owner)["state"] == "bound")
                pd_traffic(owner)
                information(owner)
                print("PASS " + previous + " -> " + owner + " PD handoff with ISC paused, same DUID/IAID/prefix and bounded original deadlines, then Rebind/Reply", flush=True)
            sequence = latest()["sequence"]
            assert progress(sequence) == mapping, "existing connection mapping changed during takeover"
            if pd_dslite:
                entries = run(inside("ha-" + owner, ["conntrack", "-L", "-p", "tcp", "--orig-src", "198.18.0.50", "--dport", "18080"]))
                assert "mark=41 " in entries, "takeover lost the primary TCP connection mark"
            run(inside("ha-client", ["ip", "-4", "neigh", "flush", "to", "198.18.0.1", "dev", "lan0"]))
            assert progress(latest()["sequence"]) == mapping
            neighbor = run(inside("ha-client", ["ip", "-4", "neigh", "show", "198.18.0.1", "dev", "lan0"]))
            assert "00:00:5e:00:01:2a" in neighbor.lower(), "new owner answered with a physical MAC: " + neighbor
            wait(lambda: run(inside("ha-" + previous, ["sysctl", "-n", "net.ipv4.conf.lan0.arp_ignore"])).strip() == "0")
            if pd_dslite:
                check_backup_arp(owner)
                wait(lambda: run(inside("ha-" + previous, ["sysctl", "-n", "net.ipv4.conf.backup0.arp_ignore"])).strip() == "0")
                wait(lambda: run(inside("ha-" + previous, ["sysctl", "-n", "net.ipv4.conf.backup0.arp_announce"])).strip() == "0")
            wait(lambda: modules(owner).get("service.leases", {}).get("ready") is True and api(owner, "control.sock", "/v1/services/service.leases")["snapshot"]["active"] is True)
            before_renew = len(lease_events(0))
            first_client.send_signal(signal.SIGUSR1)
            wait(lambda: len(lease_events(0)) > before_renew)
            assert lease_events(0)[-1].split()[1] == first_address, "takeover changed the client's lease"
            wait(lambda: saved("a")["leases"] == saved("b")["leases"])
            print("PASS continuous same TCP socket and UDP mapping after " + previous + " -> " + owner + " takeover and worker/engine restart", flush=True)
            print("PASS same BusyBox client renewed its replicated address after " + previous + " -> " + owner, flush=True)
            assert progress(latest()["sequence"]) == mapping
        # A real engine crash must revoke this owner's publication and move
        # traffic before a new worker/engine is admitted.
        before = modules("a")["service.ha"]["generation"]
        pids = run(["ip", "netns", "pids", "ha-a"]).split()
        engines = [int(pid) for pid in pids if Path("/proc/" + pid + "/comm").read_text().strip() == "conntrackd"]
        assert len(engines) == 1, "expected exactly one engine owned by VRRP"
        os.kill(engines[0], signal.SIGKILL)
        wait(lambda: active("b"))
        wait(lambda: modules("a").get("service.ha", {}).get("generation", 0) > before and modules("a").get("service.ha", {}).get("ready"))
        sequence = latest()["sequence"]
        assert progress(sequence) == mapping
        print("PASS engine SIGKILL: withdrawal, peer takeover and actual worker/engine recovery", flush=True)
        wait(lambda: modules("b").get("service.leases", {}).get("ready") is True and api("b", "control.sock", "/v1/services/service.leases")["snapshot"]["active"] is True)
        second_client = lease_client(1, first_address)
        second_address = lease_events(1)[-1].split()[1]
        assert second_address != first_address, "second BusyBox client received an existing client's address"
        wait(lambda: saved("a")["leases"] == saved("b")["leases"])
        print("PASS second independent client requested the first address and received a different free address", flush=True)
        old_generation = modules("a")["service.leases"]["generation"]
        standby_before = saved("a")
        api("a", "control.sock", "/v1/services/service.leases/restart", {})
        wait(lambda: modules("a").get("service.leases", {}).get("ready") and modules("a")["service.leases"]["generation"] > old_generation)
        assert saved("a") == standby_before, "standby restart changed original lease deadlines "
        before_renew = len(lease_events(0))
        first_client.send_signal(signal.SIGUSR1)
        wait(lambda: len(lease_events(0)) > before_renew)
        assert lease_events(0)[-1].split()[1] == first_address
        wait(lambda: saved("a")["leases"] == saved("b")["leases"])
        print("PASS standby DHCP worker restart retained the lease file and accepted the next real renewal", flush=True)
        run(["openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-keyout", str(stage / "wrong.key"), "-out", str(stage / "wrong.crt"), "-days", "1", "-subj", "/CN=wrong-peer"])
        refused = subprocess.run(inside("ha-b", ["curl", "--max-time", "3", "--noproxy", "*", "-sS", "--cert", str(stage / "wrong.crt"), "--key", str(stage / "wrong.key"), "--cacert", str(stage / "a.crt"), "https://169.254.42.2:8443/leases"]), capture_output=True)
        assert refused.returncode != 0, "unconfigured peer certificate reached lease synchronization"
        print("PASS mutual TLS rejects an unconfigured peer certificate", flush=True)
        before_renew = len(lease_events(0))
        sync_down = time.time()
        # Isolate the replication link without changing the declared interface.
        run(["ip", "link", "set", "sync0", "down"])
        first_client.send_signal(signal.SIGUSR1)
        wait(lambda: len(lease_events(0)) > before_renew)
        assert lease_events(0)[-1].split()[1] == first_address
        wait(lambda: bool(api("b", "control.sock", "/v1/services/service.leases")["snapshot"].get("sync", {}).get("error")))
        before = modules("b")["service.leases"]["generation"]
        api("b", "control.sock", "/v1/services/service.leases/restart", {})
        wait(lambda: modules("b").get("service.leases", {}).get("generation", 0) > before and modules("b")["service.leases"]["ready"])
        before_renew = len(lease_events(0))
        first_client.send_signal(signal.SIGUSR1)
        wait(lambda: len(lease_events(0)) > before_renew)
        assert lease_events(0)[-1].split()[1] == first_address
        third_client = lease_client(2, first_address)
        third_address = lease_events(2)[-1].split()[1]
        assert third_address not in (first_address, second_address), "isolated allocator duplicated a live lease"
        assert "UP" not in json.loads(run(["ip", "-j", "link", "show", "sync0"]))[0]["flags"], "sync outage was not maintained"
        if stale_owner:
            # A sleeping client cannot answer ARP, but its unsynchronized
            # binding remains valid on the disconnected former owner.
            run(inside("ha-client", ["ip", "address", "flush", "dev", "lease2"]))
            run(inside("ha-client", ["ip", "link", "set", "lease2", "down"]))
            os.kill(product_pid(b_daemon), signal.SIGKILL)
            b_daemon.wait(timeout=10)
            wait(lambda: not run(["ip", "netns", "pids", "ha-b"]).split(), timeout=30)
            wait(lambda: active("a"), timeout=30)
            def lease_status():
                return api("a", "control.sock", "/v1/services/service.leases")["snapshot"]
            wait(lambda: lease_status().get("active") and lease_status().get("sync", {}).get("allocation_blocked"), timeout=30)
            fourth_client = lease_client(3, third_address, False)
            time.sleep(3)
            assert not (stage / "lease3.progress").exists(), "stale owner allocated an unknown lease"
            before_renew = len(lease_events(0))
            first_client.send_signal(signal.SIGUSR1)
            wait(lambda: len(lease_events(0)) > before_renew)
            assert lease_events(0)[-1].split()[1] == first_address
            generation = modules("a")["service.leases"]["generation"]
            api("a", "control.sock", "/v1/services/service.leases/restart", {})
            wait(lambda: modules("a").get("service.leases", {}).get("generation", 0) > generation and modules("a")["service.leases"]["ready"], timeout=30)
            wait(lambda: lease_status().get("sync", {}).get("allocation_blocked"))
            assert not (stage / "lease3.progress").exists(), "restart lost the stale-owner restriction"
            start("ha-b", b_command, "b")
            def standby_ready():
                try:
                    return modules("b").get("service.leases", {}).get("ready") is True and api("b", "control.sock", "/v1/services/service.leases")["snapshot"].get("standby")
                except (subprocess.CalledProcessError, json.JSONDecodeError):
                    return False
            wait(standby_ready, timeout=30)
            run(["ip", "link", "set", "sync0", "up"])
            def reconciled():
                status = lease_status().get("sync")
                return status is not None and not status.get("allocation_blocked", False)
            wait(reconciled, timeout=45)
            wait(lambda: (stage / "lease3.progress").exists(), timeout=30)
            fourth_address = lease_events(3)[-1].split()[1]
            assert fourth_address != third_address, "reconciled owner duplicated the offline lease"
            run(inside("ha-client", ["ip", "address", "replace", third_address + "/24", "dev", "lease2"]))
            run(inside("ha-client", ["ip", "link", "set", "lease2", "up"]))
            before = len(lease_events(2))
            third_client.send_signal(signal.SIGUSR1)
            wait(lambda: len(lease_events(2)) > before, timeout=20)
            assert lease_events(2)[-1].split()[1] == third_address
            print("PASS stale takeover blocked unknown leases, survived restart and preserved the offline client after reconciliation", flush=True)
            return
        if dhcp6_server:
            wait(lambda: bool(pd_observed("a").get("sync", {}).get("error")))
            pd_traffic("b")
        sync_up = time.time()
        run(["ip", "link", "set", "sync0", "up"])
        wait(lambda: saved("a")["leases"] == saved("b")["leases"] and len(saved("a")["leases"]) == 3)
        def synchronized():
            snapshot = api("b", "control.sock", "/v1/services/service.leases")["snapshot"]
            state = snapshot.get("sync", {})
            return state.get("synchronized") and not state.get("error")
        wait(synchronized)
        print("PASS isolated DHCP owner renewed, restarted, allocated a distinct lease and synchronized after recovery", flush=True)
        if dhcp6_server:
            wait(lambda: pd_observed("a").get("sync") and not pd_observed("a")["sync"].get("error"))
            print("PASS PD sync outage reported on standby without interrupting the owner's IPv6 forwarding; recovery refreshed synchronization", flush=True)
        def holds(node):
            path = stage / node / "state/services/leases/dhcp-sticky.db"
            with sqlite3.connect("file:" + str(path) + "?mode=ro", uri=True) as db:
                return sorted(db.execute("SELECT mac,ip,sticky_until FROM dhcp_sticky_leases WHERE sticky_until>?", (time.time_ns(),)).fetchall())
        granted = saved("b")["leases"]
        removal_started = time.time_ns()
        if release_holds:
            previous_worker = modules("b")["service.leases"]
            init = product_pid(b_daemon)
            local_worker = str(previous_worker["pid"])
            run(["ip", "link", "set", "sync0", "down"])
            run(["nsenter", "-t", str(init), "-p", "kill", "-STOP", local_worker])
            for process in (first_client, second_client, third_client):
                process.send_signal(signal.SIGUSR2)
            # The file has removed the grants, while del hooks are queued at
            # their paused owner. Normal product shutdown must drain them.
            wait(lambda: not saved("b")["leases"])
            os.kill(init, signal.SIGTERM)
            run(["nsenter", "-t", str(init), "-p", "kill", "-CONT", local_worker])
            b_daemon.wait(timeout=35)
            assert len(holds("b")) == 3, "normal stop lost queued removal events"
            original_holds = holds("b")
            wait(lambda: active("a"), timeout=30)
            run(["ip", "link", "set", "sync0", "up"])
            b_daemon = start("ha-b", b_command, "b")
            wait(lambda: daemon_ready(b_daemon, "b"), timeout=30)
            wait(lambda: holds("a") == holds("b") == original_holds)
            wait(lambda: not saved("b")["leases"] and not saved("a")["leases"])
        for process in (first_client, second_client, third_client):
            process.terminate()
            process.wait(timeout=3)
        if not release_holds:
            last_expiry = max(int(lease[0]) for lease in granted)
            wait(lambda: not saved("b")["leases"] and not saved("a")["leases"], timeout=max(0, last_expiry-time.time())+8)
            print("PASS stopped clients expired at their actual granted deadlines on both members", flush=True)
        else:
            print("PASS explicit client RELEASE removed active bindings on both members", flush=True)
        wait(lambda: len(holds("b")) == 3 and holds("a") == holds("b"))
        retained_holds = holds("b")
        expiry_by_address = {lease[2]: int(lease[0]) for lease in granted}
        with sqlite3.connect("file:" + str(stage / "b/state/services/leases/dhcp-sticky.db") + "?mode=ro", uri=True) as db:
            removals = db.execute("SELECT ip,released_at,sticky_until FROM dhcp_sticky_leases WHERE sticky_until>?", (time.time_ns(),)).fetchall()
        for address,removed,until in removals:
            assert removed >= (removal_started if release_holds else expiry_by_address[address]*1_000_000_000), "removal preceded the actual release or expiry"
            assert until-removed == 3*86400*1_000_000_000, "hold does not last three days from the actual removal"
        fourth_client = lease_client(3, first_address)
        assert lease_events(3)[-1].split()[1] not in expiry_by_address, "a different client received a held address"
        print("PASS three-day sticky hosts excluded held addresses from a new independent client", flush=True)
        if not release_holds:
            os.kill(product_pid(b_daemon), signal.SIGTERM)
            b_daemon.wait(timeout=35)
            wait(lambda: active("a"), timeout=30)
            restarted = start("ha-b", b_command, "b")
            wait(lambda: daemon_ready(restarted, "b"), timeout=30)
            wait(lambda: holds("a") == holds("b") == retained_holds)
        assert progress(latest()["sequence"]) == mapping
        assert holds("a") == retained_holds, "whole-daemon restart or handoff extended the hold deadlines"
        before_reconnect = len(lease_events(0))
        first_client = start("ha-client", ["busybox", "udhcpc", "-f", "-i", "lease0", "-s", str(hook), "-p", str(stage / "lease0.pid"), "-r", first_address, "-t", "3", "-T", "1", "-A", "1"], "lease0")
        wait(lambda: len(lease_events(0)) > before_reconnect)
        assert lease_events(0)[-1].split()[1] == first_address, "the returning client lost its held address"
        wait(lambda: all(ip != first_address for _,ip,_ in holds("a")))
        print("PASS product restart and handoff retained original sticky deadlines; the returning client reacquired its address", flush=True)

        final_observation = None
        if pd_dslite:
            assert progress(latest()["sequence"]) == mapping
            final_observation = latest()
            client.terminate()
            client.wait(timeout=3)
        if dhcp6_server:
            wait(lambda: pd_observed("b")["state"] == "bound")
            pd_server.terminate()
            pd_server.wait(timeout=5)
            wait(lambda: pd_saved("a").get("epoch") == pd_saved("b").get("epoch") and pd_saved("a").get("revision") == pd_saved("b").get("revision"))
            original = pd_saved("b")
            before = modules("b")["service.ha"]["generation"]
            api("b", "control.sock", "/v1/services/service.ha/restart", {})
            wait(lambda: active("a"))
            wait(lambda: modules("b").get("service.ha", {}).get("generation", 0) > before and modules("b").get("service.ha", {}).get("ready"))
            pd_traffic("a")
            restored = pd_saved("a")["snapshot"]
            original_deadline = original["snapshot"]["prefixes"][0]["valid_until"]["nanos"] - 7_200_000_000_000
            assert restored["prefixes"][0]["valid_until"]["nanos"] <= original_deadline
            def no_global(node, iface):
                return all(a["scope"] != "global" for link in json.loads(run(inside(node, ["ip", "-j", "-6", "addr", "show", "dev", iface]))) for a in link["addr_info"])
            remaining = max(0, (original_deadline - time.clock_gettime_ns(time.CLOCK_BOOTTIME)) / 1_000_000_000)
            try:
                wait(lambda: not pd_observed("a").get("prefixes") and no_global("ha-a", "lan0") and no_global("ha-client", "lan0"), remaining + 8)
            except TimeoutError:
                for node in ("a", "b"):
                    print("PD expiry observation", node, pd_observed(node), flush=True)
                    print("PD expiry LAN", node, run(inside("ha-" + node, ["ip", "-j", "-6", "address", "show", "dev", "lan0"])), flush=True)
                print("PD expiry client", run(inside("ha-client", ["ip", "-j", "-6", "address", "show", "dev", "lan0"])), flush=True)
                raise
            before = modules("a")["service.pd"]["generation"]
            api("a", "control.sock", "/v1/services/service.pd/restart", {})
            wait(lambda: modules("a").get("service.pd", {}).get("generation", 0) > before and modules("a").get("service.pd", {}).get("ready"))
            assert not pd_observed("a").get("prefixes") and no_global("ha-a", "lan0") and no_global("ha-client", "lan0"), "expired replicated PD revived on restart"
            if pd_dslite:
                def current_egress(interface):
                    return next((node for node in ("a", "b") if active(node) and egress_path(node, interface)), "")
                wait(lambda: current_egress("backup-ha"))
                egress_traffic("backup", current_egress("backup-ha"))
            pd_server = pd_start()
            if pd_dslite:
                wait(lambda: current_egress("dslite0"))
                pd_traffic(current_egress("dslite0"))
                egress_traffic("DSLite", current_egress("dslite0"))
                print("PASS expired PD withdrew the B4 source; fresh IPv4 TCP/UDP used backup WAN and recovered through DS-Lite", flush=True)
            else:
                pd_traffic("a")
            print("PASS server-absent cross-clock PD takeover expired at original deadline on router/SLAAC client; restart did not revive PD and ISC recovery restored TCP/UDP", flush=True)
        print("Observed UDP datagrams lost during transitions:", (final_observation or latest())["udp_lost"], flush=True)
        print("Longest TCP reply wait across transitions:", (final_observation or latest())["longest_tcp_reply_seconds"], "seconds", flush=True)
        if client.poll() is None:
            client.terminate()
            client.wait(timeout=3)
        retired = {"schema_version": 2, "resources": [
            {"id": "lan", "interface": {"name": "lan0"}},
            {"id": "wan", "interface": {"name": "wan0"}},
            {"id": "lan-base", "address": {"interface": "lan0", "cidr": "198.18.0.2/24"}},
            {"id": "wan-base", "address": {"interface": "wan0", "cidr": "203.0.113.2/24"}},
            {"id": "forward", "forwarding": {"ipv4": True, "ipv4_rp_filter": 0}}]}
        api("a", "control.sock", "/v1/config/commit", {"expected_revision": 1, "request_id": "retire-ha", "config": retired})
        assert run(inside("ha-a", ["sysctl", "-n", "net.netfilter.nf_conntrack_tcp_be_liberal"])).strip() == "0", "retired managed sysctl was not restored"
        assert run(inside("ha-a", ["sysctl", "-n", "net.ipv4.conf.lan0.arp_ignore"])).strip() == "0", "retired VRRP ARP setting was not restored"
        assert "service.ha" not in modules("a"), "retired sync worker remains registered"
        print("PASS configuration removal reaped sync worker and restored original conntrack sysctl", flush=True)
        for process in captures:
            process.send_signal(signal.SIGINT)
            process.wait(timeout=3)
        segment, source = ("lan", "198.18.0.50") if pd_dslite else ("wan", "203.0.113.1")
        syn = run(["tcpdump", "-nn", "-r", str(stage / (segment + ".pcap")), "src host " + source + " and dst port 18080 and tcp[tcpflags] & tcp-syn != 0"])
        assert len(syn.splitlines()) == 1, "TCP connection was restarted or never established: " + syn
        sync = run(["tcpdump", "-nn", "-r", str(stage / "sync.pcap"), "src net 169.254.0.0/16 and dst net 169.254.0.0/16 and udp port 3780"])
        assert len(sync.splitlines()) > 0, "No actual conntrackd replication packets"
        assert (stage / "server.log").read_text().count("accepted") == 1, "server accepted a replacement TCP socket"
        dhcp = run(["tcpdump", "-tt", "-nn", "-vvv", "-r", str(stage / "lan.pcap"), "udp port 67 or udp port 68"])
        when, requests, acknowledgments = 0, 0, 0
        for line in dhcp.splitlines():
            stamp = re.match(r"^(\d+\.\d+) ", line)
            if stamp:
                when = float(stamp.group(1))
            if sync_down <= when < sync_up and "DHCP-Message" in line:
                requests += line.rstrip().endswith(": Request")
                acknowledgments += line.rstrip().endswith(": ACK")
        assert requests > 0 and acknowledgments > 0, "capture did not prove DHCP renewal during the sync outage"
        print("PASS captures: one TCP SYN, IPv4 link-local FTFW replication, unchanged UDP NAT tuple", flush=True)
        print("PASS two-way state handoff without TCP reconnect", flush=True)
        if dhcp6_server:
            packets = run(["tcpdump", "-n", "-tt", "-vv", "-r", str(stage / "wan.pcap"), "ip6"])
            assert "dhcp6 rebind" in packets and "dhcp6 release" not in packets
            print("PASS capture: recovery Rebind and no shared-lease Release on member demotion/restart", flush=True)
        if pd_dslite:
            encapsulated = run(["tcpdump", "-nn", "-r", str(stage / "wan.pcap"), "ip6 proto 4"]).splitlines()
            for port in (18080, 18081):
                for direction in ("IP6 2001:db8:ffff::20 >", " > 2001:db8:ffff::20:"):
                    assert any(direction in line and "203.0.113.50." + str(port) in line for line in encapsulated), "persistent application traffic did not traverse DS-Lite in both directions"
            assert not run(["tcpdump", "-nn", "-r", str(stage / "wan.pcap"), "ip and host 203.0.113.50 and (port 18080 or port 18081)"]), "persistent application bypassed the AFTR tunnel"
            assert not run(["tcpdump", "-nn", "-r", str(stage / "backup.pcap"), "tcp port 18080"]), "primary TCP escaped through the backup WAN"
            for port in (18082, 18083):
                assert run(["tcpdump", "-nn", "-r", str(stage / "backup.pcap"), "src host 192.0.2.1 and dst host 203.0.113.50 and dst port " + str(port)]), "missing backup WAN application traffic"
            print("PASS captures: bidirectional IPv4-in-IPv6 and independent backup WAN TCP/UDP", flush=True)
    except Exception:
        for node in ("a", "b"):
            if not (stage / node / "run/control.sock").exists():
                continue
            for socket, path in (("supervisor.sock", "/v1/status"), ("control.sock", "/v1/services"), ("control.sock", "/v1/network")):
                result = subprocess.run(["curl", "--max-time", "2", "-sS", "--unix-socket", str(stage / node / "run" / socket), "http://localhost" + path], capture_output=True, text=True)
                print("failure", node, path, result.stdout, flush=True)
        raise
    finally:
        signal.alarm(0)
        for process in reversed(children):
            if process.poll() is None:
                process.terminate()
        for process in reversed(children):
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for ns in reversed(namespaces):
            subprocess.run(["ip", "netns", "del", ns], capture_output=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--dhcp6-server", type=Path)
    parser.add_argument("--pd-dslite", action="store_true")
    parser.add_argument("--stale-owner", action="store_true")
    parser.add_argument("--preparation", action="store_true")
    parser.add_argument("--delegated-dns", action="store_true")
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--release-holds", action="store_true", help="Exercise client RELEASE before sticky recovery; otherwise wait for natural lease expiry")
    args = parser.parse_args()
    if args.inside:
        peer(args.inside, args.dhcp6_server, args.pd_dslite, args.stale_owner, args.preparation, args.delegated_dns, args.release_holds)
        return
    os.chdir(Path(__file__).resolve().parent.parent)
    if args.pd_dslite and not args.dhcp6_server:
        parser.error("--pd-dslite requires --dhcp6-server")
    if args.preparation and not args.pd_dslite:
        parser.error("--preparation requires --pd-dslite")
    if args.delegated_dns and not args.dhcp6_server:
        parser.error("--delegated-dns requires --dhcp6-server")
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    for module in ("nf_conntrack", "nf_conntrack_netlink"):
        run(root + ["modprobe", module])
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-ha.XXXXXX"]).strip()
    try:
        for source, name in [(Path(__file__), "peer.py"), (Path("dist/conntrack-engine.tar"), "conntrack-engine.tar"), (Path("dist/conntrack-sources.tar"), "conntrack-sources.tar"), (Path("dist/dnsmasq-engine.tar"), "dnsmasq-engine.tar"), (Path("dist/dnsmasq-sources.tar"), "dnsmasq-sources.tar"), *[(args.binary_dir / name, name) for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle")]]:
            run(root + ["install", "-m", "500", str(source.resolve()), stage + "/" + name])
        peer_args = []
        if args.dhcp6_server:
            run(root + ["install", "-m", "500", str(args.dhcp6_server.resolve()), stage + "/dhcpd"])
            peer_args = ["--dhcp6-server", stage + "/dhcpd"]
        if args.pd_dslite:
            peer_args.append("--pd-dslite")
        if args.stale_owner:
            peer_args.append("--stale-owner")
        if args.preparation:
            peer_args.append("--preparation")
        if args.delegated_dns:
            peer_args.append("--delegated-dns")
        if args.release_holds:
            peer_args.append("--release-holds")
        result = subprocess.run(root + ["unshare", "--mount", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", stage] + peer_args, timeout=380)
        if result.returncode:
            for name in ("a.log", "b.log", "client.log", "server.log", "dhcp6.log", "http6.log", "udp6.log", "egress-primary.log", "egress-backup.log"):
                log = subprocess.run(root + ["tail", "-30", stage + "/" + name], capture_output=True, text=True)
                print(name + ":\n" + log.stdout)
            for segment in ("wan", "lan"):
                packets = subprocess.run(root + ["tcpdump", "-nn", "-tt", "-r", stage + "/" + segment + ".pcap", "ip6 proto 4" if segment == "wan" else "tcp port 18080"], capture_output=True, text=True)
                print(segment + " packet tail:\n" + "\n".join(packets.stdout.splitlines()[-35:]))
            raise RuntimeError("HA communication failed")
    finally:
        run(root + ["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
