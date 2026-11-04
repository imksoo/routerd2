#!/usr/bin/env python3
"""Run routerd2 against an independent RP-PPPoE AC in private mount/network namespaces.

Requires Linux root/sudo, pppd 2.4.9 + rp-pppoe.so, pppoe-server, iproute2,
curl, tcpdump, ping, dnsmasq and dig. Host PPP files, hooks and routes are not used.
The optional --dhcp6-server selects an independent ISC dhcpd for PPP PD,
configured LAN RA/SLAAC, IPv6 forwarding and recovery, with normal DAD.
"""
import argparse
import copy
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import shutil
import time
import threading


def run(args, timeout=8):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=timeout).stdout


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
    updating = (stage.parent / "candidate.tar").exists()
    signal.alarm(600 if updating else 180)
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

    def api(sock, path, body=None, timeout=3, status=200):
        args = ["curl", "--max-time", str(timeout), "-w", "\n%{http_code}", "-sS", "--unix-socket", str(stage / "run" / sock), "http://localhost" + path]
        if body is not None:
            args += ["-X", "POST", "-H", "Content-Type: application/json", "--data", json.dumps(body)]
        response = subprocess.run(args, capture_output=True, text=True, timeout=timeout + 1)
        if response.returncode:
            raise RuntimeError(path + ": " + response.stdout.strip() + " " + response.stderr.strip())
        content, actual = response.stdout.rsplit("\n", 1)
        if actual != str(status):
            raise RuntimeError(path + ": HTTP " + actual + " " + content)
        return json.loads(content)

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

    def pd_active():
        current, observed = modules().get("service.pd", {}), pd_record()
        snapshot = observed.get("snapshot", {})
        return current.get("ready") and observed.get("generation") == current.get("generation") and snapshot.get("state") in ("bound", "renewing", "rebinding") and bool(snapshot.get("prefixes"))

    def delegated():
        return [a for link in json.loads(run(["ip", "-n", ns, "-j", "-6", "addr", "show", "dev", "lan0"])) for a in link["addr_info"] if a.get("scope") == "global" and not a.get("tentative") and not a.get("dadfailed")]

    def client_addresses():
        return [a for link in json.loads(run(["ip", "-n", lan, "-j", "-6", "addr", "show", "dev", "client0"])) for a in link["addr_info"] if a.get("scope") == "global" and not a.get("tentative") and not a.get("dadfailed")]

    def client_routed():
        routes = json.loads(run(["ip", "-n", lan, "-j", "-6", "route", "show", "default"]))
        return any(route.get("dev") == "client0" for route in routes)

    def ipv6_traffic():
        wait(lambda: pd_active() and delegated() and client_addresses() and client_routed() and api("control.sock", "/v1/services")["phase"] == "ready")
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
            {"id": "firewall", "firewall": {"forward": [{"from": "lan0", "to": "ppp-wan"}]}},
            {"id": "nat", "nat": [{"type": "masquerade", "match": {"family": 4, "source": "10.42.0.0/24", "out_interface": "ppp-wan"}}]},
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
                {"id": "pd", "depends_on": ["internet", "lan"], "when": {"fact": "service.internet.active"}, "service": {"name": "pd", "dhcp6": {"interface": "ppp-wan", "iaid": 1, "request_prefix": True, "prefix_length": 60, "delegations": [{"name": "lan", "interface": "lan0", "subnet_id": 1}]}}},
                {"id": "pd-lan-ra", "when": {"fact": "service.pd.active"}, "service": {"name": "pd-lan-ra", "ra": {"interface": "lan0", "router_lifetime_seconds": 1800, "dns_lifetime_seconds": 1800, "min_interval_seconds": 3, "max_interval_seconds": 10, "from": {"service": "pd", "delegation": "lan"}}}},
            ]
        configuration = {"schema_version": 2, "resources": resources}
        (stage / "config.json").write_text(json.dumps(configuration))
        packed = json.loads((stage.parent / "runtime.json").read_text())
        selector = ["--root", str(stage / "releases"), "--trust-file", str(stage.parent / "trust.json"), "--target-arch", "amd64"]
        run([str(stage.parent / "routerd"), "release", "stage", *selector, "--file", str(stage.parent / "runtime.tar")], timeout=120)
        run([str(stage.parent / "routerd"), "release", "initialize", *selector, "--digest", packed["digest"]], timeout=120)
        boot = dict(platform="appliance", config=str(stage / "config.json"), runtime_dir=str(stage / "run"), state_dir=str(stage / "state"), ssh={"user": "admin"}, workers={}, engines={}, release={"root": str(stage / "releases"), "trust_file": str(stage.parent / "trust.json")})
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
            print("PASS PPP IPv6CP, ISC DHCP IA_PD, configured LAN RA/SLAAC, routed IPv6 TCP/UDP", flush=True)
            if updating:
                before_config = api("control.sock", "/v1/config")
                baseline = packed["manifest"]["release_id"]
                candidate = json.loads((stage.parent / "candidate.json").read_text())
                target = json.loads(run(["curl", "--max-time", "300", "--fail-with-body", "-sS",
                    "--unix-socket", str(stage / "run/control.sock"), "-X", "POST",
                    "--upload-file", str(stage.parent / "candidate.tar"),
                    "http://localhost/v1/releases/stage"], timeout=305))
                assert target["digest"] == candidate["digest"]
                for operation, identity in (("prepare", candidate["manifest"]["release_id"]), ("rollback", baseline), ("failure", candidate["manifest"]["release_id"])):
                    state = api("control.sock", "/v1/releases/status")
                    request = {"expected_generation": state["generation"], "expected_revision": before_config["revision"]}
                    if operation == "prepare":
                        request["release"] = target
                    pending = api("control.sock", "/v1/releases/" + ("prepare" if operation == "prepare" else "rollback"), request, timeout=120)
                    assert pending["pending"]["release"]["release_id"] == identity
                    previous_instance = api("supervisor.sock", "/v1/status")["instance_id"]
                    assert json.loads(run([str(stage.parent / "routerd"), "reboot", "--runtime-dir", str(stage / "run")]))["accepted"]
                    daemon.wait(timeout=12)
                    assert daemon.returncode == -signal.SIGHUP, daemon.returncode
                    server.terminate()
                    server.wait(timeout=3)
                    if operation == "failure":
                        # Reproduce a real startup resource conflict, without
                        # changing signed bytes or configuration/store files.
                        occupied = start(ns, ["python3", "-u", "-c", 'import socket,signal;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(("0.0.0.0",53));print("BOUND",flush=True);signal.pause()'], "occupied-dns")
                        wait(lambda: "BOUND" in (stage / "occupied-dns.log").read_text())
                    daemon = start(ns, command, "console")
                    child_file = Path(f"/proc/{daemon.pid}/task/{daemon.pid}/children")
                    wait(lambda: child_file.read_text().strip())
                    pid1 = int(child_file.read_text().split()[0])
                    if operation == "failure":
                        daemon.wait(timeout=180)
                        assert daemon.returncode == -signal.SIGHUP, daemon.returncode
                        failed = json.loads(run([str(stage.parent / "routerd"), "release", "status", *selector]))
                        assert failed["last_failure"]["release"]["digest"] == target["digest"]
                        assert failed["last_failure"]["reason"] == "release readiness deadline exceeded", failed["last_failure"]
                        print("PASS readiness failure persisted after worker shutdown with its original deadline reason", flush=True)
                        assert failed["active"]["release_id"] == baseline and not failed.get("attempt") and not failed.get("pending")
                        occupied.terminate()
                        occupied.wait(timeout=3)
                        # Restore the external fault; boot selection and the
                        # paired configuration recover through product code.
                        daemon = start(ns, command, "console")
                        child_file = Path(f"/proc/{daemon.pid}/task/{daemon.pid}/children")
                        wait(lambda: child_file.read_text().strip())
                        pid1 = int(child_file.read_text().split()[0])
                        identity = baseline
                    wait(lambda: link() is not None and routed(), 60)
                    wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
                    ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
                    server = dhcp6_start()
                    ipv6_traffic()
                    traffic()
                    dns_traffic()
                    if operation != "failure":
                        wait(lambda: api("control.sock", "/v1/releases/status").get("operation") == "confirm", 120)
                    current = api("supervisor.sock", "/v1/status")
                    assert current["pid"] == 1 and current["release_id"] == identity
                    assert current["instance_id"] != previous_instance
                    assert all(m["release_id"] == identity for m in current["modules"])
                    restored = api("control.sock", "/v1/config")
                    assert restored["config"] == before_config["config"]
                    assert restored["revision"] == before_config["revision"] + (2 if operation == "failure" else 1)
                    assert restored["startup"]["revision"] == restored["revision"]
                    if operation == "failure":
                        assert restored["startup"]["recovered"]
                    assert pd_record()["snapshot"]["client_duid"] == duid
                    confirmed = api("control.sock", "/v1/releases/status")
                    assert confirmed["active"]["release_id"] == identity
                    assert confirmed["high_watermark"] == 2 and not confirmed.get("attempt") and not confirmed.get("pending")
                    print("PASS signed runtime " + operation + " selected " + identity + ": persisted configuration/DUID and LAN DNS/IPv4/IPv6 TCP/UDP", flush=True)
                    before_config = restored
                    if operation == "prepare":
                        # Binary rollback must retain changes made after the update.
                        changed = copy.deepcopy(restored["config"])
                        zone = next(r for r in changed["resources"] if r["id"] == "dns")["service"]["dns"]["zones"][0]["spec"]
                        zone["records"].append({"hostname": "after-update", "ipv4": "10.42.0.99"})
                        api("control.sock", "/v1/config/commit", {"expected_revision": restored["revision"], "request_id": "post-runtime-update", "config": changed})
                        before_config = api("control.sock", "/v1/config")
                    for transport in ([], ["+tcp"]):
                        assert run(inside(lan, ["dig", "@10.42.0.1", "after-update.router.test", "A", "+short", "+tries=1", "+time=2"] + transport)).strip() == "10.42.0.99"
                assert json.loads(run([str(stage.parent / "routerd"), "poweroff", "--runtime-dir", str(stage / "run")]))["accepted"]
                daemon.wait(timeout=12)
                assert daemon.returncode == -signal.SIGINT, daemon.returncode
                capture.send_signal(signal.SIGINT)
                capture.wait(timeout=3)
                packets = run(["tcpdump", "-n", "-r", str(stage / "pppoe.pcap"), "ip6"])
                assert "dhcp6 release" not in packets
                print("PASS update/rollback clean poweroff without upstream PD Release", flush=True)
                return
            wait(lambda: pd_record()["snapshot"]["renew_at"] != first["snapshot"]["renew_at"], 12)
            ipv6_traffic()
            print("PASS real ISC DHCP Renew extended the delegated prefix", flush=True)
            server.send_signal(signal.SIGSTOP)
            time.sleep(0.1)
            retained = pd_record()["snapshot"]
            recovery_at = time.time()
            old = modules()["service.pd"]
            signal_worker(old["pid"], signal.SIGKILL)
            wait(lambda: modules()["service.pd"]["generation"] > old["generation"] and pd_active())
            assert pd_record()["snapshot"]["client_duid"] == duid
            assert pd_record()["snapshot"]["prefixes"] == retained["prefixes"], "worker restart changed original PD deadlines without a server Reply"
            ipv6_traffic()
            time.sleep(1.2)  # allow the randomized Rebind to reach the paused peer
            recovery_reply_at = time.time()
            server.send_signal(signal.SIGCONT)
            wait(lambda: pd_bound() and pd_record()["snapshot"]["renew_at"] != retained["renew_at"])
            ipv6_traffic()
            print("PASS DHCPv6 crash recovery retained DUID/IAID/PD and exact deadlines while ISC was paused, then Rebind/Reply restored renewal", flush=True)
            stale = {k: first[k] for k in ["supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot"]}
            assert "stale" in json.dumps(api("control.sock", "/v1/services/state", stale, status=409)).lower()
            print("PASS old DHCPv6 worker notification rejected", flush=True)
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
            wait(pd_bound)
            retained = pd_record()["snapshot"]["prefixes"]
            server.terminate()
            server.wait(timeout=5)
            previous = modules()["service.pd"]
            signal_worker(previous["pid"], signal.SIGKILL)
            wait(lambda: modules()["service.pd"]["generation"] > previous["generation"] and pd_active())
            assert pd_record()["snapshot"]["prefixes"] == retained, "recovery while the server was absent rebased PD deadlines"
            wait(lambda: not delegated(), 24)
            wait(lambda: not client_addresses())
            previous = modules()["service.pd"]
            signal_worker(previous["pid"], signal.SIGKILL)
            wait(lambda: modules()["service.pd"]["generation"] > previous["generation"] and modules()["service.pd"]["ready"])
            assert not pd_record()["snapshot"].get("prefixes") and not delegated() and not client_addresses(), "worker restart revived an expired delegation"
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
            retired = copy.deepcopy(configuration)
            retired["resources"] = [r for r in retired["resources"] if r["id"] not in ("pd", "pd-lan-ra")]
            api("control.sock", "/v1/config/commit", {"expected_revision": 3, "request_id": "pd-retire", "config": retired})
            wait(lambda: "service.pd" not in modules() and "service.pd-lan-ra" not in modules())
            assert not delegated() and link()["ifindex"] == stable_index
            print("PASS PD/RA retirement withdrew advertisements and LAN addresses while PPP stayed up", flush=True)
            api("control.sock", "/v1/config/commit", {"expected_revision": 4, "request_id": "pd-restore", "config": configuration})
            ipv6_traffic()
            assert pd_record()["snapshot"]["client_duid"] == duid
            assert link()["ifindex"] == stable_index
            # Remove the WAN and its dependents in one operator transaction.
            # The DHCPv6 child withdraws local state before PPP is removed.
            offline = copy.deepcopy(configuration)
            offline["resources"] = [r for r in offline["resources"] if r["id"] in ("wan", "lan", "lan-address", "forwarding")]
            api("control.sock", "/v1/config/commit", {"expected_revision": 5, "request_id": "wan-retire", "config": offline})
            wait(lambda: "service.internet" not in modules() and "service.pd" not in modules() and link() is None)
            assert not delegated()
            print("PASS removing PPP and PD together drained DHCPv6 before its WAN", flush=True)
            if server.poll() is None:
                server.terminate()
                server.wait(timeout=3)
            api("control.sock", "/v1/config/commit", {"expected_revision": 6, "request_id": "wan-restore", "config": configuration})
            wait(lambda: link() is not None and routed())
            wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
            ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
            server = dhcp6_start()
            ipv6_traffic()
            traffic()
            assert pd_record()["snapshot"]["client_duid"] == duid
            # A PPP-only configuration change also invalidates its unchanged
            # DHCPv6 child: withdraw its local state before replacing the link.
            changing = copy.deepcopy(configuration)
            wan = next(r for r in changing["resources"] if r["id"] == "internet")["service"]["pppoe"]
            wan.update(mtu=1452, mru=1452)
            before_release = (stage / "dhcp6.log").read_text().count("releases prefix ")
            before = modules()
            previous_ppp, previous_pd, previous_dns = (before[name] for name in ("service.internet", "service.pd", "service.dns"))
            wan_change_at = time.time()
            api("control.sock", "/v1/config/commit", {"expected_revision": 7, "request_id": "wan-mtu-change", "config": changing}, timeout=20)
            wait(lambda: modules().get("service.internet", {}).get("generation", 0) > previous_ppp["generation"] and link() is not None and link()["mtu"] == 1452)
            assert (stage / "dhcp6.log").read_text().count("releases prefix ") == before_release, "PPP replacement released its DHCPv6 lease"
            if server.poll() is None:
                server.terminate()
                server.wait(timeout=3)
            wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
            ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
            server = dhcp6_start()
            ipv6_traffic()
            traffic()
            assert pd_record()["snapshot"]["client_duid"] == duid
            assert modules()["service.pd"]["generation"] > previous_pd["generation"]
            assert modules()["service.dns"]["pid"] == previous_dns["pid"] and modules()["service.dns"]["generation"] == previous_dns["generation"]
            dns_traffic()
            print("PASS PPP-only MTU reconfiguration retained the PD identity and restored the child", flush=True)
            # Operator restart uses the same control-owned dependency drain.
            before = modules()
            previous_ppp, previous_dns = before["service.internet"], before["service.dns"]
            previous_pd = pd_record()
            revision = api("control.sock", "/v1/config")["revision"]
            before_release = (stage / "dhcp6.log").read_text().count("releases prefix ")
            api("supervisor.sock", "/v1/modules/service.internet/restart", {}, status=404)
            assert modules()["service.internet"]["pid"] == previous_ppp["pid"]
            operator_restart_at = time.time()
            assert api("control.sock", "/v1/services/service.internet/restart", {}, timeout=12, status=202)["accepted"]
            wait(lambda: modules().get("service.internet", {}).get("generation", 0) > previous_ppp["generation"] and link() is not None)
            assert (stage / "dhcp6.log").read_text().count("releases prefix ") == before_release
            server.terminate()
            server.wait(timeout=3)
            wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
            ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
            server = dhcp6_start()
            ipv6_traffic()
            traffic()
            dns_traffic()
            assert api("control.sock", "/v1/config")["revision"] == revision
            assert modules()["service.dns"]["pid"] == previous_dns["pid"]
            assert pd_record()["snapshot"]["client_duid"] == duid
            assert pd_record()["generation"] > previous_pd["generation"]
            stale = {k: previous_pd[k] for k in ("supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot")}
            api("control.sock", "/v1/services/state", stale, status=409)
            print("PASS control-owned operator restart restored PD, rejected its old notification and preserved configuration/DNS", flush=True)
            before = modules()
            previous_pd = pd_record()
            before_release = (stage / "dhcp6.log").read_text().count("releases prefix ")
            control_restart_at = time.time()
            assert api("supervisor.sock", "/v1/modules/control/restart", {}, status=202)["accepted"]
            wait(lambda: modules()["control"]["generation"] > before["control"]["generation"] and modules()["control"]["ready"])
            wait(lambda: modules().get("service.internet", {}).get("generation", 0) > before["service.internet"]["generation"] and link() is not None)
            assert (stage / "dhcp6.log").read_text().count("releases prefix ") == before_release
            assert modules()["network"]["pid"] == before["network"]["pid"]
            assert modules()["management"]["pid"] == before["management"]["pid"]
            server.terminate()
            server.wait(timeout=3)
            wait(lambda: any(x["ifname"].startswith("ppp") and x["addr_info"] for x in peer_links()))
            ppp = next(x["ifname"] for x in peer_links() if x["ifname"].startswith("ppp"))
            server = dhcp6_start()
            ipv6_traffic()
            traffic()
            dns_traffic()
            assert api("control.sock", "/v1/config")["revision"] == revision
            assert pd_record()["snapshot"]["client_duid"] == duid
            stale = {k: previous_pd[k] for k in ("supervisor_instance", "name", "role", "config_digest", "generation", "producer_epoch", "sequence", "snapshot")}
            api("control.sock", "/v1/services/state", stale, status=409)
            print("PASS control restart withdrew old PD, retained writer/management, rejected the old producer and restored TCP/UDP/DNS", flush=True)
            # Request actual PID 1 poweroff while the independent server and
            # capture remain alive. Cleanup must not terminate the peer first.
            assert json.loads(run([str(stage.parent / "routerd"), "poweroff", "--runtime-dir", str(stage / "run")]))["accepted"]
            daemon.wait(timeout=12)
            # reboot(POWER_OFF) in a PID namespace terminates its init with
            # SIGINT; util-linux unshare propagates that signal (reboot(2)).
            assert daemon.returncode == -signal.SIGINT, daemon.returncode
            assert "appliance runtime failed" not in (stage / "console.log").read_text()
            capture.send_signal(signal.SIGINT)
            capture.wait(timeout=3)
            packets = run(["tcpdump", "-n", "-tt", "-vv", "-r", str(stage / "pppoe.pcap"), "ip6"])
            assert any("dhcp6 rebind" in line and recovery_at <= float(line.split()[0]) < recovery_reply_at for line in packets.splitlines() if re.match(r"^\d+\.\d+ ", line)), "no real recovery Rebind reached the paused ISC peer"
            withdrawals = []
            records = []
            for line in packets.splitlines():
                if line and line[0].isdigit():
                    records.append(line)
                elif records:
                    records[-1] += " " + line.strip()
            for line in records:
                if "router advertisement" in line and "router lifetime 0s" in line:
                    assert "valid time 0s, pref. time 0s" in line, line
                    withdrawals.append(float(line.split()[0]))
            assert not any("dhcp6 release" in line for line in records), "ordinary retirement released the PD lease"
            discovery = run(["tcpdump", "-n", "-tt", "-r", str(stage / "pppoe.pcap"), "pppoed"])
            teardowns = [float(line.split()[0]) for line in discovery.splitlines() if "PADT" in line]
            for operation, started in (("MTU change", wan_change_at), ("operator restart", operator_restart_at), ("control restart", control_restart_at)):
                teardown = next((stamp for stamp in teardowns if stamp >= started), None)
                assert teardown is not None and any(started <= stamp < teardown for stamp in withdrawals), f"{operation}: no RA withdrawal between request {started} and PPP teardown {teardown}; withdrawals={withdrawals}"
            assert len(withdrawals) >= 2, packets
            print("PASS local PD withdrawal before PPP teardown without Release; stable-DUID reacquisition restored IPv6 TCP/UDP", flush=True)
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
        dns_deadline = record()["snapshot"]["valid_until"]["nanos"]
        os.kill(pid1, signal.SIGSTOP)
        supervisor_frozen = True
        frozen = [saved["control"]["pid"], saved["service.internet"]["pid"]]
        for pid in frozen:
            signal_worker(pid, signal.SIGSTOP)
        wait(lambda: not selected(), 8)
        wait(lambda: time.clock_gettime_ns(time.CLOCK_BOOTTIME) >= dns_deadline, 8)
        assert routed() and link() is not None
        # The PPP session and its default route remain usable after PBR expiry.
        # A SERVFAIL therefore demonstrates DNS input expiry, not path loss.
        assert run(inside(ns, ["dig", "@192.0.2.1", "direct.ppp.example", "A", "+tries=1", "+time=2", "+short"])).strip() == "198.51.100.77"
        expired = run(inside(lan, ["dig", "@10.42.0.1", "expired.ppp.example", "A", "+tries=1", "+time=2", "+noall", "+comments"]))
        assert "status: SERVFAIL" in expired
        assert "expired.ppp.example" not in (stage / "dns.log").read_text()
        print("PASS original DNS deadline stopped upstream queries while peer DNS and PPP remained reachable", flush=True)
        print("PASS expired Health withdrew PBR while the live PPP session retained its default route", flush=True)
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
        assert "stale" in json.dumps(api("control.sock", "/v1/services/state", stale, status=409)).lower()
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
            print("client6routes", run(["ip", "-n", lan, "-j", "-6", "route", "show"]), flush=True)
            print("client6addresses", run(["ip", "-n", lan, "-j", "-6", "address", "show"]), flush=True)
        except (OSError, subprocess.SubprocessError, RuntimeError) as error:
            print("diagnostics unavailable:", error, flush=True)
        for name in ("console", "ac", "capture", "dhcp6", "http6", "udp6", "dns", "dns-tcp", "occupied-dns"):
            p = stage / (name + ".log")
            if p.exists():
                print(name, p.read_text(errors="replace")[-24000:], flush=True)
        raise
    finally:
        if keepalive_thread is not None:
            keepalive_stop.set()
            keepalive_thread.join(timeout=3)
        if supervisor_frozen:
            try:
                os.kill(pid1, signal.SIGCONT)
            except ProcessLookupError:
                pass
        for pid in frozen:
            try:
                signal_worker(pid, signal.SIGCONT)
            except subprocess.CalledProcessError:
                # The watchdog may already have retired this stopped child.
                # Cleanup still owns and terminates all trial namespaces below.
                pass
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
    parser.add_argument("--engine-dir", type=Path, default=Path("dist"), help="Normal release engine and source archives")
    parser.add_argument("--dhcp6-server", type=Path, help="Run PPP DHCPv6 PD against this independent ISC dhcpd executable")
    parser.add_argument("--update-binary-dir", type=Path, help="Exercise signed update/rollback to a different normal build")
    args = parser.parse_args()
    if args.inside:
        peer(args.inside, args.auth, args.dhcp6_server)
        return
    if args.update_binary_dir and not args.dhcp6_server:
        parser.error("runtime update trial requires the independent DHCPv6 peer")
    args.output.mkdir(parents=True, exist_ok=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-pppoe.XXXXXX"]).strip()
    try:
        for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle"):
            run(root + ["install", "-m", "500", str(args.binary_dir.resolve() / name), stage + "/" + name])
        run(root + ["install", "-m", "500", str(Path(__file__).resolve()), stage + "/peer.py"])
        key = stage + "/key.pem"
        run(root + ["openssl", "genpkey", "-algorithm", "ED25519", "-out", key])
        run(root + ["chmod", "600", key])
        project = Path(__file__).resolve().parents[1]
        packed = run(root + ["python3", str(project / "scripts/build-runtime-bundle.py"),
            "--boot", str(project / "images/boot.json"), "--binary-dir", str(args.binary_dir.resolve()),
            "--engine-dir", str(args.engine_dir.resolve()), "--licenses", str(project / "dist/licenses"),
            "--private-key-file", key, "--sequence", "1", "--output", stage + "/runtime.tar"], timeout=180)
        run(root + ["python3", "-c", "from pathlib import Path;import sys;Path(sys.argv[1]).write_text(sys.argv[2])", stage + "/runtime.json", packed])
        trust = run(root + [stage + "/routerd-bundle", "trust", "--private-key-file", key])
        run(root + ["python3", "-c", "from pathlib import Path;import sys;p=Path(sys.argv[1]);p.write_text(sys.argv[2]);p.chmod(0o600)", stage + "/trust.json", trust])
        if args.update_binary_dir:
            candidate = run(root + ["python3", str(project / "scripts/build-runtime-bundle.py"),
                "--boot", str(project / "images/boot.json"), "--binary-dir", str(args.update_binary_dir.resolve()),
                "--engine-dir", str(args.engine_dir.resolve()), "--licenses", str(project / "dist/licenses"),
                "--private-key-file", key, "--sequence", "2", "--output", stage + "/candidate.tar"], timeout=180)
            if json.loads(candidate)["manifest"]["release_id"] == json.loads(packed)["manifest"]["release_id"]:
                raise RuntimeError("runtime update trial requires different built release identities")
            run(root + ["python3", "-c", "from pathlib import Path;import sys;Path(sys.argv[1]).write_text(sys.argv[2])", stage + "/candidate.json", candidate])
        peer_args = []
        if args.dhcp6_server:
            run(root + ["install", "-m", "500", os.path.abspath(args.dhcp6_server), stage + "/dhcpd"])
            peer_args += ["--dhcp6-server", stage + "/dhcpd"]
        for auth in ((args.auth,) if args.auth else ("pap", "chap")):
            directory = stage + "/" + auth
            run(root + ["mkdir", "-m", "700", directory])
            with (args.output / (auth + ".log")).open("w") as output:
                result = subprocess.run(root + ["unshare", "--mount", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", directory, "--auth", auth] + peer_args, stdout=output, stderr=subprocess.STDOUT, timeout=630 if args.update_binary_dir else 200)
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
