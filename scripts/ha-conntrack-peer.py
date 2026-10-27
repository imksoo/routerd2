#!/usr/bin/env python3
"""Check two actual routers preserving one TCP socket and UDP NAT mapping.

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


def run(command):
    return subprocess.run(command, check=True, capture_output=True, text=True, timeout=10).stdout


def peer(stage):
    def deadline(*_):
        raise TimeoutError("HA connection-state communication deadline")
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(100)
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

    def wait(condition):
        until = time.monotonic() + 15
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
        roles = ("logging", "network", "control", "management", "vrrp")
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
                {"id": "wan-identity", "when": {"fact": "service.ha.elected"}, "link": {"name": "wan-ha", "kind": "macvlan", "parent": "wan0", "hardware_addr": "02:00:5e:00:01:43", "ipv6_addr_gen_mode": "none"}},
                {"id": "wan-interface", "interface": {"name": "wan-ha"}},
                {"id": "wan-address", "address": {"interface": "wan-ha", "cidr": "203.0.113.1/24"}},
                {"id": "firewall", "firewall": {"forward_chain": {"policy": "accept"}, "nat": [{"type": "snat", "match": {"family": 4, "source": "198.18.0.0/24", "out_interface": "wan-ha"}, "address": "203.0.113.1"}]}},
            ]
            # Select the shared WAN identity rather than the physical-port
            # connected route installed by the product's physical-port intent.
            resources.append({"id": "wan-route", "route": {"destination": "203.0.113.50/32", "interface": "wan-ha"}})
            (directory / "config.json").write_text(json.dumps({"schema_version": 2, "resources": resources}))
            workers = {role: str(stage / ("routerd-logging" if role == "logging" else "routerd-modules")) for role in ("logging", "network", "control", "management", "vrrp")}
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
        start("ha-b", ["unshare", "--pid", "--fork", "--mount-proc", "--kill-child=SIGTERM", str(stage / "routerd"), "boot", "--file", str(stage / "b/boot.json")], "b")
        wait(lambda: (stage / "b/run/supervisor.sock").exists())
        wait(lambda: modules("b").get("service.ha", {}).get("ready") is True)
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
        print("PASS continuous actual VRRP/NAT TCP and UDP; tcp_loose=0", flush=True)
        for previous, owner in (("a", "b"), ("b", "a")):
            before = modules(previous)["service.ha"]["generation"]
            api(previous, "control.sock", "/v1/services/service.ha/restart", {})
            wait(lambda: active(owner))
            wait(lambda: modules(previous).get("service.ha", {}).get("generation", 0) > before and modules(previous).get("service.ha", {}).get("ready"))
            sequence = latest()["sequence"]
            assert progress(sequence) == mapping, "existing connection mapping changed during takeover"
            print("PASS continuous same TCP socket and UDP mapping after " + previous + " -> " + owner + " takeover and worker/engine restart", flush=True)
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
        print("PASS captures: one TCP SYN, IPv4 link-local FTFW replication, unchanged UDP NAT tuple", flush=True)
        print("PASS two-way state handoff without TCP reconnect", flush=True)
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
    args = parser.parse_args()
    if args.inside:
        peer(args.inside)
        return
    os.chdir(Path(__file__).resolve().parent.parent)
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    for module in ("nf_conntrack", "nf_conntrack_netlink"):
        run(root + ["modprobe", module])
    stage = run(root + ["mktemp", "-d", "/tmp/routerd-ha.XXXXXX"]).strip()
    try:
        for source, name in [(Path(__file__), "peer.py"), (Path("dist/conntrack-engine.tar"), "conntrack-engine.tar"), (Path("dist/conntrack-sources.tar"), "conntrack-sources.tar"), *[(Path("dist/bin") / name, name) for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle")]]:
            run(root + ["install", "-m", "500", str(source.resolve()), stage + "/" + name])
        result = subprocess.run(root + ["unshare", "--mount", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", stage], timeout=120)
        if result.returncode:
            for name in ("a.log", "b.log", "client.log", "server.log"):
                log = subprocess.run(root + ["tail", "-30", stage + "/" + name], capture_output=True, text=True)
                print(name + ":\n" + log.stdout)
            raise RuntimeError("HA communication failed")
    finally:
        run(root + ["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
