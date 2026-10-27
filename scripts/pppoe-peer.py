#!/usr/bin/env python3
"""Run routerd2 against an independent RP-PPPoE AC in private mount/network namespaces.

Requires Linux root/sudo, pppd 2.4.9 + rp-pppoe.so, pppoe-server, iproute2,
curl, tcpdump, ping, dnsmasq and dig. Host PPP files, hooks and routes are not used.
The optional --dhcp6-server selects an independent ISC dhcpd for PPP PD,
generated LAN RA/SLAAC, IPv6 forwarding and recovery, with normal DAD.
"""
import argparse
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import shutil
import time
import tarfile
import threading


def run(args):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=8).stdout


def wait(test, seconds=25):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if test():
            return
        time.sleep(0.1)
    raise RuntimeError("independent PPPoE communication condition timed out")


def deadline(*_):
    raise TimeoutError("PPPoE fixture deadline")


def terminated(*_):
    raise KeyboardInterrupt()


def peer(stage, auth, dhcp6_server):
    # This whole fixture has a private mount namespace. The product engine
    # must additionally create its own: AC secrets/hooks are not client input.
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(180)
    signal.signal(signal.SIGTERM, terminated)
    ns, acns, lan = (prefix + str(os.getpid()) for prefix in ("r2ppp-", "r2ac-", "r2lan-"))
    children, namespaces, frozen = [], [], []
    capture = None
    supervisor_frozen = False
    keepalive_thread = None
    dns_address = "192.0.2.1"

    def inside(name, args):
        return ["ip", "netns", "exec", name] + args

    def start(name, args, log):
        with (stage / (log + ".log")).open("a") as output:
            p = subprocess.Popen(inside(name, args), stdout=output, stderr=subprocess.STDOUT)
        children.append(p)
        return p

    def signal_worker(pid, sig):
        # Supervisor status uses its PID namespace. Enter that namespace for
        # signals; those numbers must never be used as host process IDs.
        run(["nsenter", "-t", str(pid1), "-m", "-p", "python3", "-c", "import os,sys;os.kill(int(sys.argv[1]),int(sys.argv[2]))", str(pid), str(int(sig))])

    def api(sock, path, body=None):
        args = ["curl", "--max-time", "3", "-sS", "--unix-socket", str(stage / "run" / sock), "http://localhost" + path]
        if body is not None:
            args += ["-X", "POST", "-H", "Content-Type: application/json", "--data", json.dumps(body)]
        return json.loads(run(args))

    def modules():
        return {x["name"]: x for x in api("supervisor.sock", "/v1/status")["modules"]}

    def record():
        return next(x for x in api("control.sock", "/v1/services")["instances"] if x["name"] == "service.internet")

    def link():
        return next((x for x in json.loads(run(["ip", "-n", ns, "-j", "addr", "show"])) if x["ifname"] == "ppp-wan" and any(a["local"] == "192.0.2.2" for a in x["addr_info"])), None)

    def routed():
        return any(r.get("dev") == "ppp-wan" and r.get("dst") == "default" and str(r.get("table")) == "100" for r in json.loads(run(["ip", "-n", ns, "-j", "route", "show", "table", "all"])))

    def routed6():
        return any(r.get("dev") == "ppp-wan" and r.get("dst") == "default" and str(r.get("table")) == "100" for r in json.loads(run(["ip", "-n", ns, "-j", "-6", "route", "show", "table", "all"])))

    def selected():
        return any(r.get("priority") == 1000 and str(r.get("table")) == "100" for r in json.loads(run(["ip", "-n", ns, "-j", "rule", "show"])))

    def traffic():
        assert run(inside(lan, ["curl", "--noproxy", "*", "--max-time", "2", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "http://198.51.100.20:18080/"])) == "200"
        code = 'import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.connect(("198.51.100.20",18081));s.send(b"pppoe");assert s.recv(100)==b"pppoe"'
        run(inside(lan, ["python3", "-c", code]))

    def traffic_fails():
        for protocol in ("tcp", "udp"):
            code = 'import socket;s=socket.socket(socket.AF_INET,' + ('socket.SOCK_STREAM' if protocol == 'tcp' else 'socket.SOCK_DGRAM') + ');s.settimeout(1);s.connect(("198.51.100.20",' + ('18080' if protocol == 'tcp' else '18081') + '));s.send(b"lost");s.recv(100)'
            assert subprocess.run(inside(lan, ["python3", "-c", code]), capture_output=True).returncode != 0

    def ac_start():
        return start(acns, ["/usr/sbin/pppoe-server", "-F", "-k", "-I", "server0", "-C", "ROUTERD2-AC", "-S", "ROUTERD2-LAB", "-L", "192.0.2.1", "-R", "192.0.2.2", "-N", "1", "-O", str(stage / "server.conf"), "-X", str(stage / "ac.pid")], "ac")

    def pd_record():
        return next((x for x in api("control.sock", "/v1/services")["instances"] if x["name"] == "service.pd"), {})

    def pd_bound():
        current, observed = modules().get("service.pd", {}), pd_record()
        return current.get("ready") and observed.get("generation") == current.get("generation") and observed.get("snapshot", {}).get("state") == "bound"

    def delegated():
        return [a for link in json.loads(run(["ip", "-n", ns, "-j", "-6", "addr", "show", "dev", "lan0"])) for a in link["addr_info"] if a.get("scope") == "global" and not a.get("tentative") and not a.get("dadfailed")]

    def client_addresses():
        return [a for link in json.loads(run(["ip", "-n", lan, "-j", "-6", "addr", "show", "dev", "client0"])) for a in link["addr_info"] if a.get("scope") == "global" and not a.get("tentative") and not a.get("dadfailed")]

    def ipv6_traffic():
        wait(lambda: pd_bound() and delegated() and client_addresses())
        assert run(inside(lan, ["curl", "--noproxy", "*", "--max-time", "2", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "http://[2001:db8:ffff::20]:18080/"])) == "200"
        code = 'import socket;s=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.settimeout(2);s.connect(("2001:db8:ffff::20",18081));s.send(b"delegated-ppp");assert s.recv(100)==b"delegated-ppp"'
        run(inside(lan, ["python3", "-c", code]))

    def dhcp6_start():
        run(["ip", "-n", acns, "-6", "addr", "replace", "2001:db8:ffff::1/64", "dev", ppp])
        return start(acns, [str(dhcp6_server), "-6", "-f", "-d", "-cf", str(stage / "dhcp6.conf"), "-lf", str(stage / "dhcp6.leases"), "-pf", str(stage / "dhcp6.pid"), ppp], "dhcp6")

    try:
        etc = stage / "etc-ppp"
        etc.mkdir()
        (etc / "options").write_text("")
        for name in ("pap-secrets", "chap-secrets"):
            (etc / name).write_text("routerd2 * routerd2-fixture *\n")
            (etc / name).chmod(0o600)
        run(["mount", "--bind", str(etc), "/etc/ppp"])
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
        root_options = stage / "root-options"
        root_options.mkdir()
        (root_options / ".ppprc").write_text("mtu 1300\n")
        run(["mount", "--bind", str(root_options), "/root"])
        (stage / "password").write_text("routerd2-fixture\n")
        (stage / "password").chmod(0o600)
        service = dict(interface="wan0", ppp_interface="ppp-wan", username="routerd2", password_file=str(stage / "password"), auth_method=auth, ipv6=bool(dhcp6_server) or auth == "chap", service_name="ROUTERD2-LAB", ac_name="ROUTERD2-AC", mtu=1454, mru=1454, lcp_echo_interval=1, lcp_echo_failure=3, metric=20, table=100)
        resources = [
            {"id": "wan", "interface": {"name": "wan0"}},
            {"id": "lan", "interface": {"name": "lan0"}},
            {"id": "lan-address", "address": {"interface": "lan0", "cidr": "10.42.0.1/24"}},
            {"id": "forwarding", "forwarding": {"ipv4": True, "ipv4_rp_filter": 0}},
            {"id": "firewall", "firewall": {"forward": [{"from": "lan0", "to": "ppp-wan"}], "masquerade": [{"source": "10.42.0.0/24", "out_interface": "ppp-wan"}]}},
            {"id": "internet", "service": {"name": "internet", "pppoe": service}},
            {"id": "probe-policy", "policy_rule": {"family": 4, "priority": 100, "table": 100, "mark": 42}},
            {"id": "probe", "depends_on": ["internet", "probe-policy"], "when": {"fact": "service.internet.active"}, "service": {"name": "reachability", "health": {"interface": "ppp-wan", "target": "198.51.100.20", "protocol": "tcp", "port": 18082, "mark": 42, "interval_ms": 200, "timeout_ms": 100, "unhealthy_threshold": 1}}},
            {"id": "application-policy", "when": {"fact": "service.reachability.active", "max_age": "1s"}, "policy_rule": {"family": 4, "priority": 1000, "table": 100, "source": "10.42.0.0/24", "in_interface": "lan0"}},
        ]
        resources.append({"id": "provider-dns-route", "depends_on": ["internet"], "when": {"fact": "service.internet.active"}, "route": {"destination": "192.0.2.53/32", "interface": "ppp-wan"}})
        resources.append({"id": "dns", "service": {"name": "dns", "dns": {"spec": {"listen": [{"name": "lan", "addresses": ["10.42.0.1"], "port": 53}], "sources": [{"name": "local", "kind": "zone", "match": ["router.test"], "zoneRef": ["local"]}, {"name": "provider", "kind": "forward", "match": ["."], "upstreamFrom": [{"resource": "PPPoESession/internet", "field": "dnsServers"}], "queryTimeout": "1s"}]}, "zones": [{"name": "local", "spec": {"zone": "router.test", "records": [{"hostname": "gateway", "ipv4": "10.42.0.1"}]}}]}}})
        if dhcp6_server:
            next(r for r in resources if r["id"] == "forwarding")["forwarding"]["ipv6"] = True
            resources += [
                {"id": "ipv6-policy", "policy_rule": {"family": 6, "priority": 1000, "table": 100, "in_interface": "lan0"}},
                {"id": "pd", "depends_on": ["internet", "lan"], "when": {"fact": "service.internet.active"}, "service": {"name": "pd", "dhcp6": {"interface": "ppp-wan", "iaid": 1, "request_prefix": True, "prefix_length": 60, "delegations": [{"name": "lan", "interface": "lan0", "subnet_id": 1, "advertise": True}]}}},
            ]
        configuration = {"schema_version": 2, "resources": resources}
        (stage / "config.json").write_text(json.dumps(configuration))
        boot = dict(platform="host", config=str(stage / "config.json"), runtime_dir=str(stage / "run"), state_dir=str(stage / "state"), ssh={"user": "admin"}, workers={r: str(stage.parent / ("routerd-logging" if r == "logging" else "routerd-modules")) for r in ["logging", "network", "control", "management", "pppoe", "health", "dns"]})
        if dhcp6_server:
            boot["workers"].update({r: str(stage.parent / "routerd-modules") for r in ("dhcp6", "ra")})
        boot["engines"] = {"pppoe": str(stage.parent / "pppoe-engine.tar")}
        key = stage / "key.pem"
        run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(key)])
        key.chmod(0o600)
        publisher = str(stage.parent / "routerd-bundle")
        (stage / "trust.json").write_text(run([publisher, "trust", "--private-key-file", str(key)]))
        (stage / "trust.json").chmod(0o600)
        layout = {"boot_abi": 1, "worker_abi": 4, "entrypoint": "routerd",
                  "roles": [{"role": r, "artifact": "routerd-logging" if r == "logging" else "routerd-modules"} for r in boot["workers"]],
                  "engines": [{"role": "pppoe", "artifact": "ppp-image", "sources": "ppp-sources"}],
                  "artifacts": [{"name": n, "kind": "native", "command": n, "file": str(stage.parent / n)} for n in ["routerd", "routerd-modules", "routerd-logging"]] +
                               [{"name": "ppp-image", "kind": "data", "file": str(stage.parent / "pppoe-engine.tar")}, {"name": "ppp-sources", "kind": "data", "file": str(stage.parent / "pppoe-sources.tar")}]}
        (stage / "layout.json").write_text(json.dumps(layout))
        version = json.loads(run([str(stage.parent / "routerd"), "version"]))
        packed = json.loads(run([publisher, "pack", "--private-key-file", str(key), "--layout", str(stage / "layout.json"), "--release-id", version["release_id"], "--sequence", "1", "--target-arch", "amd64", "--output", str(stage / "bundle.tar")]))
        # Reproduce the actual dlopen failure, then verify both publication and
        # receipt reject it before any network configuration or engine starts.
        broken = stage / "missing-provider.tar"
        with tarfile.open(stage.parent / "pppoe-engine.tar") as src, tarfile.open(broken, "w", format=tarfile.USTAR_FORMAT) as dst:
            for h in src:
                if h.name != "usr/lib/ossl-modules/legacy.so":
                    dst.addfile(h, src.extractfile(h))
        bad_layout = copy.deepcopy(layout)
        next(a for a in bad_layout["artifacts"] if a["name"] == "ppp-image")["file"] = str(broken)
        (stage / "bad-layout.json").write_text(json.dumps(bad_layout))
        bad = subprocess.run([publisher, "pack", "--private-key-file", str(key), "--layout", str(stage / "bad-layout.json"), "--release-id", version["release_id"], "--sequence", "2", "--target-arch", "amd64", "--output", str(stage / "bad-published.tar")], capture_output=True, text=True)
        assert bad.returncode != 0 and "legacy.so" in bad.stderr and not (stage / "bad-published.tar").exists()
        malformed = copy.deepcopy(packed["manifest"])
        artifact = next(a for a in malformed["artifacts"] if a["name"] == "ppp-image")
        artifact.update(size=broken.stat().st_size, sha256=hashlib.sha256(broken.read_bytes()).hexdigest())
        raw = json.dumps(malformed, separators=(",", ":")).encode()
        (stage / "signing-input").write_bytes(b"routerd2 signed extracted runtime manifest v4\x00" + raw)
        run(["openssl", "pkeyutl", "-sign", "-rawin", "-inkey", str(key), "-in", str(stage / "signing-input"), "-out", str(stage / "bad.sig")])
        with tarfile.open(stage / "bad-signed.tar", "w", format=tarfile.USTAR_FORMAT) as dst:
            for name, data in [("manifest.json", raw), ("manifest.sig", (stage / "bad.sig").read_bytes())]:
                h = tarfile.TarInfo(name); h.size = len(data); dst.addfile(h, io.BytesIO(data))
            for a in layout["artifacts"]:
                file = broken if a["name"] == "ppp-image" else Path(a["file"])
                h = tarfile.TarInfo(a["name"]); h.size = file.stat().st_size
                with file.open("rb") as data:
                    dst.addfile(h, data)
        bad = subprocess.run([str(stage.parent / "routerd"), "release", "stage", "--root", str(stage / "bad-store"), "--trust-file", str(stage / "trust.json"), "--target-arch", "amd64", "--file", str(stage / "bad-signed.tar")], capture_output=True, text=True)
        assert bad.returncode != 0 and "legacy.so" in bad.stderr
        print("PASS publisher and signed store rejected the reproduced missing-provider image before application", flush=True)
        selector = ["--root", str(stage / "releases"), "--trust-file", str(stage / "trust.json"), "--target-arch", "amd64"]
        run([str(stage.parent / "routerd"), "release", "stage", *selector, "--file", str(stage / "bundle.tar")])
        run([str(stage.parent / "routerd"), "release", "initialize", *selector, "--digest", packed["digest"]])
        boot.update(platform="appliance", workers={}, engines={}, release={"root": str(stage / "releases"), "trust_file": str(stage / "trust.json")})
        (stage / "boot.json").write_text(json.dumps(boot))
        (stage / "server.conf").write_text("auth\nrequire-" + auth + "\nrefuse-" + ("chap" if auth == "pap" else "pap") + "\nmtu 1454\nmru 1454\nms-dns 192.0.2.1\nlogfd 1\nhide-password\nlcp-echo-interval 1\nlcp-echo-failure 3\n")
        if dhcp6_server:
            with (stage / "server.conf").open("a") as out:
                out.write("+ipv6\n")
            # An access concentrator must route the delegated pool back to its
            # subscriber. This is peer setup; the product uses no PPP hooks.
            (etc / "ipv6-up").write_text('#!/bin/sh\nexec /usr/sbin/ip -6 route replace 2001:db8:80::/56 dev "$1"\n')
            (etc / "ipv6-up").chmod(0o700)
        for name in (ns, acns, lan):
            run(["ip", "netns", "add", name])
            namespaces.append(name)
            run(["ip", "-n", name, "link", "set", "lo", "up"])
        run(["ip", "link", "add", "wan0", "netns", ns, "type", "veth", "peer", "name", "server0", "netns", acns])
        run(["ip", "link", "add", "lan0", "netns", ns, "type", "veth", "peer", "name", "client0", "netns", lan])
        for name, dev in ((acns, "server0"), (lan, "client0")):
            run(["ip", "-n", name, "link", "set", dev, "up"])
        run(["ip", "-n", lan, "addr", "add", "10.42.0.2/24", "dev", "client0"])
        run(["ip", "-n", lan, "route", "add", "default", "via", "10.42.0.1", "dev", "client0"])
        run(["ip", "-n", acns, "addr", "add", "198.51.100.20/32", "dev", "lo"])
        run(["ip", "-n", acns, "addr", "add", "192.0.2.53/32", "dev", "lo"])
        capture = start(ns, ["tcpdump", "-Z", "root", "--immediate-mode", "-U", "-n", "-i", "any", "-w", str(stage / "pppoe.pcap")], "capture")
        ac = ac_start()
        command = ["unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child=TERM", str(stage.parent / "routerd"), "boot", "--file", str(stage / "boot.json")]
        daemon = start(ns, command, "console")
        child_file = Path(f"/proc/{daemon.pid}/task/{daemon.pid}/children")
        wait(lambda: child_file.read_text().strip())
        pid1 = int(child_file.read_text().split()[0])
        wait(lambda: link() is not None and routed())
        assert api("supervisor.sock", "/v1/status")["pid"] == 1
        print("PASS signed runtime catalog selected its PPP engine under PID 1", flush=True)
        start(acns, ["python3", "-u", "-m", "http.server", "18080", "--bind", "198.51.100.20", "--directory", str(etc)], "http")
        start(acns, ["python3", "-u", "-c", 'import socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(("198.51.100.20",18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)'], "udp")
        probe_command = ["python3", "-u", "-m", "http.server", "18082", "--bind", "198.51.100.20", "--directory", str(etc)]
        probe = start(acns, probe_command, "probe")
        wait(lambda: ":18080 " in run(inside(acns, ["ss", "-lnt"])))
        wait(selected)
        traffic()
        dns_peer = start(acns, ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-dynamic", "--listen-address=192.0.2.1,192.0.2.53", "--address=/ppp.example/198.51.100.77", "--local-ttl=0", "--log-queries=extra", "--log-facility=-"], "dns")
        def dns_traffic():
            def usable():
                summary = api("services/dns.dns.sock", "/v1/status")["resources"][0]["observed"].get("upstreams", "")
                return "udp://" + dns_address + ":53" in summary and "Expired" not in summary and "Unknown" not in summary
            wait(usable)
            for transport in ([], ["+tcp"]):
                assert run(inside(lan, ["dig", "@10.42.0.1", "peer.ppp.example", "A", "+tries=1", "+time=2", "+short"] + transport)).strip() == "198.51.100.77"
        wait(lambda: any(x.get("name") == "service.dns" and x.get("ready") for x in modules().values()))
        wait(lambda: bool(api("control.sock", "/v1/services/service.dns/dns-input").get("upstreams")))
        dns_traffic()
        print("PASS negotiated PPP DNS drives independent UDP/TCP resolver queries from LAN", flush=True)
        initial = record()
        assert set(initial["snapshot"]["dns_servers"]) == {"192.0.2.1"}
        assert all(x["generation"] == 1 for name, x in modules().items() if not dhcp6_server or name != "service.pd")
        assert link()["mtu"] == 1454
        print("PASS native " + auth + ": negotiated DNS, LAN TCP/UDP, MTU 1454, generation 1", flush=True)
        if dhcp6_server:
            peer_links = lambda: json.loads(run(["ip", "-n", acns, "-j", "-6", "addr", "show"]))
            wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
            ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
            mac = json.loads(run(["ip", "-n", acns, "-j", "link", "show", "dev", "server0"]))[0]["address"]
            (stage / "dhcp6.conf").write_text("default-lease-time 18;\npreferred-lifetime 12;\noption dhcp-renewal-time 5;\noption dhcp-rebinding-time 9;\nsubnet6 2001:db8:ffff::/64 {\n prefix6 2001:db8:80:: 2001:db8:80:f0:: /60;\n}\nserver-duid LL ethernet " + mac + ";\n")
            run([str(dhcp6_server), "-6", "-t", "-cf", str(stage / "dhcp6.conf")])
            (stage / "dhcp6.leases").touch()
            run(["ip", "-n", acns, "-6", "addr", "add", "2001:db8:ffff::20/128", "dev", "lo"])
            start(acns, ["python3", "-u", "-m", "http.server", "18080", "--bind", "2001:db8:ffff::20", "--directory", str(etc)], "http6")
            start(acns, ["python3", "-u", "-c", 'import socket\ns=socket.socket(socket.AF_INET6,socket.SOCK_DGRAM);s.bind(("2001:db8:ffff::20",18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)'], "udp6")
            server = dhcp6_start()
            ipv6_traffic()
            first = pd_record()
            duid = first["snapshot"]["client_duid"]
            assert duid.startswith("0004") and len(duid) == 36
            print("PASS PPP IPv6CP, ISC DHCP IA_PD, generated LAN RA/SLAAC, routed IPv6 TCP/UDP", flush=True)
            wait(lambda: pd_record()["snapshot"]["renew_at"] != first["snapshot"]["renew_at"], 12)
            ipv6_traffic()
            print("PASS real ISC DHCP Renew extended the delegated prefix", flush=True)
            old = modules()["service.pd"]
            signal_worker(old["pid"], signal.SIGKILL)
            wait(lambda: modules()["service.pd"]["generation"] > old["generation"] and pd_bound())
            assert pd_record()["snapshot"]["client_duid"] == duid
            ipv6_traffic()
            print("PASS DHCPv6 worker replacement reused persisted DUID and restored routed traffic", flush=True)
            stale = {k: first[k] for k in ["supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot"]}
            assert "stale" in json.dumps(api("control.sock", "/v1/services/state", stale)).lower()
            print("PASS old DHCPv6 worker notification rejected", flush=True)
            saved = modules()
            frozen = [saved["control"]["pid"], saved["service.internet"]["pid"]]
            for pid in frozen:
                signal_worker(pid, signal.SIGSTOP)
            wait(lambda: not routed() and not routed6(), 8)
            assert link() is not None
            assert subprocess.run(inside(lan, ["curl", "--noproxy", "*", "--max-time", "1", "-sS", "http://[2001:db8:ffff::20]:18080/"]), capture_output=True).returncode != 0
            for pid in frozen:
                signal_worker(pid, signal.SIGCONT)
            frozen = []
            wait(lambda: routed() and routed6())
            # The normal five-second watchdog can replace the frozen cohort.
            # ISC DHCP binds a specific PPP ifindex and must follow that peer
            # session replacement too; it cannot serve through its old socket.
            server.terminate()
            server.wait(timeout=5)
            server = dhcp6_start()
            ipv6_traffic()
            print("PASS original PPP deadline withdrew both family routes; supervision recovery and peer rebind restored IPv6 traffic", flush=True)
            # The independent server binds this peer's PPP interface. Restart
            # it with the recreated access session, preserving its lease file.
            server.terminate()
            server.wait(timeout=5)
            stable_dns = modules()["service.dns"]
            previous, old_index = modules()["service.internet"], link()["ifindex"]
            signal_worker(previous["pid"], signal.SIGKILL)
            wait(lambda: link() is not None and link()["ifindex"] != old_index and modules()["service.internet"]["generation"] > previous["generation"], 35)
            server = dhcp6_start()
            ipv6_traffic()
            assert pd_record()["snapshot"]["client_duid"] == duid
            dns_traffic()
            assert modules()["service.dns"]["generation"] == stable_dns["generation"]
            print("PASS PPP worker/session replacement reacquired PD with the same DUID and restored IPv6 TCP/UDP", flush=True)
            server.terminate()
            server.wait(timeout=5)
            wait(lambda: not delegated(), 24)
            wait(lambda: not client_addresses())
            print("PASS server silence expired router and independent SLAAC client prefixes", flush=True)
            server = dhcp6_start()
            ipv6_traffic()
            assert pd_record()["snapshot"]["client_duid"] == duid
            print("PASS independent ISC DHCP restart reacquired PD and restored IPv6 TCP/UDP", flush=True)
            stable = modules()["service.internet"]
            stable_index = link()["ifindex"]
            trial = copy.deepcopy(configuration)
            next(r for r in trial["resources"] if r["id"] == "internet")["service"]["pppoe"]["default_route"] = False
            api("control.sock", "/v1/config/commit", {"expected_revision": 1, "request_id": "pppoe-dual-family-route-trial", "config": trial, "confirm_timeout_seconds": 5})
            wait(lambda: not routed() and not routed6(), 3)
            assert subprocess.run(inside(lan, ["curl", "--noproxy", "*", "--max-time", "1", "-sS", "http://[2001:db8:ffff::20]:18080/"]), capture_output=True).returncode != 0
            wait(lambda: api("control.sock", "/v1/config")["revision"] == 3 and routed() and routed6(), 12)
            ipv6_traffic()
            traffic()
            assert link()["ifindex"] == stable_index and modules()["service.internet"]["generation"] == stable["generation"]
            print("PASS confirmed rollback restored both family routes and TCP/UDP without replacing the PPP session", flush=True)
            return
        probe.terminate()
        probe.wait(timeout=3)
        wait(lambda: not selected())
        assert routed() and link() is not None
        traffic_fails()
        probe = start(acns, probe_command, "probe")
        wait(selected)
        traffic()
        print("PASS PPP-scoped marked Health withdrew/restored LAN PBR and TCP/UDP while IPCP stayed up", flush=True)
        assert run(inside(ns, ["sysctl", "-n", "net.ipv4.conf.all.rp_filter", "net.ipv4.conf.default.rp_filter", "net.ipv4.conf.ppp-wan.rp_filter"])).split() == ["0", "0", "0"]
        # Removing the explicit profile restores the inherited RPF=2, making
        # marked Health fail. Timed rollback must restore the real path.
        stable_index = link()["ifindex"]
        trial = copy.deepcopy(configuration)
        next(r for r in trial["resources"] if r["id"] == "forwarding")["forwarding"].pop("ipv4_rp_filter")
        api("control.sock", "/v1/config/commit", {"expected_revision": 1, "request_id": "pppoe-rpf-trial", "config": trial, "confirm_timeout_seconds": 8})
        wait(lambda: not selected(), 5)
        assert run(inside(ns, ["sysctl", "-n", "net.ipv4.conf.all.rp_filter", "net.ipv4.conf.default.rp_filter"])).split() == ["2", "2"]
        traffic_fails()
        wait(lambda: api("control.sock", "/v1/config")["revision"] == 3 and selected(), 15)
        traffic()
        assert link()["ifindex"] == stable_index and modules()["service.internet"]["generation"] == 1
        print("PASS profile removal restored original RPF; confirmed rollback restored Health/PBR/TCP/UDP without PPP restart", flush=True)
        if auth == "chap":
            ll = next(a["address"] for a in link()["addr_info"] if a["family"] == "inet6")
            run(inside(ns, ["ping", "-6", "-I", "ppp-wan", "-c", "1", "-W", "2", ll]))
            print("PASS actual IPv6CP peer link-local ping", flush=True)
        saved = modules()
        os.kill(pid1, signal.SIGSTOP)
        supervisor_frozen = True
        frozen = [saved["control"]["pid"], saved["service.internet"]["pid"]]
        for pid in frozen:
            signal_worker(pid, signal.SIGSTOP)
        wait(lambda: not routed(), 8)
        assert link() is not None
        # The peer's connected PPP route remains usable after writer expiry.
        # A SERVFAIL therefore demonstrates DNS input expiry, not path loss.
        assert run(inside(ns, ["dig", "@192.0.2.1", "direct.ppp.example", "A", "+tries=1", "+time=2", "+short"])).strip() == "198.51.100.77"
        expired = run(inside(lan, ["dig", "@10.42.0.1", "expired.ppp.example", "A", "+tries=1", "+time=2", "+noall", "+comments"]))
        assert "status: SERVFAIL" in expired
        assert "expired.ppp.example" not in (stage / "dns.log").read_text()
        print("PASS original DNS deadline stopped upstream queries while peer DNS and PPP remained reachable", flush=True)
        print("PASS original observation expiry withdrew writer route with live PPP and frozen producers", flush=True)
        for pid in frozen:
            signal_worker(pid, signal.SIGCONT)
        frozen = []
        os.kill(pid1, signal.SIGCONT)
        supervisor_frozen = False
        wait(lambda: routed() and selected())
        traffic()
        dns_traffic()
        stable_dns = modules()["service.dns"]
        with (stage / "dns-tcp.log").open("w") as output:
            persistent = subprocess.Popen(inside(lan, ["stdbuf", "-oL", "dig", "@10.42.0.1", "+tcp", "+keepopen", "+short", "+tries=1", "+time=2", "-f", "-"]), stdin=subprocess.PIPE, stdout=output, stderr=subprocess.STDOUT, text=True)
        children.append(persistent)
        tcp_lock = threading.Lock()
        def send_tcp(name):
            with tcp_lock:
                persistent.stdin.write(name + " A\n")
                persistent.stdin.flush()
        send_tcp("before.ppp.example")
        wait(lambda: (stage / "dns-tcp.log").read_text().splitlines() == ["198.51.100.77"])
        # DNS TCP has a normal eight-second idle timeout. Keep this real client
        # active with a local-zone question while PPP is reconnecting.
        keepalive_stop = threading.Event()
        def keepalive():
            while not keepalive_stop.wait(2):
                send_tcp("gateway.router.test")
        keepalive_thread = threading.Thread(target=keepalive)
        keepalive_thread.start()
        dns_address = "192.0.2.53"
        dns_peer.terminate()
        dns_peer.wait(timeout=5)
        dns_peer = start(acns, ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-dynamic", "--listen-address=192.0.2.53", "--address=/ppp.example/198.51.100.77", "--local-ttl=0", "--log-queries=extra", "--log-facility=-"], "dns")
        server_options = stage / "server.conf"
        server_options.write_text(server_options.read_text().replace("ms-dns 192.0.2.1", "ms-dns 192.0.2.53"))
        previous, old_index = modules()["service.internet"], link()["ifindex"]
        signal_worker(previous["pid"], signal.SIGKILL)
        wait(lambda: modules()["service.internet"]["generation"] > previous["generation"] and routed() and selected() and link() is not None and link()["ifindex"] != old_index, 35)
        traffic()
        wait(lambda: record().get("snapshot", {}).get("dns_servers") == ["192.0.2.53"])
        wait(lambda: bool(api("control.sock", "/v1/services/service.dns/dns-input").get("upstreams")))
        dns_traffic()
        assert modules()["service.dns"]["generation"] == stable_dns["generation"]
        send_tcp("after.ppp.example")
        wait(lambda: (stage / "dns-tcp.log").read_text().splitlines().count("198.51.100.77") == 2)
        keepalive_stop.set()
        keepalive_thread.join(timeout=3)
        assert not keepalive_thread.is_alive()
        assert all(line in {"198.51.100.77", "10.42.0.1"} for line in (stage / "dns-tcp.log").read_text().splitlines())
        persistent.stdin.close()
        assert persistent.wait(timeout=5) == 0
        print("PASS the same dig TCP connection survived the learned DNS address change", flush=True)
        print("PASS new negotiated DNS address restored UDP/TCP queries without restarting the DNS worker", flush=True)
        print("PASS killed worker removed old PPP child; replacement restored LAN TCP/UDP", flush=True)
        stale = {k: initial[k] for k in ["supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot"]}
        assert "stale" in json.dumps(api("control.sock", "/v1/services/state", stale)).lower()
        traffic()
        print("PASS old generation notification rejected", flush=True)
        (stage / "password").write_text("wrong-fixture\n")
        ac.terminate()
        ac.wait(timeout=5)
        wait(lambda: link() is None and not routed())
        print("PASS independent AC stop withdrew PPP and native route", flush=True)
        ac = ac_start()
        # Reconnect uses the supervisor's existing bounded 30-second backoff.
        wait(lambda: "authentication failed" in (stage / "console.log").read_text().lower(), 45)
        assert not routed()
        print("PASS independent AC rejected incorrect credentials without a default route", flush=True)
        (stage / "password").write_text("routerd2-fixture\n")
        wait(lambda: routed() and selected(), 40)
        traffic()
        print("PASS independent AC restart restored LAN TCP/UDP", flush=True)
    except BaseException:
        try:
            print("services", api("control.sock", "/v1/services"), flush=True)
            print("rules", run(["ip", "-n", ns, "-j", "rule", "show"]), flush=True)
        except (OSError, subprocess.SubprocessError):
            pass
        for name in ("console", "ac", "capture", "dhcp6", "http6", "udp6", "dns", "dns-tcp"):
            p = stage / (name + ".log")
            if p.exists():
                print(name, p.read_text(errors="replace"), flush=True)
        raise
    finally:
        if keepalive_thread is not None:
            keepalive_stop.set()
            keepalive_thread.join(timeout=3)
        if supervisor_frozen:
            os.kill(pid1, signal.SIGCONT)
        for pid in frozen:
            signal_worker(pid, signal.SIGCONT)
        for p in reversed(children):
            if p is capture:
                continue
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
        if capture is not None and capture.poll() is None:
            capture.send_signal(signal.SIGINT)
            capture.wait(timeout=3)
        for name in namespaces:
            for pid in run(["ip", "netns", "pids", name]).split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            run(["ip", "netns", "delete", name])
        print("Private PPP network/mount namespaces stopped", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--auth", choices=("pap", "chap"))
    parser.add_argument("--binary-dir", type=Path, default=Path("dist/bin"))
    parser.add_argument("--output", type=Path, default=Path("dist/pppoe-peer"))
    parser.add_argument("--engine-image", type=Path, default=Path("dist/pppoe-engine.tar"), help="Pinned pppd root filesystem archive")
    parser.add_argument("--dhcp6-server", type=Path, help="Run PPP DHCPv6 PD against this independent ISC dhcpd executable")
    args = parser.parse_args()
    if args.inside:
        peer(args.inside, args.auth, args.dhcp6_server)
        return
    args.output.mkdir(parents=True, exist_ok=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-pppoe.XXXXXX"]).strip()
    try:
        for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle"):
            run(root + ["install", "-m", "500", str(args.binary_dir.resolve() / name), stage + "/" + name])
        run(root + ["install", "-m", "500", str(Path(__file__).resolve()), stage + "/peer.py"])
        peer_args = []
        run(root + ["install", "-m", "400", str(args.engine_image.resolve()), stage + "/pppoe-engine.tar"])
        run(root + ["install", "-m", "400", str(args.engine_image.resolve().with_name("pppoe-sources.tar")), stage + "/pppoe-sources.tar"])
        if args.dhcp6_server:
            run(root + ["install", "-m", "500", os.path.abspath(args.dhcp6_server), stage + "/dhcpd"])
            peer_args += ["--dhcp6-server", stage + "/dhcpd"]
        for auth in ((args.auth,) if args.auth else ("pap", "chap")):
            directory = stage + "/" + auth
            run(root + ["mkdir", "-m", "700", directory])
            with (args.output / (auth + ".log")).open("w") as output:
                result = subprocess.run(root + ["unshare", "--mount", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", directory, "--auth", auth] + peer_args, stdout=output, stderr=subprocess.STDOUT, timeout=200)
            packet = subprocess.run(root + ["cat", directory + "/pppoe.pcap"], capture_output=True)
            if packet.returncode == 0:
                (args.output / (auth + ".pcap")).write_bytes(packet.stdout)
            if result.returncode:
                raise RuntimeError(auth + " failed; see " + str(args.output / (auth + ".log")))
            if packet.returncode or len(packet.stdout) <= 24:
                raise RuntimeError(auth + " missing PPPoE capture")
            print(auth + ": PASS; " + str(args.output / (auth + ".log")), flush=True)
    finally:
        run(root + ["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
