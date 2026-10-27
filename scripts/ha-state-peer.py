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


def run(command):
    return subprocess.run(command, check=True, capture_output=True, text=True, timeout=10).stdout


def peer(stage, dhcp6_server):
    def deadline(*_):
        raise TimeoutError("HA connection-state communication deadline")
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(230 if dhcp6_server else 160)
    # Keep iproute2's named-namespace files off the host filesystem too.
    Path("/run/netns").mkdir(parents=True, exist_ok=True)
    run(["mount", "-t", "tmpfs", "-o", "mode=700", "tmpfs", "/run/netns"])
    namespaces = ["ha-a", "ha-b", "ha-client", "ha-server"]
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

    def active(node):
        output = run(inside("ha-" + node, ["ip", "-j", "address", "show"])) if (stage / node / "run" / "supervisor.sock").exists() else "[]"
        return any(address.get("local") == "203.0.113.1" for link in json.loads(output) for address in link.get("addr_info", []))

    try:
        for ns in namespaces:
            run(["ip", "netns", "add", ns])
            run(inside(ns, ["ip", "link", "set", "lo", "up"]))
        for bridge in ("lan", "wan", "sync"):
            run(["ip", "link", "add", bridge, "type", "bridge"])
            run(["ip", "link", "set", bridge, "up"])
        for index, ns in enumerate(namespaces):
            segments = ("lan", "wan", "sync") if index < 2 else (("lan",) if index == 2 else ("wan",))
            for segment in segments:
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
        roles = ("logging", "network", "control", "management", "vrrp", "dhcp_server") + (("dhcp6", "dhcp6_server", "ra") if dhcp6_server else ())
        layout = {"boot_abi": 1, "worker_abi": 5, "entrypoint": "routerd",
                  "roles": [{"role": role, "artifact": "routerd-logging" if role == "logging" else "routerd-modules"} for role in roles],
                  "engines": [{"role": "vrrp", "artifact": "ct-image", "sources": "ct-sources"}],
                  "artifacts": [{"name": name, "kind": "native", "command": name, "file": str(stage / name)} for name in ("routerd", "routerd-modules", "routerd-logging")] +
                               [{"name": "ct-image", "kind": "data", "file": str(stage / "conntrack-engine.tar")}, {"name": "ct-sources", "kind": "data", "file": str(stage / "conntrack-sources.tar")}]}
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
                    "vip_interface": "lan-ha", "virtual_router_id": 42, "priority": 150 - index * 50, "preempt": False, "advert_interval_centiseconds": 20,
                    "addresses": ["198.18.0.1/24"], "accept_mode": True,
                    "conntrack_sync": {"interface": "sync0", "local_address": "169.254.42." + str(index + 2), "peer_address": "169.254.42." + str(3 - index)}
                }}},
                {"id": "leases", "service": {"name": "leases", "dhcp_server": {
                    "interface": "lan-ha", "server_address": "198.18.0.1", "authoritative": True, "sticky_hold_days": 3,
                    "primary": {"prefix": "198.18.0.0/24", "start": "198.18.0.100", "end": "198.18.0.101", "lease_seconds": 60, "router": "198.18.0.1"},
                    "ha": {"owner": "ha", "replication": {"listen_address": "169.254.42." + str(index + 2) + ":8443", "peer_address": "169.254.42." + str(3 - index) + ":8443", "certificate_file": str(stage / (node + ".crt")), "private_key_file": str(stage / (node + ".key")), "peer_certificate_file": str(stage / (("b" if node == "a" else "a") + ".crt"))}}
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
                pd_peer = {"listen_address": "169.254.42." + str(index + 2) + ":8444", "peer_address": "169.254.42." + str(3 - index) + ":8444", "certificate_file": str(stage / (node + ".crt")), "private_key_file": str(stage / (node + ".key")), "peer_certificate_file": str(stage / (("b" if node == "a" else "a") + ".crt"))}
                resources += [
                    {"id": "wan-linklocal", "when": {"fact": "service.ha.elected"}, "address": {"interface": "wan-ha", "cidr": "fe80::1/64"}},
                    {"id": "wan6-route", "when": {"fact": "service.ha.elected"}, "route": {"destination": "2001:db8:ffff::/64", "interface": "wan-ha", "gateway": "fe80::2"}},
                    {"id": "pd", "service": {"name": "pd", "dhcp6": {"interface": "wan-ha", "request_prefix": True, "prefix_length": 60, "client_duid": "0003000102005e000143", "iaid": 7,
                        "ha": {"owner": "ha", "replication": pd_peer}, "delegations": [{"name": "lan", "interface": "lan0", "subnet_id": 1, "advertise": True, "other_config": True, "router_lifetime_seconds": 10}]}}}
                ]
                resources.append({"id": "lan-info", "when": {"fact": "service.ha.active"}, "service": {"name": "lan-info", "dhcp6_server": {
                    "interface": "lan0", "server_duid": "000400112233445546778899aabbccddeeff", "options_from": ["pd"], "domain_search": ["local.test"], "refresh_seconds": 600
                }}})
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
        run(inside("ha-server", ["ip", "address", "add", "203.0.113.50/24", "dev", "wan0"]))
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
            return api(node, "control.sock", "/v1/services/service.pd")["snapshot"]

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
            def received():
                reply = information_request()
                if reply.returncode:
                    return False
                options = dict(line.split("=", 1) for line in reply.stdout.splitlines() if line.startswith(("dns=", "search=")))
                return [str(ipaddress.IPv6Address(ip)) for ip in options.get("dns", "").split()] == ["2001:db8:ffff::20"] and set(options.get("search", "").split()) == {"wan.test", "local.test"}
            wait(received, timeout=6)
            assert modules(owner).get("service.lan-info", {}).get("ready"), "master information server absent"
            standby = "b" if owner == "a" else "a"
            assert "service.lan-info" not in modules(standby), "standby information server still active"
            for transport in ("+notcp", "+tcp"):
                assert "2001:db8:ffff::20" in run(inside("ha-client", ["dig", "-6", "@2001:db8:ffff::20", "host.wan.test", "AAAA", "+short", "+time=2", "+tries=1", transport]))

        for segment in ("lan", "wan", "sync"):
            log = (stage / (segment + "-capture.log")).open("w")
            process = subprocess.Popen(["tcpdump", "--immediate-mode", "-U", "-n", "-i", segment, "-w", str(stage / (segment + ".pcap"))], stdout=log, stderr=log)
            children.append(process)
            captures.append(process)
            log.close()
        start("ha-a", ["unshare", "--pid", "--fork", "--mount-proc", "--kill-child=SIGTERM", str(stage / "routerd"), "boot", "--file", str(stage / "a/boot.json")], "a")
        wait(lambda: (stage / "a/run/supervisor.sock").exists())
        wait(lambda: modules("a").get("service.ha", {}).get("ready") is True)
        wait(lambda: active("a"))
        start("ha-b", ["unshare"] + (["--time", "--boottime", "7200"] if dhcp6_server else []) + ["--pid", "--fork", "--mount-proc", "--kill-child=SIGTERM", str(stage / "routerd"), "boot", "--file", str(stage / "b/boot.json")], "b")
        wait(lambda: (stage / "b/run/supervisor.sock").exists())
        wait(lambda: modules("b").get("service.ha", {}).get("ready") is True)
        wait(lambda: modules("a").get("service.leases", {}).get("ready") is True and modules("b").get("service.leases", {}).get("ready") is True)
        if dhcp6_server:
            wait(lambda: modules("a").get("service.pd", {}).get("ready") and modules("b").get("service.pd", {}).get("ready"))
            pd_traffic("a")
            information("a")
            wait(lambda: pd_saved("a").get("revision") == pd_saved("b").get("revision") and pd_saved("a").get("snapshot", {}).get("prefixes"))
            original = pd_saved("b")
            before = modules("b")["service.pd"]["generation"]
            api("b", "control.sock", "/v1/services/service.pd/restart", {})
            wait(lambda: modules("b").get("service.pd", {}).get("generation", 0) > before and modules("b").get("service.pd", {}).get("ready"))
            assert pd_saved("b") == original, "standby restart rebased PD or lost the source revision"
            print("PASS ISC PD, independent SLAAC TCP/UDP, authenticated standby checkpoint with BOOTTIME +7200s and restart with original mapping", flush=True)
        hook = stage / "dhcp-hook.sh"
        hook.write_text('#!/bin/sh\ncase "$1" in\n bound|renew) ip address replace "$ip/24" dev "$interface"; printf "%s %s\\n" "$1" "$ip" >> "' + str(stage) + '/$interface.progress";;\nesac\n')
        hook.chmod(0o500)

        def lease_client(number, requested=None):
            name = "lease" + str(number)
            port = "dhcp" + str(number)
            run(["ip", "link", "add", port, "type", "veth", "peer", "name", name])
            run(["ip", "link", "set", port, "master", "lan"])
            run(["ip", "link", "set", port, "up"])
            run(["ip", "link", "set", name, "netns", "ha-client"])
            run(inside("ha-client", ["ip", "link", "set", name, "up"]))
            command = ["busybox", "udhcpc", "-f", "-i", name, "-s", str(hook), "-p", str(stage / (name + ".pid")), "-t", "3", "-T", "1", "-A", "1"]
            if requested:
                command += ["-r", requested]
            process = start("ha-client", command, name)
            wait(lambda: (stage / (name + ".progress")).exists())
            return process

        def lease_events(number):
            return (stage / ("lease" + str(number) + ".progress")).read_text().splitlines()

        def saved(node):
            return json.loads((stage / node / "state/services/leases/dhcp-leases.json").read_text())

        headers = stage / "sync-headers"
        response = stage / "sync-response"

        def replica_http(method, body=None, cert="b", domain=None):
            command = ["curl", "--max-time", "3", "--noproxy", "*", "-sS", "--cert", str(stage / (cert + ".crt")), "--key", str(stage / (cert + ".key")), "--cacert", str(stage / "a.crt"), "-X", method, "-D", str(headers), "-o", str(response), "-w", "%{http_code}", "https://169.254.42.2:8443/v1/leases"]
            if domain:
                command += ["-H", "X-Routerd-DHCP-Domain: " + domain]
            if body is not None:
                request = stage / "sync-request"
                request.write_text(json.dumps(body))
                command += ["--data-binary", "@" + str(request)]
            return subprocess.run(inside("ha-b", command), capture_output=True, text=True, timeout=5)

        first_client = lease_client(0)
        first_address = lease_events(0)[-1].split()[1]
        assert first_address in ("198.18.0.100", "198.18.0.101")
        assert saved("a")["leases"] == saved("b")["leases"], "wire ACK preceded standby persistence"
        old_replica = saved("b")
        print("PASS independent BusyBox DORA; both durable journals contain the acknowledged lease", flush=True)
        client_code = r'''
import socket,time,json
s=socket.create_connection(('203.0.113.50',18080),timeout=5);f=s.makefile('rwb',buffering=0)
u=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);u.bind(('198.18.0.50',30001));u.connect(('203.0.113.50',18081))
sequence=0;udp_sequence=0;udp_peer='';lost=0
while True:
 sequence+=1;data=str(sequence).encode();prefix=data+b' '
 f.write(data+b'\n');tcp=f.readline().strip()
 if not tcp.startswith(prefix):raise RuntimeError('TCP stream closed or changed')
 u.send(data);end=time.monotonic()+0.2
 while True:
  remaining=end-time.monotonic()
  if remaining<=0:lost+=1;break
  u.settimeout(remaining)
  try:reply=u.recv(2048)
  except socket.timeout:lost+=1;break
  if reply.startswith(prefix):udp_peer=reply[len(prefix):].decode();udp_sequence=sequence;break
 print(json.dumps({'sequence':sequence,'udp_sequence':udp_sequence,'tcp_peer':tcp[len(prefix):].decode(),'udp_peer':udp_peer,'udp_lost':lost}),flush=True)
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
        if dhcp6_server:
            information("a")
            local_pid = modules("a")["control"]["pid"]
            candidates = []
            for value in run(["ip", "netns", "pids", "ha-a"]).split():
                fields = Path("/proc/" + value + "/status").read_text().splitlines()
                if any(line.startswith("NSpid:") and int(line.split()[-1]) == local_pid for line in fields):
                    candidates.append(int(value))
            assert len(candidates) == 1, "control process mapping is not unique"
            controller = candidates[0]
            addresses = json.loads(run(inside("ha-a", ["ip", "-j", "-6", "address", "show", "dev", "lan0"])))
            target = next(a["local"] for link in addresses for a in link["addr_info"] if a["scope"] == "link" and not a.get("tentative"))
            direct_information(target, True)
            os.kill(controller, signal.SIGSTOP)
            try:
                time.sleep(2)
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
            print("PASS original conditional serving authority expired on the stopped-control node; peer serving and the same TCP/UDP mapping recovered", flush=True)
        print("PASS continuous actual VRRP/NAT TCP and UDP; tcp_loose=0", flush=True)
        for previous, owner in (("a", "b"), ("b", "a")):
            partial = None
            if previous == "b":
                fetched = replica_http("GET")
                assert fetched.returncode == 0 and fetched.stdout == "200"
                partial_domain = next(line.partition(":")[2].strip() for line in headers.read_text().splitlines() if line.lower().startswith("x-routerd-dhcp-domain:"))
                partial_state = json.loads(response.read_text())
                partial_state["revision"] += 1000
                upload = stage / "partial-replica.json"
                upload.write_text(json.dumps(partial_state))
                diagnostic = (stage / "partial-replica.log").open("w")
                partial = subprocess.Popen(inside("ha-b", ["curl", "--max-time", "8", "--noproxy", "*", "-sS", "--cert", str(stage / "b.crt"), "--key", str(stage / "b.key"), "--cacert", str(stage / "a.crt"), "-X", "PUT", "-H", "X-Routerd-DHCP-Domain: " + partial_domain, "--limit-rate", "1", "--data-binary", "@" + str(upload), "https://169.254.42.2:8443/v1/leases"]), stdout=diagnostic, stderr=diagnostic)
                diagnostic.close()
                children.append(partial)
                time.sleep(0.2)
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
            wait(lambda: modules(owner).get("service.leases", {}).get("ready") is True and api(owner, "control.sock", "/v1/services/service.leases")["snapshot"]["active"] is True)
            before_renew = len(lease_events(0))
            first_client.send_signal(signal.SIGUSR1)
            wait(lambda: len(lease_events(0)) > before_renew)
            assert lease_events(0)[-1].split()[1] == first_address, "takeover changed the client's lease"
            assert saved("a")["leases"] == saved("b")["leases"], "renew ACK preceded standby persistence"
            if partial is not None:
                assert partial.wait(timeout=9) != 0, "incomplete old-worker upload was accepted"
                assert saved("a")["revision"] < partial_state["revision"], "partial snapshot changed allocator state at takeover"
                print("PASS takeover during a real incomplete TLS snapshot upload left the journal unchanged", flush=True)
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
        assert saved("a")["leases"] == saved("b")["leases"]
        print("PASS second independent client requested the first address and received a different free address", flush=True)
        old_generation = modules("a")["service.leases"]["generation"]
        standby_before = saved("a")
        api("a", "control.sock", "/v1/services/service.leases/restart", {})
        wait(lambda: modules("a").get("service.leases", {}).get("ready") and modules("a")["service.leases"]["generation"] > old_generation)
        assert saved("a") == standby_before, "standby restart changed original lease deadlines or journal revision"
        before_renew = len(lease_events(0))
        first_client.send_signal(signal.SIGUSR1)
        wait(lambda: len(lease_events(0)) > before_renew)
        assert lease_events(0)[-1].split()[1] == first_address
        print("PASS standby DHCP worker restart retained the journal and accepted the next real renewal", flush=True)
        fetched = replica_http("GET")
        assert fetched.returncode == 0 and fetched.stdout == "200", fetched.stderr
        domain = next(line.partition(":")[2].strip() for line in headers.read_text().splitlines() if line.lower().startswith("x-routerd-dhcp-domain:"))
        current_replica = json.loads(response.read_text())
        assert replica_http("PUT", current_replica, domain=domain).stdout == "204", "identical durable retransmission was not acknowledged"
        assert replica_http("PUT", old_replica, domain=domain).stdout == "409", "old lease revision was accepted"
        current_replica["leases"][0]["hostname"] = "conflicting-host"
        assert replica_http("PUT", current_replica, domain=domain).stdout == "409", "conflicting equal revision was accepted"
        run(["openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-keyout", str(stage / "wrong.key"), "-out", str(stage / "wrong.crt"), "-days", "1", "-subj", "/CN=wrong-peer"])
        refused = replica_http("GET", cert="wrong")
        assert refused.returncode != 0 and refused.stdout == "000", "unconfigured certificate reached the replication API"
        print("PASS mutual TLS peer rejection, identical retry, stale revision and conflicting equal revision", flush=True)
        before_renew = len(lease_events(0))
        sync_down = time.time()
        # Break the external bridge port. The product correctly repairs its
        # own declared interface when the fixture only changes that intent.
        run(["ip", "link", "set", "sync0", "down"])
        first_client.send_signal(signal.SIGUSR1)
        time.sleep(4)
        assert "UP" not in json.loads(run(["ip", "-j", "link", "show", "sync0"]))[0]["flags"], "sync outage was not maintained"
        assert len(lease_events(0)) == before_renew, "client received a renewal ACK without standby persistence"
        if dhcp6_server:
            wait(lambda: bool(pd_observed("a").get("sync", {}).get("error")))
            pd_traffic("b")
        sync_up = time.time()
        run(["ip", "link", "set", "sync0", "up"])
        wait(lambda: modules("b").get("service.leases", {}).get("ready") is True)
        first_client.send_signal(signal.SIGUSR1)
        wait(lambda: len(lease_events(0)) > before_renew)
        assert lease_events(0)[-1].split()[1] == first_address
        print("PASS sync-link loss withheld renewal ACK; restoration recovered the existing client lease", flush=True)
        if dhcp6_server:
            wait(lambda: pd_observed("a").get("sync") and not pd_observed("a")["sync"].get("error"))
            print("PASS PD sync outage reported on standby without interrupting the owner's IPv6 forwarding; recovery refreshed synchronization", flush=True)
        for process in (first_client, second_client):
            process.terminate()
            process.wait(timeout=3)
        def expiry_checkpoint():
            current = saved("b")
            # Local persistence precedes the peer write. Observe the worker's
            # completed checkpoint, rather than its provisional local file.
            snapshot = api("b", "control.sock", "/v1/services/service.leases")["snapshot"]
            return snapshot["active"] and snapshot["checkpoint_revision"] == current["revision"] and all(lease.get("expired") is True for lease in current["leases"])

        wait(expiry_checkpoint, timeout=70)
        expired = saved("b")["leases"]
        assert saved("a")["leases"] == expired, "completed expiry checkpoint differs on the standby"
        assert len(expired) == 2 and all(lease["held_until"] > lease["expires_at"] for lease in expired)
        print("PASS real 60-second lease expiry replicated without rebasing sticky hold deadlines", flush=True)
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
            wait(lambda: not pd_observed("a").get("prefixes") and no_global("ha-a", "lan0") and no_global("ha-client", "lan0"), remaining + 8)
            before = modules("a")["service.pd"]["generation"]
            api("a", "control.sock", "/v1/services/service.pd/restart", {})
            wait(lambda: modules("a").get("service.pd", {}).get("generation", 0) > before and modules("a").get("service.pd", {}).get("ready"))
            assert not pd_observed("a").get("prefixes") and no_global("ha-a", "lan0") and no_global("ha-client", "lan0"), "expired replicated PD revived on restart"
            pd_server = pd_start()
            pd_traffic("a")
            print("PASS server-absent cross-clock PD takeover expired at original deadline on router/SLAAC client; restart did not revive PD and ISC recovery restored TCP/UDP", flush=True)
        print("Observed UDP datagrams lost during transitions:", latest()["udp_lost"], flush=True)
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
        assert "service.ha" not in modules("a"), "retired sync worker remains registered"
        print("PASS configuration removal reaped sync worker and restored original conntrack sysctl", flush=True)
        for process in captures:
            process.send_signal(signal.SIGINT)
            process.wait(timeout=3)
        syn = run(["tcpdump", "-nn", "-r", str(stage / "wan.pcap"), "src host 203.0.113.1 and dst port 18080 and tcp[tcpflags] & tcp-syn != 0"])
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
        assert requests > 0 and acknowledgments == 0, "capture did not prove requested-but-unacknowledged renewal during the sync outage"
        print("PASS captures: one TCP SYN, IPv4 link-local FTFW replication, unchanged UDP NAT tuple", flush=True)
        print("PASS two-way state handoff without TCP reconnect", flush=True)
        if dhcp6_server:
            packets = run(["tcpdump", "-n", "-tt", "-vv", "-r", str(stage / "wan.pcap"), "ip6"])
            assert "dhcp6 rebind" in packets and "dhcp6 release" not in packets
            print("PASS capture: recovery Rebind and no shared-lease Release on member demotion/restart", flush=True)
    finally:
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
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    args = parser.parse_args()
    if args.inside:
        peer(args.inside, args.dhcp6_server)
        return
    os.chdir(Path(__file__).resolve().parent.parent)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    for module in ("nf_conntrack", "nf_conntrack_netlink"):
        run(root + ["modprobe", module])
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-ha.XXXXXX"]).strip()
    try:
        for source, name in [(Path(__file__), "peer.py"), (Path("dist/conntrack-engine.tar"), "conntrack-engine.tar"), (Path("dist/conntrack-sources.tar"), "conntrack-sources.tar"), *[(args.binary_dir / name, name) for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle")]]:
            run(root + ["install", "-m", "500", str(source.resolve()), stage + "/" + name])
        peer_args = []
        if args.dhcp6_server:
            run(root + ["install", "-m", "500", str(args.dhcp6_server.resolve()), stage + "/dhcpd"])
            peer_args = ["--dhcp6-server", stage + "/dhcpd"]
        result = subprocess.run(root + ["unshare", "--mount", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", stage] + peer_args, timeout=250 if args.dhcp6_server else 180)
        if result.returncode:
            for name in ("a.log", "b.log", "client.log", "server.log", "dhcp6.log", "http6.log", "udp6.log"):
                log = subprocess.run(root + ["tail", "-30", stage + "/" + name], capture_output=True, text=True)
                print(name + ":\n" + log.stdout)
            raise RuntimeError("HA communication failed")
    finally:
        run(root + ["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
