#!/usr/bin/env python3
"""Run routerd2 against an independent RP-PPPoE AC in private mount/network namespaces.

Requires Linux root/sudo, pppd 2.4.9 + rp-pppoe.so, pppoe-server, iproute2,
curl, tcpdump and ping. Host PPP files, hooks and routes are not used.
"""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import shutil
import stat
import time


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


def peer(stage, auth, engine_archive):
    # This whole fixture has a private mount namespace. The product engine
    # must additionally create its own: AC secrets/hooks are not client input.
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(180)
    signal.signal(signal.SIGTERM, terminated)
    ns, acns, lan = (prefix + str(os.getpid()) for prefix in ("r2ppp-", "r2ac-", "r2lan-"))
    children, namespaces, frozen = [], [], []
    capture = None

    def inside(name, args):
        return ["ip", "netns", "exec", name] + args

    def start(name, args, log):
        with (stage / (log + ".log")).open("a") as output:
            p = subprocess.Popen(inside(name, args), stdout=output, stderr=subprocess.STDOUT)
        children.append(p)
        return p

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
        return any(r.get("dev") == "ppp-wan" and r.get("dst") == "default" for r in json.loads(run(["ip", "-n", ns, "-j", "route", "show"])))

    def traffic():
        assert run(inside(lan, ["curl", "--noproxy", "*", "--max-time", "2", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "http://198.51.100.20:18080/"])) == "200"
        code = 'import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.settimeout(2);s.connect(("198.51.100.20",18081));s.send(b"pppoe");assert s.recv(100)==b"pppoe"'
        run(inside(lan, ["python3", "-c", code]))

    def ac_start():
        return start(acns, ["/usr/sbin/pppoe-server", "-F", "-k", "-I", "server0", "-C", "ROUTERD2-AC", "-S", "ROUTERD2-LAB", "-L", "192.0.2.1", "-R", "192.0.2.2", "-N", "1", "-O", str(stage / "server.conf"), "-X", str(stage / "ac.pid")], "ac")

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
        service = dict(interface="wan0", ppp_interface="ppp-wan", username="routerd2", password_file=str(stage / "password"), auth_method=auth, ipv6=auth == "chap", service_name="ROUTERD2-LAB", ac_name="ROUTERD2-AC", mtu=1454, mru=1454, lcp_echo_interval=1, lcp_echo_failure=3, metric=20)
        resources = [
            {"id": "wan", "interface": {"name": "wan0"}},
            {"id": "lan", "interface": {"name": "lan0"}},
            {"id": "lan-address", "address": {"interface": "lan0", "cidr": "10.42.0.1/24"}},
            {"id": "forwarding", "forwarding": {"ipv4": True}},
            {"id": "firewall", "firewall": {"forward": [{"from": "lan0", "to": "ppp-wan"}], "masquerade": [{"source": "10.42.0.0/24", "out_interface": "ppp-wan"}]}},
            {"id": "internet", "service": {"name": "internet", "pppoe": service}},
        ]
        (stage / "config.json").write_text(json.dumps({"schema_version": 2, "resources": resources}))
        boot = dict(platform="host", config=str(stage / "config.json"), runtime_dir=str(stage / "run"), state_dir=str(stage / "state"), ssh={"user": "admin"}, workers={r: str(stage.parent / ("routerd-logging" if r == "logging" else "routerd-modules")) for r in ["logging", "network", "control", "management", "pppoe"]})
        (stage / "boot.json").write_text(json.dumps(boot))
        (stage / "server.conf").write_text("auth\nrequire-" + auth + "\nrefuse-" + ("chap" if auth == "pap" else "pap") + "\nmtu 1454\nmru 1454\nms-dns 203.0.113.53\nlogfd 1\nhide-password\nlcp-echo-interval 1\nlcp-echo-failure 3\n")
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
        capture = start(ns, ["tcpdump", "-Z", "root", "--immediate-mode", "-U", "-n", "-i", "any", "-w", str(stage / "pppoe.pcap")], "capture")
        ac = ac_start()
        command = [str(stage.parent / "routerd"), "boot", "--file", str(stage / "boot.json")]
        if engine_archive:
            # Exercise a real alternate distribution root, including its ELF
            # loader/libraries/plugin, without installing it into the host.
            profile = stage.parent / ("engine-" + auth)
            profile.mkdir()
            run(["tar", "--no-same-owner", "-xf", str(engine_archive), "-C", str(profile)])
            run(["mount", "--bind", str(profile), str(profile)])
            for name in ("routerd", "routerd-modules", "routerd-logging"):
                target = profile / str(stage.parent / name).lstrip("/")
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(stage.parent / name, target)
                target.chmod(0o500)
            # Only this case's config/runtime/state crosses the chroot.
            target = profile / str(stage).lstrip("/")
            target.mkdir(parents=True, exist_ok=True)
            run(["mount", "--bind", str(stage), str(target)])
            run(["mount", "--bind", "/proc", str(profile / "proc")])
            run(["mount", "--bind", str(root_options), str(profile / "root")])
            for name, major, minor in (("null", 1, 3), ("ppp", 108, 0), ("urandom", 1, 9)):
                path = profile / "dev" / name
                if not path.exists():
                    os.mknod(path, stat.S_IFCHR | 0o600, os.makedev(major, minor))
            command = ["python3", "-I", "-c", "import os,sys;os.chroot(sys.argv[1]);os.chdir('/');os.execv(sys.argv[2],sys.argv[2:])", str(profile)] + command
        daemon = start(ns, command, "console")
        wait(lambda: link() is not None and routed())
        start(acns, ["python3", "-u", "-m", "http.server", "18080", "--bind", "198.51.100.20", "--directory", str(etc)], "http")
        start(acns, ["python3", "-u", "-c", 'import socket\ns=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(("198.51.100.20",18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)'], "udp")
        wait(lambda: ":18080 " in run(inside(acns, ["ss", "-lnt"])))
        traffic()
        initial = record()
        assert set(initial["snapshot"]["dns_servers"]) == {"203.0.113.53"}
        assert all(x["generation"] == 1 for x in modules().values())
        assert link()["mtu"] == 1454
        print("PASS native " + auth + ": negotiated DNS, LAN TCP/UDP, MTU 1454, generation 1", flush=True)
        if auth == "chap":
            ll = next(a["address"] for a in link()["addr_info"] if a["family"] == "inet6")
            run(inside(ns, ["ping", "-6", "-I", "ppp-wan", "-c", "1", "-W", "2", ll]))
            print("PASS actual IPv6CP peer link-local ping", flush=True)
        saved = modules()
        frozen = [saved["control"]["pid"], saved["service.internet"]["pid"]]
        for pid in frozen:
            os.kill(pid, signal.SIGSTOP)
        wait(lambda: not routed(), 8)
        assert link() is not None
        print("PASS original observation expiry withdrew writer route with live PPP and frozen producers", flush=True)
        for pid in frozen:
            os.kill(pid, signal.SIGCONT)
        frozen = []
        wait(routed)
        traffic()
        previous, old_index = modules()["service.internet"], link()["ifindex"]
        os.kill(previous["pid"], signal.SIGKILL)
        wait(lambda: modules()["service.internet"]["generation"] > previous["generation"] and routed() and link() is not None and link()["ifindex"] != old_index, 35)
        traffic()
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
        wait(routed, 40)
        traffic()
        print("PASS independent AC restart restored LAN TCP/UDP", flush=True)
    except BaseException:
        for name in ("console", "ac", "capture"):
            p = stage / (name + ".log")
            if p.exists():
                print(name, p.read_text(), flush=True)
        raise
    finally:
        for pid in frozen:
            os.kill(pid, signal.SIGCONT)
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
    parser.add_argument("--engine-archive", type=Path, help="Docker-exported distribution with pppd and its actual dependencies")
    args = parser.parse_args()
    if args.inside:
        peer(args.inside, args.auth, args.engine_archive)
        return
    args.output.mkdir(parents=True, exist_ok=True)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-pppoe.XXXXXX"]).strip()
    try:
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            run(root + ["install", "-m", "500", str(args.binary_dir.resolve() / name), stage + "/" + name])
        run(root + ["install", "-m", "500", str(Path(__file__).resolve()), stage + "/peer.py"])
        profile_args = []
        if args.engine_archive:
            run(root + ["install", "-m", "400", str(args.engine_archive.resolve()), stage + "/engine.tar"])
            profile_args = ["--engine-archive", stage + "/engine.tar"]
        for auth in ((args.auth,) if args.auth else ("pap", "chap")):
            directory = stage + "/" + auth
            run(root + ["mkdir", "-m", "700", directory])
            with (args.output / (auth + ".log")).open("w") as output:
                result = subprocess.run(root + ["unshare", "--mount", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", directory, "--auth", auth] + profile_args, stdout=output, stderr=subprocess.STDOUT, timeout=200)
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
