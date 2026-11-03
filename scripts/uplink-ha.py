#!/usr/bin/env python3
"""Run paired native homert profiles against a real HGW and independent peers."""
import argparse
from datetime import datetime
import ipaddress
import json
import os
from pathlib import Path
import re
import runpy
import signal
import subprocess
import tarfile
import time


def run(command, timeout=15):
    p = subprocess.run(command, capture_output=True, text=True, timeout=timeout)
    if p.returncode:
        raise RuntimeError(f"{command[0]}: {p.stderr[-2000:]} {p.stdout[-2000:]}")
    return p.stdout


def owner(stage, uplink):
    def interrupted(*_):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(600)
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/var/lib/dhcpcd"])
    namespaces, children = [], []
    prefix = "r2fh-" + str(os.getpid())
    names = {key: prefix + "-" + key for key in ("wire", "a", "b", "client", "gateway", "tail", "bgp0", "bgp1")}
    def inside(key, command):
        return ["ip", "netns", "exec", names[key]] + command
    def start(key, command, name):
        with (stage / (name + ".log")).open("a") as out:
            p = subprocess.Popen(inside(key, command), stdout=out, stderr=subprocess.STDOUT)
        children.append(p)
        return p
    def wait(condition, seconds=90):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            if condition():
                return
            time.sleep(.2)
        raise RuntimeError("full native HA readiness deadline")
    def api(node, socket, path):
        return json.loads(run(["curl", "--fail-with-body", "--max-time", "4", "-sS", "--unix-socket", str(stage / node / "run" / socket), "http://localhost" + path]))
    def connect(key, device, bridge, mac=None):
        port = key + "-" + device
        run(["ip", "link", "add", device, "netns", names[key], "type", "veth", "peer", "name", port, "netns", names["wire"]])
        run(inside("wire", ["ip", "link", "set", port, "master", bridge, "up"]))
        command = ["ip", "link", "set", device]
        if mac:
            command += ["address", mac]
        run(inside(key, command + ["up"]))
    try:
        for key in names:
            run(["ip", "netns", "add", names[key]])
            namespaces.append(names[key])
            run(inside(key, ["ip", "link", "set", "lo", "up"]))
        for bridge in ("lan", "wan", "mgmt", "bgp", "sync", "tail"):
            run(inside("wire", ["ip", "link", "add", bridge, "type", "bridge"]))
            run(inside("wire", ["ip", "link", "set", bridge, "up"]))
        # Receive the shared WAN MAC once at the physical attachment. The
        # two router ports remain independent veth parents for their VMACs.
        run(["ip", "link", "add", "link", uplink, "name", "uplink", "netns", names["wire"], "type", "macvlan", "mode", "bridge"])
        run(inside("wire", ["ip", "link", "set", "wan", "address", "02:72:6f:75:74:07"]))
        run(inside("wire", ["ip", "link", "set", "uplink", "address", "02:00:5e:00:01:43", "master", "wan", "up"]))
        run(inside("wire", ["bridge", "fdb", "del", "02:00:5e:00:01:43", "dev", "uplink", "master"]))
        for node in ("a", "b"):
            for device, bridge in (("lan0", "lan"), ("wan0", "wan"), ("mgmt0", "mgmt"), ("bgp0", "bgp"), ("sync0", "sync"), ("peer0", "tail")):
                connect(node, device, bridge)
        connect("client", "client0", "lan", "02:00:00:00:00:03")
        start("client", ["tcpdump", "-n", "-l", "-v", "-e", "-i", "client0", "-c", "80", "icmp6 and (ip6[40] == 134 or ip6[40] == 136)"], "client-ra")
        connect("client", "mgmt0", "mgmt")
        run(inside("client", ["ip", "address", "add", "198.18.0.254/32", "dev", "lo"]))
        run(inside("client", ["ip", "address", "add", "198.19.0.254/24", "dev", "mgmt0"]))
        connect("gateway", "lan0", "lan")
        run(inside("gateway", ["ip", "address", "add", "198.18.0.253/24", "dev", "lan0"]))
        run(["ip", "link", "add", "link", uplink, "name", "wan0", "netns", names["gateway"], "type", "macvlan", "mode", "bridge"])
        run(inside("gateway", ["ip", "link", "set", "wan0", "address", "02:72:6f:75:74:06", "up"]))
        hook = stage / "gateway.sh"
        hook.write_text('#!/bin/sh\ncase "$1" in deconfig) ip -4 address flush dev "$interface"; ip -4 route flush dev "$interface";; bound|renew) ip address replace "$ip/$subnet" dev "$interface"; set -- $router; ip route replace default via "$1" dev "$interface";; esac\n')
        hook.chmod(0o500)
        start("gateway", ["busybox", "udhcpc", "-f", "-i", "wan0", "-s", str(hook), "-p", str(stage / "gateway.pid")], "gateway-dhcp")
        wait(lambda: bool(json.loads(run(inside("gateway", ["ip", "-j", "route", "show", "default"])))))
        run(inside("gateway", ["sysctl", "-qw", "net.ipv4.ip_forward=1"]))
        nat = stage / "gateway.nft"
        nat.write_text('table ip gateway {\n chain nat {\n type nat hook postrouting priority srcnat;\n ip saddr 198.18.0.0/24 oifname "wan0" masquerade\n }\n}\n')
        run(inside("gateway", ["nft", "-f", str(nat)]))
        for key, address, port, answer in (("gateway", "198.18.0.253", 53, None), ("client", "198.18.0.254", 5353, "192.0.2.77"), ("client", "198.18.0.254", 5354, "192.0.2.78")):
            command = ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-interfaces", "--listen-address=" + address, "--port=" + str(port)]
            command += ["--address=/#/" + answer] if answer else ["--server=1.1.1.1"]
            start(key, command, "dns-" + str(port))
        start("gateway", ["tcpdump", "-n", "-l", "-e", "-i", "lan0", "-c", "100", "arp or udp port 53"], "gateway-wire")
        echo = runpy.run_path(str(stage / "peer-fixture.py"))
        for index in range(2):
            key = "bgp" + str(index)
            address = "192.0.2." + str(index+2)
            connect(key, "eth0", "bgp")
            run(inside(key, ["ip", "address", "add", address + "/24", "dev", "eth0"]))
            run(inside(key, ["ip", "address", "add", "203.0.113.1/24", "dev", "lo"]))
            root = Path("/run/routerd-bgp-peer" + str(index))
            root.mkdir(mode=0o755)
            run(["chown", "frr:frr", str(root)])
            (root / "zebra.conf").write_text("hostname independent\nlog stdout\n")
            (root / "bgpd.conf").write_text("hostname independent\nlog stdout\nrouter bgp 64512\n bgp router-id " + address + "\n no bgp ebgp-requires-policy\n neighbor 192.0.2.10 remote-as 64500\n neighbor 192.0.2.11 remote-as 64500\n address-family ipv4 unicast\n  network 203.0.113.0/24\n  neighbor 192.0.2.10 activate\n  neighbor 192.0.2.11 activate\n exit-address-family\n")
            start(key, ["/usr/lib/frr/zebra", "-u", "frr", "-g", "frr", "-z", str(root / "zserv.api"), "--vty_socket", str(root), "-i", str(root / "zebra.pid"), "-f", str(root / "zebra.conf")], key + "-zebra")
            wait(lambda: (root / "zserv.api").exists())
            start(key, ["/usr/lib/frr/bgpd", "-S", "-z", str(root / "zserv.api"), "--vty_socket", str(root), "-i", str(root / "bgpd.pid"), "-f", str(root / "bgpd.conf")], key + "-bgpd")
            start(key, ["python3", "-u", "-c", echo["ECHO_SERVER"], "203.0.113.1"], key + "-echo")
        # The existing UDP syslog fixture binds 192.0.2.2.
        start("bgp0", ["python3", "-I", str(stage / "syslog-peer.py"), "--mode", "receive", "--port", "5514", "--output", str(stage / "syslog.jsonl")], "syslog-peer")
        collector = stage / "collector.json"
        collector.write_text(json.dumps({"receivers": {"otlp": {"protocols": {"grpc": {"endpoint": "192.0.2.2:4317"}}}}, "exporters": {"file": {"path": str(stage / "otlp.jsonl"), "rotation": {"max_megabytes": 8, "max_backups": 1}}}, "service": {"telemetry": {"logs": {"level": "error"}, "metrics": {"level": "none"}}, "pipelines": {s: {"receivers": ["otlp"], "exporters": ["file"]} for s in ("logs", "metrics", "traces")}}}))
        start("bgp0", ["/usr/local/bin/otelcol-contrib", "--config", str(collector)], "collector")
        ntp = stage / "chrony"
        ntp.mkdir()
        with tarfile.open(stage / "chrony-engine.tar") as archive:
            archive.extractall(ntp, filter="data")
        loader = next((ntp / "lib").glob("ld-musl-*.so.1"))
        chrony = [str(loader), "--library-path", str(ntp / "lib") + ":" + str(ntp / "usr/lib"), str(ntp / "usr/sbin/chronyd")]
        conf = stage / "chrony.conf"
        conf.write_text(f"local stratum 8\nbindaddress 198.18.0.254\nallow 198.18.0.0/24\ncmdport 0\nbindcmdaddress {stage}/ntp.sock\npidfile {stage}/ntp.pid\ndriftfile {stage}/ntp.drift\n")
        start("client", chrony + ["-d", "-x", "-u", "root", "-f", str(conf)], "ntp-peer")
        ntp_client = stage / "ntp-client.conf"
        ntp_client.write_text(f"server 198.18.0.1 iburst maxsamples 1\nport 0\ncmdport 0\npidfile {stage}/ntp-client.pid\n")
        connect("tail", "eth0", "tail")
        run(inside("tail", ["ip", "address", "add", "198.51.100.1/24", "dev", "eth0"]))
        private = Path("/run/routerd-ha")
        private.mkdir(mode=0o700)
        for index,node in enumerate(("a", "b")):
            run(["openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-days", "1", "-keyout", str(private / ("node-" + node + ".key")), "-out", str(private / ("node-" + node + ".crt")), "-subj", "/CN=" + node, "-addext", "subjectAltName=IP:169.254.42." + str(index+2), "-addext", "extendedKeyUsage=serverAuth,clientAuth"])
        cert,key = stage / "control.crt", stage / "control.key"
        run(["openssl", "req", "-config", "/dev/null", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-subj", "/CN=routerd-ha", "-addext", "subjectAltName=IP:198.51.100.1"])
        roots = Path("/etc/ssl/certs/ca-certificates.crt")
        trusted = stage / "roots.pem"
        trusted.write_bytes(roots.read_bytes() + cert.read_bytes())
        run(["mount", "--bind", str(trusted), str(roots)])
        policy = stage / "tail-policy.json"
        policy.write_text(json.dumps({"grants": [{"src": ["*"], "dst": ["*"], "ip": ["*"]}], "autoApprovers": {"routes": {"198.18.0.0/24": ["fixture@"]}, "exitNode": ["fixture@"]}}))
        hs = stage / "headscale.json"
        hs.write_text(json.dumps({"server_url": "https://198.51.100.1:8443", "listen_addr": "198.51.100.1:8443", "noise": {"private_key_path": str(stage / "noise.key")}, "prefixes": {"v4": "100.64.0.0/10", "v6": "fd7a:115c:a1e0::/48"}, "database": {"type": "sqlite", "sqlite": {"path": str(stage / "control.db")}}, "derp": {"urls": [], "paths": [], "auto_update_enabled": False, "server": {"enabled": True, "region_id": 999, "region_code": "fixture", "region_name": "isolated DERP", "stun_listen_addr": "0.0.0.0:3478", "private_key_path": str(stage / "derp.key"), "automatically_add_embedded_derp_region": True, "ipv4": "198.51.100.1"}}, "dns": {"override_local_dns": False, "magic_dns": True, "base_domain": "test.invalid"}, "unix_socket": str(stage / "headscale.sock"), "tls_cert_path": str(cert), "tls_key_path": str(key), "policy": {"mode": "file", "path": str(policy)}, "disable_check_updates": True, "log": {"level": "warn"}}))
        head = [str(stage / "headscale"), "-c", str(hs)]
        start("tail", head + ["serve"], "headscale")
        wait(lambda: (stage / "headscale.sock").exists())
        user = json.loads(run(inside("tail", head + ["users", "create", "fixture", "-o", "json"])))
        auth = json.loads(run(inside("tail", head + ["preauthkeys", "create", "--user", str(user["id"]), "--reusable", "-o", "json"])))
        peer_root = stage / "tail-peer"
        peer_root.mkdir(mode=0o700)
        peer_key = peer_root / "auth.key"
        peer_key.write_text(auth["key"])
        peer_key.chmod(0o600)
        peer_socket = peer_root / "tailscaled.sock"
        start("tail", ["env", "TS_LOGS_DIR="+str(peer_root), str(stage / "tailscaled"), "--state="+str(peer_root / "state"), "--statedir="+str(peer_root), "--socket="+str(peer_socket), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "tail-peer")
        wait(peer_socket.exists)
        peer_cli = [str(stage / "tailscale"), "--socket="+str(peer_socket)]
        run(inside("tail", peer_cli + ["up", "--timeout=15s", "--login-server=https://198.51.100.1:8443", "--hostname=independent-peer", "--auth-key=file:"+str(peer_key), "--accept-dns=false", "--accept-routes=true", "--netfilter-mode=off"]), 20)
        peer_status = json.loads(run(inside("tail", peer_cli + ["status", "--json"])))
        peer_ips = {ipaddress.ip_address(a).version: a for a in peer_status["TailscaleIPs"]}
        for address in peer_ips.values():
            name = "tail-echo-"+address.replace(":", "_")
            start("tail", ["python3", "-u", "-c", echo["ECHO_SERVER"], address], name)
            wait(lambda: "READY" in (stage / (name+".log")).read_text())
        start("tail", ["tcpdump", "-n", "-Z", "root", "-l", "-i", "tailscale0", "-c", "80", "tcp port 18080 or udp port 18081"], "tail-flow-peer")
        for node in ("a", "b"):
            p = private / ("tailscale-" + node + ".key")
            p.write_text(auth["key"])
            p.chmod(0o600)
        roles = ("logging", "network", "control", "management", "ra_client", "dhcp4", "dhcp6", "dhcp_server", "dhcp6_server", "ra", "aftr", "dns", "health", "bgp", "ntp", "tailscale", "traffic_flow", "firewall_log", "syslog", "otlp_logs", "web_console", "vrrp")
        management_key = stage / "management-key"
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(management_key)])
        management_public_key = management_key.with_suffix(".pub").read_text().strip()
        daemons = {}
        for index,node in enumerate(("a", "b")):
            directory = stage / node
            directory.mkdir()
            boot = {"platform": "host", "config": str(stage / ("node-" + node + ".yaml")), "runtime_dir": str(directory / "run"), "state_dir": str(directory / "state"), "ssh": {"listen": f"198.19.0.{index+2}:2222", "user": "admin"}, "workers": {r: str(stage / ("routerd-logging" if r=="logging" else "routerd-modules")) for r in roles}, "engines": {r: str(stage / image) for r,image in (("dhcp_server", "dnsmasq-engine.tar"), ("vrrp", "conntrack-engine.tar"), ("ntp", "chrony-engine.tar"), ("tailscale", "tailscale-engine.tar"))}}
            path = directory / "boot.json"
            boot["ssh"]["authorized_keys"] = [management_public_key]
            path.write_text(json.dumps(boot))
            daemons[node] = start(node, [str(stage / "routerd"), "boot", "--file", str(path)], "router-" + node)
            start(node, ["tcpdump", "-n", "-Z", "root", "-l", "-i", "any", "-c", "80", "tcp port 18080 or udp port 18081"], "tail-flow-"+node)
        (stage / "location.json").write_text(json.dumps({"owner_pid": os.getpid(), "namespaces": names, "stage": str(stage)}))
        print("Full native HA owner", os.getpid(), "stage", stage, flush=True)
        def elected():
            active = []
            for node in daemons:
                if daemons[node].poll() is not None:
                    raise RuntimeError("product exited: " + (stage / ("router-" + node + ".log")).read_text()[-3000:])
                if not (stage / node / "run/control.sock").exists():
                    return None
                instances = api(node, "control.sock", "/v1/services").get("instances", [])
                if not any(instance["name"] == "service.lan-ha" for instance in instances):
                    return None
                snapshot = api(node, "control.sock", "/v1/services/service.lan-ha").get("snapshot", {})
                if snapshot.get("active"):
                    active.append(node)
            if len(active)>1:
                raise RuntimeError("two LAN gateways active")
            return active[0] if active else None
        wait(elected, 180)
        print("PASS admitted native HA owner", elected(), flush=True)
        lease = stage / "client.lease"
        hook = stage / "client.sh"
        hook.write_text('#!/bin/sh\ncase "$1" in deconfig) ip -4 address flush dev "$interface"; ip -4 route flush dev "$interface";; bound|renew) ip address replace "$ip/$subnet" dev "$interface"; ip route replace default via "$router" dev "$interface" src "$ip"; printf "%s %s %s\\n" "$ip" "$router" "$dns" > "' + str(lease) + '";; esac\n')
        hook.chmod(0o500)
        dhcp_client = start("client", ["busybox", "udhcpc", "-f", "-i", "client0", "-s", str(hook), "-p", str(stage / "client.pid")], "client-dhcp")
        wait(lease.exists)
        def traffic():
            address,gateway,dns = lease.read_text().split()
            assert gateway==dns=="198.18.0.1", (gateway,dns)
            print("HA client lease", address, "gateway/DNS", gateway, flush=True)
            for family,host in (("-4","1.1.1.1"),("-6","[2606:4700:4700::1111]")):
                code = run(inside("client", ["curl", family, "--noproxy", "*", "--max-time", "8", "--resolve", "one.one.one.one:443:"+host, "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
                assert code in ("200","301","302","308"), (family,code)
                print("PASS HA client HTTPS", family, flush=True)
            for transport in ([],["+tcp"]):
                assert run(inside("client", ["dig", "@"+dns, "example.net", "A", "+short", "+time=3", "+tries=1"]+transport)).strip()
            def advertised_dns():
                advertised = re.findall(r"rdnss option[^\n]* lifetime (\d+)s, addr: ([0-9a-f:]+)", (stage / "client-ra.log").read_text())
                return next((str(ipaddress.ip_address(address)) for seconds,address in reversed(advertised) if int(seconds) > 0), None)
            wait(advertised_dns)
            dns6 = advertised_dns()
            assert run(inside("client", ["dig", "-6", "@"+dns6, "example.net", "AAAA", "+short", "+time=3", "+tries=1"])).strip()
            run(inside("client", chrony + ["-Q", "-t", "8", "-u", "root", "-f", str(ntp_client)]), 12)
            print("PASS HA client NTP response", flush=True)
            node = elected()
            for query_name, expected in (("available.split.test", {"192.0.2.77", "192.0.2.78"}), ("branch.split.test", {"192.0.2.78"}), ("host.corp.test", {"192.0.2.78"})):
                for server in (dns, dns6):
                    answer = run(inside("client", ["dig", "@"+server, query_name, "A", "+short", "+time=3", "+tries=1"])).strip()
                    assert answer in expected, (query_name, answer)
            for server in (dns, dns6):
                assert run(inside("client", ["dig", "@"+server, "www.cloudflare.com", "A", "+short", "+time=4", "+tries=1"])).strip()
            def doh_ready():
                upstreams = api(node, "services/dns-core.dns.sock", "/v1/status")["resources"][0]["observed"]["upstreams"]
                return all(endpoint+":Healthy" in upstreams for endpoint in ("https://cloudflare-dns.com/dns-query", "https://dns.google/dns-query"))
            wait(doh_ready)
            def bgp_ready():
                peers = api(node, "control.sock", "/v1/services/service.routing").get("snapshot", {}).get("peers", [])
                return {p["address"]: p["state"] for p in peers} == {address: "SESSION_STATE_ESTABLISHED" for address in ("192.0.2.2", "192.0.2.3")}
            wait(bgp_ready)
            for index in range(2):
                root = Path("/run/routerd-bgp-peer"+str(index))
                view = json.loads(run(inside("bgp"+str(index), ["vtysh", "--vty_socket", str(root), "-d", "bgpd", "-c", "show bgp ipv4 unicast 198.18.0.0/24 json"])))
                assert view.get("paths"), "independent FRR did not receive the LAN advertisement"
                routes = json.loads(run(inside("bgp"+str(index), ["ip", "-j", "route", "show", "198.18.0.0/24"])))
                expected_hop = "192.0.2.10" if node == "a" else "192.0.2.11"
                hops = [hop.get("gateway") for route in routes for hop in route.get("nexthops", [route])]
                assert hops and set(hops) == {expected_hop}, ("FRR return path bypasses the serving owner", hops)
            run(inside("client", ["python3", "-c", echo["ECHO_CLIENT"], "203.0.113.1", address, address]))
            print("PASS HA conditional DNS/nested suffix, both DoH peers, FRR advertisements and client TCP/UDP without NAT via", node, flush=True)
            print("PASS HA client lease and IPv4/IPv6 HTTPS, UDP/TCP DNS via", elected(), flush=True)
        def client_ipv6_ready():
            addresses = json.loads(run(inside("client", ["ip", "-j", "-6", "address", "show", "dev", "client0", "scope", "global"])))
            usable = any(not info.get("tentative") and not info.get("dadfailed")
                         for link in addresses for info in link.get("addr_info", []))
            return usable and bool(json.loads(run(inside("client", ["ip", "-j", "-6", "route", "show", "default"]))))
        wait(client_ipv6_ready, 90)
        started = time.monotonic()
        def initial_traffic():
            try:
                traffic()
                return True
            except (RuntimeError, AssertionError) as error:
                print("HA startup probe failed", round(time.monotonic()-started, 2), str(error), flush=True)
                return False
        wait(initial_traffic)
        client_address = lease.read_text().split()[0]
        start("client", ["python3", "-u", "-c", echo["ECHO_SERVER"], client_address], "client-echo")
        wait(lambda: "READY" in (stage / "client-echo.log").read_text())
        def tailnet_snapshot(node):
            instances = api(node, "control.sock", "/v1/services").get("instances", [])
            if not any(i["name"] == "service.tailnet" for i in instances):
                return {}
            return api(node, "control.sock", "/v1/services/service.tailnet").get("snapshot", {})
        wait(lambda: tailnet_snapshot(elected()).get("backend_state") == "Running")
        tailnet_ids = {}
        def check_tailnet():
            node = elected()
            snapshot = tailnet_snapshot(node)
            assert snapshot.get("backend_state") == "Running", "elected tailnet node is not running"
            assert snapshot["node_id"] == tailnet_ids.setdefault(node, snapshot["node_id"]), "tailnet identity changed after daemon restart"
            addresses = snapshot["addresses"]
            router6 = next(a for a in addresses if ipaddress.ip_address(a).version == 6)
            for family in (4, 6):
                source = client_address if family == 4 else router6
                run(inside("client", ["python3", "-c", echo["ECHO_CLIENT"], peer_ips[family], source]))
            status = json.loads(run(inside("tail", peer_cli + ["status", "--json"])))
            selected = [p for p in status.get("Peer", {}).values() if "198.18.0.0/24" in p.get("PrimaryRoutes", [])]
            assert len(selected) == 1, "independent peer has no selected LAN subnet route"
            source = {"hgw-router-a": "198.18.0.2", "hgw-router-b": "198.18.0.3"}[selected[0]["HostName"]]
            run(inside("tail", ["python3", "-c", echo["ECHO_CLIENT"], client_address, source]))
            print("PASS HA tailnet LAN-to-peer IPv4/IPv6 and peer-to-LAN IPv4 TCP/UDP; automatic subnet selection", selected[0]["HostName"], flush=True)
        wait(lambda: any("198.18.0.0/24" in p.get("PrimaryRoutes", []) for p in json.loads(run(inside("tail", peer_cli + ["status", "--json"]))).get("Peer", {}).values()))
        check_tailnet()
        def check_management():
            for node in daemons:
                address = "198.19.0.2" if node == "a" else "198.19.0.3"
                status = json.loads(run(inside("client", ["ssh", "-i", str(management_key), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+str(stage / "known-hosts"), "-o", "ConnectTimeout=3", "-p", "2222", "admin@"+address, "show status"])))
                assert status["ready"], "management SSH reported a router unavailable"
            node = elected()
            address = "198.19.0.2" if node == "a" else "198.19.0.3"
            for target, server in ((node, "127.0.0.1"), ("client", address)):
                assert run(inside(target, ["dig", "@"+server, "router.home.arpa", "A", "+short", "+time=3", "+tries=1"])).strip() == "198.18.0.1"
            history = api(node, "control.sock", "/v1/services/service.dns-core/dns-history?name=available.split.test.&limit=20")
            assert history.get("queries"), "actual DNS queries were not retained"
            page = run(inside("client", ["curl", "--noproxy", "*", "--fail", "--max-time", "4", "-sS", "http://"+address+":8080/dns-queries?service=service.dns-core&name=available.split.test.&limit=20"]))
            assert 'data-query="' in page and any(answer in page for answer in ("192.0.2.77", "192.0.2.78")), "Web console omitted the real DNS answer"
            denied = subprocess.run(inside("client", ["curl", "--interface", "client0", "--noproxy", "*", "--max-time", "2", "-sS", "http://"+address+":8080/"]), capture_output=True)
            assert denied.returncode != 0, "management Web was reachable from the LAN"
            print("PASS HA management SSH, retained host keys, loopback/management DNS, Web query history and LAN access restriction", flush=True)
        check_management()
        def check_operations():
            node = elected()
            since = time.time_ns()
            def fresh(stamp):
                return datetime.fromisoformat(stamp.replace("Z", "+00:00")).timestamp() >= since/1e9
            run(inside("client", ["curl", "-4", "--noproxy", "*", "--fail", "--max-time", "8", "--resolve", "one.one.one.one:443:1.1.1.1", "-sS", "-I", "https://one.one.one.one/"]))
            assert run(inside("client", ["dig", "@1.1.1.1", "example.net", "A", "+short", "+time=3", "+tries=1"])).strip()
            def classified():
                rows = api(node, "control.sock", "/v1/services/service.flows/traffic-history?client="+client_address+"&peer=1.1.1.1&limit=100")["flows"] or []
                rows = [r for r in rows if fresh(r["tsStarted"])]
                return any(r.get("engine") == "ndpi" and r.get("source") == "libndpi" and r.get("accounting") and r.get("bytesOut", 0) > 0 and r.get("bytesIn", 0) > 0 for r in rows) and any(r.get("tlsSNI") == "one.one.one.one" for r in rows)
            wait(classified)
            management = "198.19.0.2" if node == "a" else "198.19.0.3"
            denied = subprocess.run(inside("client", ["curl", "--interface", "client0", "--noproxy", "*", "--max-time", "2", "-sS", "http://"+management+":8080/"]), capture_output=True)
            assert denied.returncode != 0
            def recorded_drop():
                rows = api(node, "control.sock", "/v1/services/service.packets/firewall-history?action=drop&limit=100")["entries"] or []
                return any(fresh(r["ts"]) and r.get("srcAddress") == client_address and r.get("dstPort") == 8080 and r.get("inIface") in ("lan0", "vip0") for r in rows)
            wait(recorded_drop)
            source = "192.0.2.10" if node == "a" else "192.0.2.11"
            def received_syslog():
                for line in (stage / "syslog.jsonl").read_text().splitlines(keepends=True):
                    if not line.endswith("\n"):
                        continue
                    row = json.loads(line)
                    message = row.get("message", "")
                    stamp = re.search(r"timestamp=(\S+)", message)
                    if row.get("peer", [None])[0] == source and "health probe completed" in message and stamp and fresh(stamp[1]):
                        return True
                return False
            wait(received_syslog)
            label = "homert02" if node == "a" else "homert03"
            def ours(resource):
                return any(a["key"] == "routerd.node" and a["value"].get("stringValue") == label for a in resource["resource"].get("attributes", []))
            def received_otlp():
                path = stage / "otlp.jsonl"
                if not path.exists():
                    return False
                batches = [json.loads(line) for line in path.read_text().splitlines(keepends=True) if line.endswith("\n")]
                logs = [r for b in batches for resource in b.get("resourceLogs", []) if ours(resource) for scope in resource.get("scopeLogs", []) for r in scope.get("logRecords", [])]
                metrics = [m for b in batches for resource in b.get("resourceMetrics", []) if ours(resource) for scope in resource.get("scopeMetrics", []) for m in scope.get("metrics", [])]
                spans = [s for b in batches for resource in b.get("resourceSpans", []) if ours(resource) for scope in resource.get("scopeSpans", []) for s in scope.get("spans", [])]
                return (any(int(r.get("timeUnixNano", 0)) >= since and r.get("body", {}).get("stringValue") == "health probe completed" for r in logs)
                    and any(m.get("name") == "routerd.healthcheck.probes" and any(int(p.get("timeUnixNano", 0)) >= since for p in m.get("sum", {}).get("dataPoints", [])) for m in metrics)
                    and any(s.get("name") == "healthcheck.probe" and int(s.get("startTimeUnixNano", 0)) >= since for s in spans))
            wait(received_otlp)
            print("PASS HA fresh native nDPI counters/TLS SNI, firewall history, syslog and OTLP logs/metrics/traces from", label, flush=True)
        check_operations()
        special_clients = []
        for index, mac in enumerate(("02:77:00:00:00:04", "02:77:01:00:00:05", "02:77:01:00:00:06")):
            key = "special"+str(index)
            names[key] = prefix+"-"+key
            run(["ip", "netns", "add", names[key]])
            namespaces.append(names[key])
            run(inside(key, ["ip", "link", "set", "lo", "up"]))
            run(inside("client", ["ip", "link", "add", "link", "client0", "name", "special0", "netns", names[key], "type", "macvlan", "mode", "bridge"]))
            run(inside(key, ["ip", "link", "set", "special0", "address", mac, "up"]))
            lease_file = stage / (key+".lease")
            hook = stage / (key+".sh")
            hook.write_text('#!/bin/sh\ncase "$1" in deconfig) ip -4 address flush dev "$interface"; ip -4 route flush dev "$interface";; bound|renew) ip address replace "$ip/$subnet" dev "$interface"; ip route replace default via "$router" dev "$interface"; printf "%s %s %s\\n" "$ip" "$router" "$dns" > "'+str(lease_file)+'";; esac\n')
            hook.chmod(0o500)
            process = start(key, ["busybox", "udhcpc", "-f", "-i", "special0", "-s", str(hook), "-p", str(stage / (key+".pid")), "-x", "hostname:"+key], key)
            wait(lease_file.exists)
            special_clients.append((key, process, lease_file, lease_file.read_text().split()[0], mac))
        def check_special_clients(fingerprints=False):
            for key, process, lease_file, original, mac in special_clients:
                assert process.poll() is None, "independent DHCP client exited"
                address, gateway, dns = lease_file.read_text().split()
                assert address == original and gateway == dns == "198.18.0.253", (key, address, gateway, dns)
                assert ipaddress.IPv4Address("198.18.0.32") <= ipaddress.IPv4Address(address) <= ipaddress.IPv4Address("198.18.0.63"), address
                assert run(inside(key, ["dig", "@"+dns, "example.net", "A", "+short", "+time=3", "+tries=1"])).strip()
                run(inside(key, ["curl", "-4", "--noproxy", "*", "--fail", "--max-time", "8", "--resolve", "one.one.one.one:443:1.1.1.1", "-sS", "-I", "-o", "/dev/null", "https://one.one.one.one/"]))
                if fingerprints:
                    wait(lambda: bool(api(elected(), "control.sock", "/v1/services/service.lan-leases/dhcp-fingerprints?mac="+mac+"&limit=10").get("entries")))
            print("PASS HA OUI and exact-MAC scopes retained their real leases and used the independent gateway/DNS", flush=True)
        check_special_clients(fingerprints=True)
        neighbor = json.loads(run(inside("client", ["ip", "-j", "neigh", "show", "to", "198.18.0.1", "dev", "client0"])))
        assert len(neighbor) == 1 and neighbor[0].get("lladdr") == "00:00:5e:00:01:2a", neighbor
        print("PASS client learned the IPv4 VIP MAC rather than its IPv6 sibling", flush=True)
        def primary_upstream_selected():
            firewall = api(elected(), "control.sock", "/v1/network").get("desired", {}).get("firewall", {})
            flows = firewall.get("flow_routes", [])
            return bool(flows) and all(flow.get("buckets") and set(flow["buckets"]) <= {272, 273, 274} for flow in flows)
        wait(primary_upstream_selected)
        flow_processes = {}
        capture = start("client", ["tcpdump", "-n", "-Z", "root", "-U", "-i", "client0", "-c", "256", "-w", str(stage / "flow.pcap"), "host 1.1.1.1 and tcp and tcp[tcpflags] & (tcp-syn | tcp-fin | tcp-rst) != 0"], "flow-capture")
        for mode in ("tcp", "udp"):
            flow_processes[mode] = start("client", ["python3", "-I", str(stage / "internet-flow.py"), mode, "--progress", str(stage / (mode + ".json"))], mode + "-flow")
        def progress(mode):
            if flow_processes[mode].poll() is not None:
                raise RuntimeError((stage / (mode + "-flow.log")).read_text())
            path = stage / (mode + ".json")
            return json.loads(path.read_text()) if path.exists() else {}
        wait(lambda: all(progress(mode).get("received", 0) >= 2 for mode in flow_processes))
        initial_flows = {mode: progress(mode) for mode in flow_processes}
        def nat_mapping(node, mode):
            address, port = initial_flows[mode]["endpoint"]
            output = run(inside(node, ["conntrack", "-L", "-p", mode, "--orig-src", address, "--orig-dst", "1.1.1.1", "--sport", str(port), "--dport", "443" if mode == "tcp" else "53"]))
            tuples = re.findall(r"src=(\S+) dst=(\S+) sport=(\d+) dport=(\d+)", output)
            mark = re.search(r"\bmark=(\d+)", output)
            assert len(tuples) == 2 and mark, output
            return {"reply": tuples[1], "mark": int(mark[1])}
        mappings = {mode: nat_mapping(elected(), mode) for mode in flow_processes}
        assert all(mapping["mark"] in {272, 273, 274} for mapping in mappings.values()), mappings
        def pd_identity(node):
            s = api(node, "control.sock", "/v1/services/service.wan-pd")["snapshot"]
            return s["client_duid"], s["iaid"], {ipaddress.ip_network(p["prefix"]) for p in s.get("prefixes", [])}
        print("Initial public NAT tuples", mappings, flush=True)
        original_pd = pd_identity(elected())
        original_address = lease.read_text().split()[0]
        def standby_synced(node):
            if daemons[node].poll() is not None:
                raise RuntimeError("rejoining product exited: " + node)
            try:
                pd = api(node, "control.sock", "/v1/services/service.wan-pd").get("snapshot", {})
                dhcp = api(node, "control.sock", "/v1/services/service.lan-leases").get("snapshot", {})
            except RuntimeError:
                return False
            sync = pd.get("sync", {})
            leases = stage / node / "state/services/lan-leases/dnsmasq.leases"
            wanted = {original_address, *(c[3] for c in special_clients)}
            mirrored = leases.exists() and wanted <= {line.split()[2] for line in leases.read_text().splitlines() if len(line.split()) >= 3}
            return pd.get("state") == "standby" and sync.get("epoch", 0) > 0 and not sync.get("error") and dhcp.get("standby") and mirrored
        for _ in range(2):
            previous = elected()
            survivor = "b" if previous == "a" else "a"
            wait(lambda: standby_synced(survivor))
            handoff_started = time.monotonic()
            daemons[previous].send_signal(signal.SIGTERM)
            daemons[previous].wait(timeout=30)
            daemons.pop(previous)
            wait(lambda: elected() == survivor)
            failed_probes = 0
            def recovered():
                nonlocal failed_probes
                try:
                    traffic()
                    check_tailnet()
                    check_management()
                    return True
                except (RuntimeError, AssertionError) as error:
                    failed_probes += 1
                    print("HA recovery probe failed", round(time.monotonic()-handoff_started, 2), str(error), flush=True)
                    return False
            wait(recovered)
            check_operations()
            counts = {mode: progress(mode)["received"] for mode in flow_processes}
            wait(lambda: all(progress(mode)["received"] > counts[mode] for mode in flow_processes))
            for mode in flow_processes:
                assert progress(mode)["endpoint"] == initial_flows[mode]["endpoint"]
                assert nat_mapping(survivor, mode) == mappings[mode]
            assert pd_identity(survivor) == original_pd, "takeover changed the shared DUID/IAID/prefix"
            renewed_before = lease.stat().st_mtime_ns
            dhcp_client.send_signal(signal.SIGUSR1)
            wait(lambda: lease.stat().st_mtime_ns > renewed_before)
            assert lease.read_text().split()[0] == original_address
            for _, process, lease_file, _, _ in special_clients:
                before = lease_file.stat().st_mtime_ns
                process.send_signal(signal.SIGUSR1)
                wait(lambda: lease_file.stat().st_mtime_ns > before)
            check_special_clients()
            # dnsmasq supplies fingerprint metadata on acquisition, not renewal.
            # Exercise a real release/reacquisition on the new owner as well.
            for key, process, lease_file, _, _ in special_clients:
                process.send_signal(signal.SIGUSR2)
                wait(lambda: not any(link.get("addr_info") for link in json.loads(run(inside(key, ["ip", "-j", "-4", "address", "show", "dev", "special0"])))))
                before = lease_file.stat().st_mtime_ns
                process.send_signal(signal.SIGUSR1)
                wait(lambda: lease_file.stat().st_mtime_ns > before)
            check_special_clients(fingerprints=True)
            print("PASS HA special-client Release/reacquisition retained sticky addresses and recorded fingerprints on the new owner", flush=True)
            print("PASS full native HA handoff", previous, "->", survivor,
                  "elapsed", round(time.monotonic()-handoff_started, 2), "failed probes", failed_probes,
                  "same TCP/UDP NAT tuples, PD identity and real DHCP renewal", flush=True)
            daemons[previous] = start(previous, [str(stage / "routerd"), "boot", "--file", str(stage / previous / "boot.json")], "router-" + previous)
            wait(lambda: standby_synced(previous))
            assert elected() == survivor, "rejoining member preempted the configured non-preemptive owner"
            check_tailnet()
            check_management()
            print("PASS rejoined member synchronized without preemption", previous, flush=True)
        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=5)
        tcp_port = initial_flows["tcp"]["endpoint"][1]
        packets = run(["tcpdump", "-nn", "-r", str(stage / "flow.pcap"), f"src port {tcp_port} and dst port 443 and tcp[tcpflags] & tcp-syn != 0"])
        assert len(packets.splitlines()) == 1, packets
        print("PASS one TCP SYN through both handoffs", {mode: progress(mode) for mode in flow_processes}, flush=True)
        node = elected()
        address = next(a for a in tailnet_snapshot(node)["addresses"] if ipaddress.ip_address(a).version == 4)
        run(inside("tail", peer_cli + ["set", "--exit-node="+address, "--exit-node-allow-lan-access=true"]))
        for family, server in (("-4", "1.1.1.1"), ("-6", "[2606:4700:4700::1111]")):
            code = run(inside("tail", ["curl", family, "--noproxy", "*", "--max-time", "8", "--resolve", "one.one.one.one:443:"+server, "-sS", "-I", "-o", "/dev/null", "-w", "%{http_code}", "https://one.one.one.one/"]))
            assert code in ("200", "301", "302", "308"), (family, code)
            assert run(inside("tail", ["dig", family, "@"+server.strip("[]"), "example.net", "A", "+short", "+time=3", "+tries=1"])).strip()
        print("PASS independent tailnet client used the elected node for IPv4/IPv6 exit HTTPS and UDP DNS", flush=True)
    except BaseException:
        for name in ("gateway-dhcp", "gateway-wire", "dns-53", "special0", "special1", "special2", "tail-flow-a", "tail-flow-b", "tail-flow-peer", "tail-peer"):
            path = stage / (name+".log")
            if path.exists():
                print("HA peer failure", name, path.read_text(errors="replace")[-10000:], flush=True)
        packets = stage / "flow.pcap"
        if packets.exists():
            print("HA flow flags", run(["tcpdump", "-nn", "-tt", "-r", str(packets)]), flush=True)
        for mode in ("tcp", "udp"):
            flow = stage / (mode + ".json")
            if not flow.exists():
                continue
            details = json.loads(flow.read_text())
            print("HA flow progress", mode, details, flush=True)
            for node in ("a", "b"):
                if names[node] not in namespaces:
                    continue
                address, port = details["endpoint"]
                result = subprocess.run(inside(node, ["conntrack", "-L", "-p", mode, "--orig-src", address, "--sport", str(port)]), capture_output=True, text=True)
                print("HA flow NAT", node, mode, result.stdout, flush=True)
        capture = stage / "client-ra.log"
        if capture.exists():
            print("HA received RA", capture.read_text()[-12000:], flush=True)
        if names["client"] in namespaces:
            for command in (["ip", "-j", "address"], ["ip", "-6", "route"], ["ip", "-6", "neigh"]):
                print("HA client failure", command, run(inside("client", command)), flush=True)
        for node in ("a", "b"):
            for socket,path in (("control.sock","/v1/services"),("control.sock","/v1/services/service.dns"),("control.sock","/v1/services/service.lan-ha"),("control.sock","/v1/network")):
                if (stage / node / "run" / socket).exists():
                    try:
                        print("HA failure",node,path,json.dumps(api(node,socket,path)),flush=True)
                    except Exception as error:
                        print("HA inspection",node,error,flush=True)
            for path in (stage / ("router-"+node+".log"),*(stage / node / "state/logs").glob("*.ndjson")):
                if path.is_file():
                    for line in path.read_text(errors="replace").splitlines()[-25:]:
                        if '"ERROR"' in line or '"WARN"' in line:
                            print(node,line,flush=True)
        raise
    finally:
        for p in reversed(children):
            if p.poll() is None:
                p.terminate()
        for p in reversed(children):
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                p.kill();p.wait(timeout=5)
        for namespace in reversed(namespaces):
            for pid in run(["ip", "netns", "pids", namespace]).split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            subprocess.run(["ip","netns","del",namespace],capture_output=True)
        print("Full native HA namespaces removed",flush=True)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("--uplink",required=True)
    p.add_argument("--inside",type=Path)
    a=p.parse_args()
    if a.inside:
        owner(a.inside,a.uplink);return
    root=[] if os.geteuid()==0 else ["sudo","-n"]
    stage=Path(run(root+["mktemp","-d","/tmp/routerd-full-ha.XXXXXX"]).strip())
    try:
        files=[(Path(__file__),"owner.py"),(Path("scripts/syslog-peer.py"),"syslog-peer.py"),(Path("scripts/internet-flow.py"),"internet-flow.py"),(Path("scripts/tailscale-peer.py"),"peer-fixture.py")]
        files += [(Path("dist/bin")/n,n) for n in ("routerd","routerd-modules","routerd-logging")]
        files += [(Path("dist")/n,n) for n in ("dnsmasq-engine.tar","conntrack-engine.tar","chrony-engine.tar","tailscale-engine.tar")]
        files += [(Path("dist/peer-env/tailscale")/n,n) for n in ("headscale","tailscale","tailscaled")]
        files += [(Path("examples/homert0"+str(i+2)+"-native.yaml"),"node-"+node+".yaml") for i,node in enumerate(("a","b"))]
        for source,name in files:
            run(root+["install","-m","500",str(source.resolve()),str(stage/name)])
        print("Full native HA stage",stage,flush=True)
        subprocess.run(root+["unshare","--mount","--propagation","private","python3","-I",str(stage/"owner.py"),"--inside",str(stage),"--uplink",a.uplink],check=True,timeout=640)
    finally:
        run(root+["python3","-I","-c","import shutil,sys;shutil.rmtree(sys.argv[1])",str(stage)])


if __name__=="__main__":
    main()
