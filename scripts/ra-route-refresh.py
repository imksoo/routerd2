#!/usr/bin/env python3
"""Check RA refresh and learned DNS against dnsmasq and a routed Linux AFTR.

Uses the actual supervisor/control/network/RA/AFTR workers, ordinary DAD, a
separate LAN client, one persistent TCP connection and 30,000 distinct UDP
exchanges. No lost exchange is retried. This is not a latency benchmark.
RA/DHCPv4/DHCPv6 learned DNS uses a separate dnsmasq peer that remains reachable
during RDNSS expiry. Requires root/sudo, iproute2, dnsmasq, dig, tcpdump, curl
and Python 3.
"""

import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import textwrap


def run(args):
    return subprocess.run(
        args, check=True, capture_output=True, text=True, timeout=8
    ).stdout


def expired(*_):
    raise TimeoutError("RA route fixture deadline")


def terminated(*_):
    raise KeyboardInterrupt()


def peer(stage):
    import time

    signal.signal(signal.SIGALRM, expired)
    signal.signal(signal.SIGTERM, terminated)
    signal.alarm(240)
    run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
    ns = "r2ragap-" + str(os.getpid())
    peer = ns + "p"
    client = ns + "c"
    children = []
    capture = None
    frozen = []

    def inside(n, a):
        return ["ip", "netns", "exec", n] + a

    def start(n, a, name):
        p = subprocess.Popen(
            inside(n, a),
            stdout=(stage / (name + ".log")).open("w"),
            stderr=subprocess.STDOUT,
        )
        children.append(p)
        return p

    def wait(test, t=25):
        end = time.monotonic() + t
        while time.monotonic() < end:
            if test():
                return
            time.sleep(0.05)
        raise RuntimeError("RA/AFTR actual forwarding timed out")

    boot = dict(
        platform="host",
        config=str(stage / "config.json"),
        runtime_dir=str(stage / "run"),
        state_dir=str(stage / "state"),
        ssh={"user": "admin"},
        workers={
            r: str(stage / ("routerd-logging" if r == "logging" else "routerd-modules"))
            for r in [
                "logging",
                "network",
                "control",
                "management",
                "ra_client",
                "aftr",
                "dns",
                "dhcp4",
                "dhcp6",
            ]
        },
    )
    (stage / "boot.json").write_text(json.dumps(boot))
    resources = [
        {"id": "wan", "interface": {"name": "wan0"}},
        {"id": "lan", "interface": {"name": "lan0"}},
        {"id": "wan-ip", "address": {"interface": "wan0", "cidr": "2001:db8:42::2/64"}},
        {"id": "lan-ip", "address": {"interface": "lan0", "cidr": "10.42.0.1/24"}},
        {"id": "forwarding", "forwarding": {"ipv4": True, "ipv4_rp_filter": 0}},
        {
            "id": "firewall",
            "firewall": {
                "forward": [{"from": "lan0", "to": "dslite0"}],
                "masquerade": [{"source": "10.42.0.0/24", "out_interface": "dslite0"}],
            },
        },
        {
            "id": "ra",
            "service": {
                "name": "wan-ra",
                "ra_client": {
                    "interface": "wan0",
                    "slaac": False,
                    "on_link": False,
                    "default_route": True,
                },
            },
        },
        {
            "id": "dslite",
            "service": {
                "name": "internet",
                "ds_lite": {
                    "interface": "wan0",
                    "tunnel": "dslite0",
                    "local_address": "2001:db8:42::2",
                    "candidates": ["2001:db8:99::1"],
                    "b4_peer": "192.0.0.1/32",
                    "route_gateway": "192.0.0.1",
                    "metric": 20,
                },
            },
        },
    ]
    resources += [
        {
            "id": "wan4",
            "service": {
                "name": "wan4",
                "dhcp4": {"interface": "wan0", "default_route": False},
            },
        },
        {
            "id": "info",
            "service": {"name": "info", "dhcp6": {"interface": "wan0", "iaid": 1}},
        },
        {
            "id": "dns",
            "service": {
                "name": "dns",
                "dns": {
                    "spec": {
                        "listen": [
                            {"name": "lan", "addresses": ["10.42.0.1"], "port": 53}
                        ],
                        "sources": [
                            {
                                "name": name,
                                "kind": "forward",
                                "match": [name + ".example"],
                                "queryTimeout": "1s",
                                "upstreamFrom": [{"resource": provider}],
                            }
                            for name, provider in [
                                ("ra", "IPv6RAAddress/wan-ra"),
                                ("v4", "DHCPv4Client/wan4"),
                                ("v6", "DHCPv6Information/info"),
                            ]
                        ],
                    }
                },
            },
        },
    ]
    resources[-1]["service"]["dns"]["spec"]["sources"].append(
        {
            "name": "boot",
            "kind": "forward",
            "match": ["boot.example"],
            "queryTimeout": "1s",
            "upstreams": ["udp://upstream.router.test:53"],
            "bootstrapResolverFrom": [{"resource": "IPv6RAAddress/wan-ra"}],
        }
    )
    for name, providers in [
        ("first4", ["DHCPv4Client/wan4", "IPv6RAAddress/wan-ra"]),
        ("first6", ["IPv6RAAddress/wan-ra", "DHCPv4Client/wan4"]),
    ]:
        resources[-1]["service"]["dns"]["spec"]["sources"].append(
            {
                "name": name,
                "kind": "forward",
                "match": [name + ".example"],
                "queryTimeout": "1s",
                "upstreamFrom": [{"resource": provider} for provider in providers],
            }
        )
    (stage / "config.json").write_text(
        json.dumps({"schema_version": 2, "resources": resources})
    )
    try:
        for n in [ns, peer, client]:
            run(["ip", "netns", "add", n])
            run(["ip", "-n", n, "link", "set", "lo", "up"])
        run(
            [
                "ip",
                "link",
                "add",
                "wan0",
                "netns",
                ns,
                "type",
                "veth",
                "peer",
                "name",
                "server0",
                "netns",
                peer,
            ]
        )
        run(
            [
                "ip",
                "link",
                "add",
                "lan0",
                "netns",
                ns,
                "type",
                "veth",
                "peer",
                "name",
                "client0",
                "netns",
                client,
            ]
        )
        for n, iface in [(peer, "server0"), (client, "client0")]:
            run(["ip", "-n", n, "link", "set", iface, "up"])
        for addr, iface in [
            ("2001:db8:42::1/64", "server0"),
            ("192.0.2.1/24", "server0"),
            ("2001:db8:99::1/128", "lo"),
            ("198.51.100.20/32", "lo"),
        ]:
            run(["ip", "-n", peer, "addr", "add", addr, "dev", iface])
        run(["ip", "-n", client, "addr", "add", "10.42.0.2/24", "dev", "client0"])
        run(
            [
                "ip",
                "-n",
                client,
                "route",
                "add",
                "default",
                "via",
                "10.42.0.1",
                "dev",
                "client0",
            ]
        )
        run(
            [
                "ip",
                "-n",
                peer,
                "-6",
                "tunnel",
                "add",
                "aftr0",
                "mode",
                "ipip6",
                "local",
                "2001:db8:99::1",
                "remote",
                "2001:db8:42::2",
                "dev",
                "server0",
                "encaplimit",
                "none",
            ]
        )
        run(
            [
                "ip",
                "-n",
                peer,
                "addr",
                "add",
                "192.0.0.1/32",
                "peer",
                "192.0.0.2/32",
                "dev",
                "aftr0",
            ]
        )
        run(["ip", "-n", peer, "link", "set", "aftr0", "up"])
        capture = start(
            ns,
            [
                "tcpdump",
                "-Z",
                "root",
                "-U",
                "--immediate-mode",
                "-n",
                "-i",
                "any",
                "-w",
                str(stage / "traffic.pcap"),
            ],
            "capture",
        )
        start(ns, ["ip", "-ts", "-6", "monitor", "route"], "routes")
        daemon = start(
            ns,
            [str(stage / "routerd"), "boot", "--file", str(stage / "boot.json")],
            "console",
        )
        wait(
            lambda: all(
                not a.get("tentative", False)
                for l in json.loads(run(["ip", "-n", peer, "-j", "-6", "addr", "show"]))
                for a in l["addr_info"]
            )
        )
        link_local = next(
            a["local"]
            for l in json.loads(
                run(["ip", "-n", peer, "-j", "-6", "addr", "show", "dev", "server0"])
            )
            for a in l["addr_info"]
            if a["scope"] == "link"
        )
        start(
            peer,
            [
                "dnsmasq",
                "--no-daemon",
                "--conf-file=/dev/null",
                "--no-resolv",
                "--no-hosts",
                "--bind-interfaces",
                "--interface=server0",
                "--address=/#/198.51.100.77",
                "--address=/upstream.router.test/192.0.2.1",
                "--local-ttl=0",
            ],
            "dns",
        )
        ra_command = [
            "dnsmasq",
            "--no-daemon",
            "--conf-file=/dev/null",
            "--port=0",
            "--interface=server0",
            "--bind-interfaces",
            "--enable-ra",
            "--dhcp-range=::,constructor:server0,ra-stateless,64,2m",
            "--ra-param=server0,20,90",
            "--dhcp-range=192.0.2.100,192.0.2.110,120s",
            "--dhcp-option=option:dns-server,192.0.2.1",
            "--dhcp-option=option6:dns-server,[" + link_local + "]",
            "--pid-file=",
        ]
        ra = start(peer, ra_command, "ra")
        start(
            peer,
            [
                "python3",
                "-u",
                "-c",
                'import socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(("198.51.100.20",18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)',
            ],
            "udp",
        )
        wait(
            lambda: any(
                x["ifname"] == "dslite0" and "UP" in x["flags"]
                for x in json.loads(run(["ip", "-n", ns, "-j", "link", "show"]))
            )
        )
        wait(
            lambda: any(
                r.get("dst") == "default"
                for r in json.loads(run(["ip", "-n", ns, "-j", "-6", "route", "show"]))
            )
        )

        def dns_input():
            return json.loads(
                run(
                    inside(
                        ns,
                        [
                            "curl",
                            "--max-time",
                            "3",
                            "-sS",
                            "--unix-socket",
                            str(stage / "run/control.sock"),
                            "http://localhost/v1/services/service.dns/dns-input",
                        ],
                    )
                )
            )

        wait(
            lambda: {u["source"] for u in dns_input()["upstreams"]}
            == {"ra", "v4", "v6", "first4", "first6"}
        )

        def dns_traffic():
            # The coordinator publishes input before the DNS worker's next
            # poll. Wait for the consumer to apply all three endpoint sets.
            def applied():
                status = json.loads(
                    run(
                        inside(
                            ns,
                            [
                                "curl",
                                "--max-time",
                                "3",
                                "-sS",
                                "--unix-socket",
                                str(stage / "run/services/dns.dns.sock"),
                                "http://localhost/v1/status",
                            ],
                        )
                    )
                )
                pools = status["resources"][0]["observed"].get("upstreams", "")
                return all(
                    name + "=0:udp://" in pools
                    for name in ["ra", "v4", "v6", "first4", "first6"]
                )

            wait(applied)
            for source in ["ra", "v4", "v6", "boot", "first4", "first6"]:
                for transport in [[], ["+tcp"]]:
                    answer = run(
                        inside(
                            client,
                            [
                                "dig",
                                "@10.42.0.1",
                                "host." + source + ".example",
                                "A",
                                "+short",
                                "+time=2",
                                "+tries=1",
                            ]
                            + transport,
                        )
                    ).strip()
                    assert answer == "198.51.100.77", (source, transport, answer)

        dns_traffic()
        original_dns = dns_input()
        first4 = [
            u["address"] for u in original_dns["upstreams"] if u["source"] == "first4"
        ]
        first6 = [
            u["address"] for u in original_dns["upstreams"] if u["source"] == "first6"
        ]
        assert first4[0] == "192.0.2.1" and "%wan0" in first6[0]
        assert first4 == list(reversed(first6)), (first4, first6)
        assert all(
            "%wan0" in u["address"]
            for u in original_dns["upstreams"]
            if u["source"] in {"ra", "v6"}
        )
        print(
            "PASS actual RA/DHCP4/DHCP6 DNS: scoped link-local upstreams, LAN UDP/TCP answers",
            flush=True,
        )
        first_route = next(
            r
            for r in json.loads(run(["ip", "-n", ns, "-j", "-6", "route", "show"]))
            if r.get("dst") == "default"
        )
        first_expiry = time.monotonic() + first_route["expires"]
        start(
            peer,
            [
                "python3",
                "-u",
                "-c",
                "import http.server\nclass H(http.server.BaseHTTPRequestHandler):\n protocol_version='HTTP/1.1'\n def do_HEAD(self):self.send_response(200);self.send_header('Content-Length','0');self.end_headers()\nhttp.server.ThreadingHTTPServer(('198.51.100.20',18080),H).serve_forever()",
            ],
            "http",
        )
        wait(lambda: ":18080 " in run(inside(peer, ["ss", "-lnt"])))
        code = """
    import socket,time,collections,http.client
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(1);s.connect(('198.51.100.20',18081));failed=collections.Counter();ok=0
    c=http.client.HTTPConnection('198.51.100.20',18080,timeout=2);port=None
    for i in range(30000):
     if i%200==0:
      c.request('HEAD','/');r=c.getresponse();r.read();assert r.status==200
      p=c.sock.getsockname()[1]
      if port is None:port=p
      assert p==port
     b=i.to_bytes(4,'big')
     try:
      s.send(b);r=s.recv(100)
      if r==b:ok+=1
      else:failed['wrong-reply']+=1
     except OSError as e:failed[str(e)]+=1
     time.sleep(.001)
    c.close();print('actual DS-Lite UDP: successful',ok,'failed',dict(failed),'same TCP',port,flush=True)
    assert not failed
    """
        r = subprocess.run(
            inside(client, ["python3", "-u", "-c", textwrap.dedent(code)]),
            capture_output=True,
            text=True,
            timeout=60,
        )
        print(r.stdout, r.stderr, flush=True)
        assert r.returncode == 0
        renewed_route = next(
            r
            for r in json.loads(run(["ip", "-n", ns, "-j", "-6", "route", "show"]))
            if r.get("dst") == "default"
        )
        assert time.monotonic() + renewed_route["expires"] > first_expiry + 5
        print(
            "PASS RA refresh advanced the actual kernel route timer and kept AFTR TCP/UDP",
            flush=True,
        )
        dns_traffic()
        ra.terminate()
        ra.wait(timeout=3)
        status = json.loads(
            run(
                [
                    "curl",
                    "--max-time",
                    "3",
                    "-sS",
                    "--unix-socket",
                    str(stage / "run/supervisor.sock"),
                    "http://localhost/v1/status",
                ]
            )
        )
        assert all(x["generation"] == 1 for x in status["modules"])
        dns_deadline = (
            max(
                u["valid_until"]["nanos"]
                for u in dns_input()["upstreams"]
                if u["source"] == "ra"
            )
            / 1e9
        )
        frozen = [daemon.pid] + [
            x["pid"] for x in status["modules"] if x["name"] in ["control", "network"]
        ]
        for pid in frozen:
            os.kill(pid, signal.SIGSTOP)
        wait(
            lambda: subprocess.run(
                ["ip", "-n", ns, "-6", "route", "get", "2001:db8:99::1"],
                capture_output=True,
            ).returncode
            != 0,
            100,
        )
        # The DNS peer remains reachable on the connected WAN. The original
        # RDNSS deadline, not a receipt timeout or the route loss, must stop use.
        assert (
            run(
                inside(
                    ns,
                    [
                        "dig",
                        "-6",
                        "@" + link_local + "%wan0",
                        "peer.ra.example",
                        "A",
                        "+short",
                        "+tries=1",
                        "+time=2",
                    ],
                )
            ).strip()
            == "198.51.100.77"
        )
        # dnsmasq advertises RDNSS for 120s while its router lifetime is 90s.
        # Use the received DNS budget independently of the expired route.
        wait(
            lambda: time.clock_gettime(time.CLOCK_BOOTTIME) > dns_deadline + 1,
            max(2, dns_deadline - time.clock_gettime(time.CLOCK_BOOTTIME) + 2),
        )
        response = run(
            inside(
                client,
                ["dig", "@10.42.0.1", "expired.ra.example", "A", "+tries=1", "+time=2"],
            )
        )
        assert "status: SERVFAIL" in response
        bootstrap = run(
            inside(
                client,
                [
                    "dig",
                    "@10.42.0.1",
                    "expired.boot.example",
                    "A",
                    "+tries=1",
                    "+time=3",
                ],
            )
        )
        assert "status: SERVFAIL" in bootstrap
        print(
            "PASS expired learned bootstrap stopped new hostname resolution without host DNS fallback",
            flush=True,
        )
        print(
            "PASS original RDNSS expiry stopped resolver forwarding while independent DNS stayed reachable",
            flush=True,
        )
        failed = subprocess.run(
            inside(
                client,
                [
                    "curl",
                    "--noproxy",
                    "*",
                    "--max-time",
                    "1",
                    "-sS",
                    "http://198.51.100.20:18080/",
                ],
            ),
            capture_output=True,
        )
        assert failed.returncode != 0
        print(
            "PASS original renewed kernel timer expired with control/writer/supervisor frozen; TCP stopped",
            flush=True,
        )
        for pid in frozen:
            os.kill(pid, signal.SIGCONT)
        frozen = []
        ra = start(peer, ra_command, "ra-recovery")
        wait(
            lambda: subprocess.run(
                ["ip", "-n", ns, "-6", "route", "get", "2001:db8:99::1"],
                capture_output=True,
            ).returncode
            == 0,
            30,
        )
        assert (
            run(
                inside(
                    client,
                    [
                        "curl",
                        "--noproxy",
                        "*",
                        "--max-time",
                        "3",
                        "-sS",
                        "-I",
                        "-o",
                        "/dev/null",
                        "-w",
                        "%{http_code}",
                        "http://198.51.100.20:18080/",
                    ],
                )
            )
            == "200"
        )
        wait(
            lambda: {u["source"] for u in dns_input()["upstreams"]}
            == {"ra", "v4", "v6", "first4", "first6"}
        )
        dns_traffic()
        print(
            "PASS fresh independent RA restored routed AFTR TCP after expiry and process recovery",
            flush=True,
        )
    except BaseException:
        for name in ["console", "ra"]:
            p = stage / (name + ".log")
            if p.exists():
                print(name, p.read_text(), flush=True)
        raise
    finally:
        for pid in frozen:
            try:
                os.kill(pid, signal.SIGCONT)
            except ProcessLookupError:
                pass
        for p in reversed(children):
            if p.poll() is None:
                p.send_signal(signal.SIGINT if p is capture else signal.SIGTERM)
                try:
                    p.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
        for n in [ns, peer, client]:
            for pid in subprocess.run(
                ["ip", "netns", "pids", n], capture_output=True, text=True
            ).stdout.split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            subprocess.run(["ip", "netns", "delete", n], capture_output=True)
        print("Own RA/AFTR namespaces cleaned", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/ra-route-refresh"))
    args = parser.parse_args()
    if args.inside:
        peer(args.inside)
        return
    args.output.mkdir(parents=True, exist_ok=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-ra-refresh.XXXXXX"]).strip()
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
        run(
            root
            + [
                "install",
                "-m",
                "500",
                str(Path(__file__).resolve()),
                stage + "/peer.py",
            ]
        )
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
                timeout=280,
            )
        for name in ("traffic.pcap", "routes.log"):
            artifact = subprocess.run(
                root + ["cat", stage + "/" + name], capture_output=True
            )
            if artifact.returncode == 0:
                (args.output / name).write_bytes(artifact.stdout)
        if result.returncode:
            raise RuntimeError(
                "RA/AFTR route refresh failed; see " + str(args.output / "peer.log")
            )
        if not (args.output / "traffic.pcap").is_file():
            raise RuntimeError("missing actual RA/AFTR capture")
        print("PASS; " + str(args.output / "peer.log"), flush=True)
    finally:
        run(
            root
            + [
                "python3",
                "-I",
                "-c",
                "import shutil,sys;shutil.rmtree(sys.argv[1])",
                stage,
            ]
        )


if __name__ == "__main__":
    main()
