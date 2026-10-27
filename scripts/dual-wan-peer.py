#!/usr/bin/env python3
"""Verify real PPPoE/DS-Lite Health/PBR failover with independent peers.

Requires Linux root/sudo, pppd with pppoe.so, RP-PPPoE pppoe-server,
iproute2, nft, tcpdump, curl and Python 3. Host PPP files and routes are isolated.
Packet capture includes both real WAN paths and the separate LAN client.
The independent AC negotiates MRU 1400 against product MTU 1454.
WAN providers route to separate MTU-1500 application hosts and suppress IPv4
fragmentation-needed messages. Check 1 MiB TCP in both directions and small MSS.
"""

import argparse
import copy
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import textwrap


def run(a):
    return subprocess.run(a, check=True, capture_output=True, text=True, timeout=8).stdout


def deadline(*_):
    raise TimeoutError("dual-WAN fixture deadline")


def terminated(*_):
    raise KeyboardInterrupt()


def peer(stage):
    signal.signal(signal.SIGALRM, deadline)
    signal.signal(signal.SIGTERM, terminated)
    signal.alarm(150)
    ns = "r2dual-" + str(os.getpid())
    ppp = ns + "p"
    dsl = ns + "d"
    lan = ns + "l"
    ppp_app = ns + "pa"
    dsl_app = ns + "da"
    children = []
    capture = None

    def inside(n, a):
        return ["ip", "netns", "exec", n] + a

    def start(n, a, name):
        with (stage / (name + ".log")).open("a") as output:
            p = subprocess.Popen(inside(n, a), stdout=output, stderr=subprocess.STDOUT)
        children.append(p)
        return p

    def wait(test, seconds=25):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if test():
                return
            time.sleep(0.1)
        raise RuntimeError("Independent dual-WAN communication timed out")

    def api(sock, path, body=None, status=200):
        a = [
            "curl",
            "--max-time",
            "3",
            "-sS",
            "--unix-socket",
            str(stage / "run" / sock),
            "http://localhost" + path,
            "-w",
            "\n%{http_code}",
        ]
        if body is not None:
            a += [
                "-X",
                "POST",
                "-H",
                "Content-Type: application/json",
                "--data",
                json.dumps(body, separators=(",", ":")),
            ]
        reply, code = run(a).rsplit("\n", 1)
        assert int(code) == status, (path, code, reply)
        return json.loads(reply)

    def modules():
        return {x["name"]: x for x in api("supervisor.sock", "/v1/status")["modules"]}

    def selected(priority, table):
        return any(
            r.get("priority") == priority and str(r.get("table")) == str(table)
            for r in json.loads(run(["ip", "-n", ns, "-j", "rule", "show"]))
        )

    def traffic(expected):
        text = run(
            inside(
                lan,
                ["curl", "--noproxy", "*", "--max-time", "2", "-sS", "http://198.51.100.20:18080/"],
            )
        )
        assert text == expected, (text, expected)
        code = (
            'import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.connect(("198.51.100.20",18081));s.send(b"app");assert s.recv(100)=='
            + repr(expected.encode())
        )
        run(inside(lan, ["python3", "-c", code]))

    def large_transfer(host, mtu):
        code = r"""import http.client,hashlib,socket,sys
host=sys.argv[2];c=http.client.HTTPConnection(host,18080,timeout=3)
c.connect();mss=c.sock.getsockopt(socket.IPPROTO_TCP,socket.TCP_MAXSEG);assert mss<=int(sys.argv[1])-(60 if ':' in host else 40),mss
c.request('GET','/large');r=c.getresponse();data=r.read();assert r.status==200 and data==b'x'*(1024*1024)
c.close()
c=http.client.HTTPConnection(host,18080,timeout=3)
body=b'y'*(1024*1024);c.request('POST','/',body);r=c.getresponse();assert r.status==200 and r.read().decode()==hashlib.sha256(body).hexdigest()
small=socket.socket(socket.AF_INET6 if ':' in host else socket.AF_INET,socket.SOCK_STREAM)
small.settimeout(3);small.setsockopt(socket.IPPROTO_TCP,socket.TCP_MAXSEG,1200);small.connect((host,18083))
small.sendall(b'GET / HTTP/1.0\r\nHost: peer\r\n\r\n');response=http.client.HTTPResponse(small);response.begin();assert response.status==200 and response.read() in (b'PPP',b'DSL')
assert small.getsockopt(socket.IPPROTO_TCP,socket.TCP_MAXSEG)<=1200
small.close()
print('PASS actual 1 MiB TCP download/upload',host,'TCP_MAXSEG',mss,'and smaller MSS; upstream ICMP fragmentation-needed blocked for IPv4',flush=True)
"""
        result = subprocess.run(
            inside(lan, ["python3", "-u", "-c", code, str(mtu), host]),
            capture_output=True,
            text=True,
            timeout=10,
        )
        print(result.stdout, result.stderr, flush=True)
        assert result.returncode == 0

    def udp_mtu(expected):
        code = r"""import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2)
s.setsockopt(socket.IPPROTO_IP,10,0)
s.connect(('198.51.100.20',18081));body=b'u'*1450;s.send(body);assert s.recv(4096)==sys.argv[1].encode()+b':'+body
s.close()
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.setsockopt(socket.IPPROTO_IP,10,3)
s.connect(('198.51.100.20',18081));s.send(body);assert s.recv(4096)==sys.argv[1].encode()+b':'+body
print('PASS reassembled 1450-byte UDP payload with DF clear and DF set',sys.argv[1],flush=True)
"""
        result = subprocess.run(
            inside(lan, ["python3", "-u", "-c", code, expected]),
            capture_output=True,
            text=True,
            timeout=8,
        )
        print(result.stdout, result.stderr, flush=True)
        assert result.returncode == 0

    def marked_udp():
        # The primary source-policy rule is gone, but mark41 selects PPP.
        # 1458 bytes exceed PPP1400 and fit DSL1460, so a FIB lookup that
        # ignores the packet mark cannot accidentally satisfy this exchange.
        code = r"""import socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.bind(('0.0.0.0',18084));s.setsockopt(socket.IPPROTO_IP,10,3)
s.connect(('198.51.100.20',18081));body=b'm'*1430;s.send(body);assert s.recv(4096)==b'PPP:'+body
"""
        run(inside(lan, ["python3", "-c", code]))
        print(
            "PASS marked1458-byte DF UDP selected PPP while source-policy selected DSL", flush=True
        )

    def probe(n, name):
        return start(
            n,
            [
                "python3",
                "-u",
                "-m",
                "http.server",
                "18082",
                "--bind",
                "198.51.100.10",
                "--directory",
                str(stage / "www"),
            ],
            name,
        )

    resources = [
        {"id": "wan0", "interface": {"name": "wan0"}},
        {"id": "wan1", "interface": {"name": "wan1"}},
        {"id": "lan", "interface": {"name": "lan0"}},
        {"id": "lan-ip", "address": {"interface": "lan0", "cidr": "10.42.0.1/24"}},
        {"id": "dsl-ip", "address": {"interface": "wan1", "cidr": "2001:db8:42::2/64"}},
        {"id": "forward", "forwarding": {"ipv4": True, "ipv6": True, "ipv4_rp_filter": 0}},
        {"id": "lan-v6", "address": {"interface": "lan0", "cidr": "2001:db8:43::1/64"}},
        {
            "id": "peer-v6",
            "route": {
                "destination": "2001:db8:45::1/128",
                "gateway": "2001:db8:42::1",
                "interface": "wan1",
            },
        },
        {
            "id": "firewall",
            "firewall": {
                "forward": [{"from": "lan0", "to": "dslite0"}],
                "forward_chain": {
                    "policy": "drop",
                    "rules": [{"match": {"in_interface": "lan0", "out_interface": "ppp-wan"}, "action": "accept"}],
                },
                "masquerade": [
                    {"source": "10.42.0.0/24", "out_interface": "ppp-wan"},
                    {"source": "10.42.0.0/24", "out_interface": "dslite0"},
                ],
            },
        },
        {
            "id": "internet-ppp",
            "service": {
                "name": "internet-ppp",
                "pppoe": {
                    "interface": "wan0",
                    "ppp_interface": "ppp-wan",
                    "username": "routerd2",
                    "password_file": str(stage / "password"),
                    "auth_method": "pap",
                    "mtu": 1454,
                    "mru": 1454,
                    "lcp_echo_interval": 1,
                    "lcp_echo_failure": 3,
                    "table": 101,
                },
            },
        },
        {
            "id": "internet-dsl",
            "service": {
                "name": "internet-dsl",
                "ds_lite": {
                    "interface": "wan1",
                    "tunnel": "dslite0",
                    "local_address": "2001:db8:42::2",
                    "candidates": ["2001:db8:42::1"],
                    "b4_peer": "192.0.0.1/32",
                    "route_gateway": "192.0.0.1",
                    "table": 102,
                },
            },
        },
    ]
    for name, iface, table, mark, prio in [
        ("ppp", "ppp-wan", 101, 41, 1000),
        ("dsl", "dslite0", 102, 42, 1100),
    ]:
        resources += [
            {
                "id": "probe-rule-" + name,
                "policy_rule": {"family": 4, "priority": table, "table": table, "mark": mark},
            },
            {
                "id": "health-" + name,
                "depends_on": ["internet-" + name, "probe-rule-" + name],
                "when": {"fact": "service.internet-" + name + ".active"},
                "service": {
                    "name": "health-" + name,
                    "health": {
                        "interface": iface,
                        "target": "198.51.100.10",
                        "protocol": "tcp",
                        "port": 18082,
                        "mark": mark,
                        "interval_ms": 200,
                        "timeout_ms": 200,
                        "healthy_threshold": 2,
                        "unhealthy_threshold": 1,
                    },
                },
            },
            {
                "id": "app-" + name,
                "when": {"fact": "service.health-" + name + ".active", "max_age": "1s"},
                "policy_rule": {
                    "family": 4,
                    "priority": prio,
                    "table": table,
                    "source": "10.42.0.0/24",
                },
            },
        ]
    firewall = next(r["firewall"] for r in resources if r["id"] == "firewall")
    firewall["forward"].append({"from": "lan0", "to": "wan1"})
    firewall["mss_clamp"] = [
        {"from": a, "to": b, "family": 6, "mtu": 1460}
        for a, b in [("lan0", "wan1"), ("wan1", "lan0")]
    ]
    firewall["ipv4_clear_df"] = [
        {"from": "lan0", "to": wan}
        for wan in ["ppp-wan", "dslite0"]
    ]
    configuration = {"schema_version": 2, "resources": resources}
    (stage / "config.json").write_text(json.dumps(configuration))
    (stage / "password").write_text("routerd2-fixture\n")
    (stage / "password").chmod(0o600)
    boot = dict(
        platform="host",
        config=str(stage / "config.json"),
        runtime_dir=str(stage / "run"),
        state_dir=str(stage / "state"),
        ssh={"user": "admin"},
        workers={
            r: str(stage / ("routerd-logging" if r == "logging" else "routerd-modules"))
            for r in ["logging", "network", "control", "management", "pppoe", "aftr", "health"]
        },
    )
    (stage / "boot.json").write_text(json.dumps(boot))
    try:
        etc = stage / "etc-ppp"
        etc.mkdir()
        (etc / "options").write_text("")
        (etc / "pap-secrets").write_text("routerd2 * routerd2-fixture *\n")
        (etc / "pap-secrets").chmod(0o600)
        (stage / "www").mkdir()
        run(["mount", "--bind", str(etc), "/etc/ppp"])
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
        # RP-PPPoE appends its own MTU/MRU after the options file. Configure
        # the independent pppd at its command-line boundary, then verify LCP.
        ac_pppd = stage / "ac-pppd"
        ac_pppd.write_text('#!/bin/sh\nexec /usr/sbin/pppd "$@" mtu 1400 mru 1400\n')
        ac_pppd.chmod(0o500)
        (stage / "server.conf").write_text(
            "auth\nrequire-pap\nrefuse-chap\nlogfd 1\nhide-password\nlcp-echo-interval 1\nlcp-echo-failure 3\n"
        )
        for n in [ns, ppp, dsl, lan, ppp_app, dsl_app]:
            run(["ip", "netns", "add", n])
            run(["ip", "-n", n, "link", "set", "lo", "up"])
        for dev, peer, n in [
            ("wan0", "server0", ppp),
            ("wan1", "server1", dsl),
            ("lan0", "client0", lan),
        ]:
            run(
                [
                    "ip",
                    "link",
                    "add",
                    dev,
                    "netns",
                    ns,
                    "type",
                    "veth",
                    "peer",
                    "name",
                    peer,
                    "netns",
                    n,
                ]
            )
            run(["ip", "-n", n, "link", "set", peer, "up"])
        run(["ip", "-n", lan, "addr", "add", "10.42.0.2/24", "dev", "client0"])
        run(["ip", "-n", lan, "route", "add", "default", "via", "10.42.0.1", "dev", "client0"])
        run(["ip", "-n", lan, "-6", "addr", "add", "2001:db8:43::2/64", "dev", "client0"])
        run(["ip", "-n", lan, "-6", "route", "add", "default", "via", "2001:db8:43::1"])
        run(["ip", "-n", dsl, "addr", "add", "2001:db8:42::1/64", "dev", "server1"])
        for provider, backend in [(ppp, ppp_app), (dsl, dsl_app)]:
            run(["ip", "-n", provider, "addr", "add", "198.51.100.10/32", "dev", "lo"])
            run(
                [
                    "ip",
                    "link",
                    "add",
                    "backend0",
                    "netns",
                    provider,
                    "type",
                    "veth",
                    "peer",
                    "name",
                    "uplink0",
                    "netns",
                    backend,
                ]
            )
            for n, dev, address in [
                (provider, "backend0", "192.0.3.1/24"),
                (backend, "uplink0", "192.0.3.2/24"),
            ]:
                run(["ip", "-n", n, "link", "set", dev, "up"])
                run(["ip", "-n", n, "addr", "add", address, "dev", dev])
            run(["ip", "-n", provider, "route", "add", "198.51.100.20/32", "via", "192.0.3.2"])
            run(["ip", "-n", backend, "addr", "add", "198.51.100.20/32", "dev", "lo"])
            run(["ip", "-n", backend, "route", "add", "default", "via", "192.0.3.1"])
            run(inside(provider, ["sysctl", "-qw", "net.ipv4.ip_forward=1"]))
            # A real upstream firewall can suppress PMTU feedback. Keep this
            # condition outside the product and count the suppressed packets.
            rules = "table inet peer { chain output { type filter hook output priority 0; policy accept; ip protocol icmp icmp type destination-unreachable icmp code frag-needed counter drop; }; }\n"
            subprocess.run(inside(provider, ["nft", "-f", "-"]), input=rules, text=True, check=True)
        for n, dev, address in [
            (dsl, "backend0", "2001:db8:44::1/64"),
            (dsl_app, "uplink0", "2001:db8:44::2/64"),
            (dsl_app, "lo", "2001:db8:45::1/128"),
        ]:
            run(["ip", "-n", n, "-6", "addr", "add", address, "dev", dev])
        run(["ip", "-n", dsl_app, "-6", "route", "add", "default", "via", "2001:db8:44::1"])
        run(["ip", "-n", dsl, "-6", "route", "add", "2001:db8:45::1/128", "via", "2001:db8:44::2"])
        run(["ip", "-n", dsl, "-6", "route", "add", "2001:db8:43::/64", "via", "2001:db8:42::2"])
        run(inside(dsl, ["sysctl", "-qw", "net.ipv6.conf.all.forwarding=1"]))
        run(
            [
                "ip",
                "-n",
                dsl,
                "-6",
                "tunnel",
                "add",
                "aftr0",
                "mode",
                "ipip6",
                "local",
                "2001:db8:42::1",
                "remote",
                "2001:db8:42::2",
                "dev",
                "server1",
                "encaplimit",
                "none",
            ]
        )
        run(
            ["ip", "-n", dsl, "addr", "add", "192.0.0.1/32", "peer", "192.0.0.2/32", "dev", "aftr0"]
        )
        run(["ip", "-n", dsl, "link", "set", "aftr0", "up"])
        # An independently owned marking table supplies a real incoming mark.
        # The product must preserve it when replacing its own firewall table.
        marking = 'table inet fixture_mark { chain input_mark { type filter hook prerouting priority -150; policy accept; iifname "lan0" udp sport 18084 meta mark set 41; }; }\n'
        subprocess.run(inside(ns, ["nft", "-f", "-"]), input=marking, text=True, check=True)
        capture = start(
            ns,
            [
                "tcpdump",
                "-Z",
                "root",
                "-U",
                "-n",
                "--immediate-mode",
                "-i",
                "any",
                "-w",
                str(stage / "traffic.pcap"),
            ],
            "capture",
        )
        ac = start(
            ppp,
            [
                "/usr/sbin/pppoe-server",
                "-q",
                str(ac_pppd),
                "-F",
                "-k",
                "-I",
                "server0",
                "-L",
                "192.0.2.1",
                "-R",
                "192.0.2.2",
                "-N",
                "1",
                "-O",
                str(stage / "server.conf"),
                "-X",
                str(stage / "ac.pid"),
            ],
            "ac",
        )
        daemon = start(
            ns, [str(stage / "routerd"), "boot", "--file", str(stage / "boot.json")], "console"
        )
        server = textwrap.dedent("""\
            import socket, threading, http.server, sys
            name = sys.argv[1].encode()
            class H(http.server.BaseHTTPRequestHandler):
                def do_GET(self):
                    self.send_response(200)
                    body = b'x' * (1024 * 1024) if self.path == '/large' else name
                    self.send_header('Content-Length', str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
                def do_POST(self):
                    import hashlib
                    body = self.rfile.read(int(self.headers['Content-Length']))
                    answer = hashlib.sha256(body).hexdigest().encode()
                    self.send_response(200)
                    self.send_header('Content-Length', str(len(answer)))
                    self.end_headers()
                    self.wfile.write(answer)
            def udp():
                s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
                s.setsockopt(socket.IPPROTO_IP,10,0)
                s.bind(('198.51.100.20', 18081))
                while True:
                    body, address = s.recvfrom(4096)
                    s.sendto(name+b':'+body if len(body)>100 else name,address)
            threading.Thread(target=udp, daemon=True).start()
            def small_mss_server(server):
                server.socket.setsockopt(socket.IPPROTO_TCP,socket.TCP_MAXSEG,1200)
                server.server_bind();server.server_activate()
                threading.Thread(target=server.serve_forever,daemon=True).start()
            small_mss_server(http.server.ThreadingHTTPServer(('198.51.100.20',18083),H,bind_and_activate=False))
            if name == b'DSL':
                class V6Server(http.server.ThreadingHTTPServer):
                    address_family = socket.AF_INET6
                threading.Thread(target=V6Server(('2001:db8:45::1',18080),H).serve_forever, daemon=True).start()
                small_mss_server(V6Server(('2001:db8:45::1',18083),H,bind_and_activate=False))
            http.server.ThreadingHTTPServer(('198.51.100.20', 18080), H).serve_forever()
        """)
        for n, name in [(ppp_app, "PPP"), (dsl_app, "DSL")]:
            start(n, ["python3", "-u", "-c", server, name], "apps-" + name)
        a = probe(ppp, "probe-ppp")
        b = probe(dsl, "probe-dsl")
        wait(lambda: selected(1000, 101) and selected(1100, 102))
        traffic("PPP")
        large_transfer("198.51.100.20", 1400)
        udp_mtu("PPP")
        large_transfer("2001:db8:45::1", 1460)
        assert all(x["generation"] == 1 for x in modules().values())
        print(
            "PASS two independent PPP/AFTR WANs and actual marked Health/PBR -> primary PPP TCP/UDP",
            flush=True,
        )
        a.terminate()
        a.wait(timeout=3)
        wait(lambda: not selected(1000, 101))
        traffic("DSL")
        large_transfer("198.51.100.20", 1460)
        udp_mtu("DSL")
        marked_udp()
        print(
            "PASS primary reachability loss selected real AFTR TCP/UDP while PPP stayed up",
            flush=True,
        )
        a = probe(ppp, "probe-ppp")
        wait(lambda: selected(1000, 101))
        traffic("PPP")
        print("PASS primary recovery returned actual TCP/UDP to PPP", flush=True)
        pid = modules()["service.health-ppp"]["pid"]
        os.kill(pid, signal.SIGSTOP)
        try:
            wait(lambda: not selected(1000, 101), 3)
            traffic("DSL")
            previous = api("control.sock", "/v1/services/service.health-ppp")
            replay = {
                k: previous[k]
                for k in [
                    "supervisor_instance",
                    "name",
                    "role",
                    "config_digest",
                    "generation",
                    "producer_epoch",
                    "sequence",
                    "snapshot",
                ]
            }
            api("control.sock", "/v1/services/state", replay)
            assert not selected(1000, 101)
        finally:
            os.kill(pid, signal.SIGCONT)
        wait(lambda: selected(1000, 101))
        traffic("PPP")
        print(
            "PASS consumer expiry and stale replay preserved AFTR fallback; fresh probe restored PPP",
            flush=True,
        )
        trial = copy.deepcopy(configuration)
        next(r for r in trial["resources"] if r["id"] == "app-ppp")["policy_rule"]["table"] = 102
        api(
            "control.sock",
            "/v1/config/commit",
            dict(
                expected_revision=1,
                request_id="dual-wan-trial",
                config=trial,
                confirm_timeout_seconds=5,
            ),
        )
        wait(lambda: selected(1000, 102))
        traffic("DSL")
        wait(lambda: api("control.sock", "/v1/config")["revision"] == 3 and selected(1000, 101), 12)
        traffic("PPP")
        print("PASS confirmed rollback restored actual preferred WAN TCP/UDP", flush=True)
        original = modules()["service.internet-dsl"]
        prior_health = modules()["service.health-dsl"]["generation"]
        old_aftr = api("control.sock", "/v1/services/service.internet-dsl")
        os.kill(original["pid"], signal.SIGKILL)
        traffic("PPP")

        def recovered():
            state = api("control.sock", "/v1/services")
            mods = modules()
            return (
                mods["service.internet-dsl"]["generation"] > original["generation"]
                and mods["service.internet-dsl"]["ready"]
                and mods.get("service.health-dsl", {}).get("generation", 0) > prior_health
                and state["phase"] == "ready"
                and mods["service.health-dsl"]["ready"]
                and api("control.sock", "/v1/services/service.health-dsl")["snapshot"].get("active")
                and selected(1100, 102)
            )

        wait(recovered)
        replay = {
            k: old_aftr[k]
            for k in [
                "supervisor_instance",
                "name",
                "role",
                "config_digest",
                "generation",
                "producer_epoch",
                "sequence",
                "snapshot",
            ]
        }
        api("control.sock", "/v1/services/state", replay, 409)
        traffic("PPP")
        a.terminate()
        a.wait(timeout=3)
        wait(lambda: not selected(1000, 101))
        traffic("DSL")
        large_transfer("198.51.100.20", 1460)
        udp_mtu("DSL")
        print(
            "PASS actual AFTR worker restart recovered backup forwarding; primary remained usable",
            flush=True,
        )
    except BaseException:
        try:
            print("services", api("control.sock", "/v1/services"), flush=True)
        except Exception:
            pass
        for name in ["console", "ac", "apps-PPP", "apps-DSL"]:
            p = stage / (name + ".log")
            if p.exists():
                print(name, p.read_text(), flush=True)
        raise
    finally:
        for p in reversed(children):
            if p.poll() is None:
                p.send_signal(signal.SIGINT if p is capture else signal.SIGTERM)
                try:
                    p.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
        for n in [ns, ppp, dsl, lan, ppp_app, dsl_app]:
            for pid in subprocess.run(
                ["ip", "netns", "pids", n], capture_output=True, text=True
            ).stdout.split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            subprocess.run(["ip", "netns", "delete", n], capture_output=True)
        print("Own dual-WAN namespaces stopped", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/dual-wan"))
    args = parser.parse_args()
    if args.inside:
        peer(args.inside)
        return
    args.output.mkdir(parents=True, exist_ok=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-dual-wan.XXXXXX"]).strip()
    try:
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            run(
                root
                + [
                    "install",
                    "-m",
                    "500",
                    str(args.binary_dir.resolve() / name),
                    stage + "/" + name,
                ]
            )
        run(root + ["install", "-m", "500", str(Path(__file__).resolve()), stage + "/peer.py"])
        with (args.output / "peer.log").open("w") as output:
            result = subprocess.run(
                root
                + [
                    "unshare",
                    "--mount",
                    "--propagation",
                    "private",
                    "python3",
                    "-I",
                    stage + "/peer.py",
                    "--inside",
                    stage,
                ],
                stdout=output,
                stderr=subprocess.STDOUT,
                timeout=180,
            )
        for name in ("traffic.pcap", "console.log"):
            artifact = subprocess.run(root + ["cat", stage + "/" + name], capture_output=True)
            if artifact.returncode == 0:
                (args.output / name).write_bytes(artifact.stdout)
        if result.returncode:
            raise RuntimeError(
                "dual-WAN communication failed; see " + str(args.output / "peer.log")
            )
        if not (args.output / "traffic.pcap").is_file():
            raise RuntimeError("missing actual dual-WAN capture")
        print("PASS; " + str(args.output / "peer.log"), flush=True)
    finally:
        run(root + ["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
