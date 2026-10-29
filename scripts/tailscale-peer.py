#!/usr/bin/env python3
"""Exercise the product router with isolated Headscale and an official peer.

Requires Linux root/sudo, TUN, iproute2, nft, curl, OpenSSL, tcpdump, dig, and pinned
Headscale/Tailscale peers below dist/peer-env/tailscale. No host network or DNS
settings are used. Peer credentials, logs and captures are temporary.
"""
import copy
import json
import ipaddress
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import tempfile
import time

PEERS = Path(__file__).resolve().parents[1] / "dist/peer-env/tailscale"
PRODUCT = Path(__file__).resolve().parents[1] / "dist/bin"


def run(args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=20)
    if result.returncode:
        raise RuntimeError(Path(args[0]).name + ": " + result.stderr.strip()[:2000])
    return result.stdout


def inside(namespace, args):
    return ["ip", "netns", "exec", namespace] + args


def wait(condition, seconds=20):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if condition():
            return
        time.sleep(0.1)
    raise RuntimeError("actual Tailscale communication condition timed out")


def vendor_api(socket, path, body=None):
    args = ["curl", "--max-time", "3", "--fail-with-body", "-sS", "--unix-socket", str(socket), "http://local-tailscaled.sock/localapi/v0/" + path]
    if body is not None:
        args += ["-X", "POST", "-H", "Content-Type: application/json", "--data-binary", json.dumps(body)]
    data = run(args)
    return json.loads(data) if data else None


ECHO_SERVER = r"""import json,socket,sys,threading
host=sys.argv[1];family=socket.AF_INET6 if ':' in host else socket.AF_INET
t=socket.socket(family,socket.SOCK_STREAM);t.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);t.bind((host,18080));t.listen()
def tcp():
 while True:
  c,a=t.accept();body=c.recv(1024);c.sendall(json.dumps({'source':a[0],'payload':body.decode()}).encode());c.close()
threading.Thread(target=tcp,daemon=True).start()
u=socket.socket(family,socket.SOCK_DGRAM);u.bind((host,18081));print("READY",flush=True)
while True:
 body,a=u.recvfrom(4096);u.sendto(json.dumps({'source':a[0],'payload':body.decode()}).encode(),a)
"""
ECHO_CLIENT = r"""import ipaddress,json,socket,sys
host,source=sys.argv[1:];family=socket.AF_INET6 if ':' in host else socket.AF_INET
for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
 s=socket.socket(family,kind);s.settimeout(2)
 try:
  s.connect((host,port));s.sendall(b'actual-network');data=json.loads(s.recv(4096))
 except OSError as error:
  raise RuntimeError(f'{"TCP" if kind==socket.SOCK_STREAM else "UDP"} {source} -> {host}:{port}: {error}') from error
 assert data['payload']=='actual-network',data
 assert ipaddress.ip_address(data['source'])==ipaddress.ip_address(source),data
 s.close()
"""


BLOCKED_CLIENT = r"""import socket,sys
host=sys.argv[1];family=socket.AF_INET6 if ':' in host else socket.AF_INET
for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
 s=socket.socket(family,kind);s.settimeout(1)
 try:
  s.connect((host,port));s.sendall(b'blocked-connection');s.recv(4096)
 except socket.timeout:pass
 else:raise AssertionError('shields-up accepted a new inbound connection')
 finally:s.close()
"""


def peer(stage):
    children, namespaces = [], []
    frozen = []
    pid1 = 0
    module = ""

    def signal_worker(pid, sig):
        run(["nsenter", "-t", str(pid1), "-m", "-p", "python3", "-c", "import os,sys;os.kill(int(sys.argv[1]),int(sys.argv[2]))", str(pid), str(int(sig))])

    def start(namespace, args, name):
        with (stage / (name + ".log")).open("a") as output:
            process = subprocess.Popen(inside(namespace, args), stdout=output, stderr=subprocess.STDOUT)
        children.append(process)
        return process

    def product_api(socket, path, body=None, timeout=3):
        args = ["curl", "--max-time", str(timeout), "-fsS", "--unix-socket", str(stage / "product-run" / socket), "http://localhost" + path]
        if body is not None:
            args += ["-X", "POST", "-H", "Content-Type: application/json", "--data-binary", json.dumps(body)]
        return json.loads(run(args))

    def modules():
        return {x["name"]: x for x in product_api("supervisor.sock", "/v1/status")["modules"]}

    def control_io(control):
        raw = Path(f"/proc/{pid1}/root/proc/{control}/io").read_text()
        return {k: int(v) for k, v in (line.split(":") for line in raw.splitlines()) if k in ("rchar", "syscr")}

    def observation():
        instances = product_api("control.sock", "/v1/services")["instances"]
        return next((x.get("snapshot", {}) for x in instances if x["name"] == module), {})

    def pingable(host):
        return subprocess.run(inside("ts-b", ["ping", "-c", "1", "-W", "1", host]), capture_output=True).returncode == 0

    def traffic(host, source, namespace="ts-b"):
        run(inside(namespace, ["python3", "-c", ECHO_CLIENT, host, source]))

    def resolve_peer():
        for transport in ("+notcp", "+tcp"):
            for record, family in (("A", 4), ("AAAA", 6)):
                result = subprocess.run(inside("ts-lan", ["dig", "@10.42.0.1", "independent-peer.test.invalid", record, transport, "+noall", "+answer", "+comments", "+tries=1", "+time=2"]), capture_output=True, text=True, timeout=4)
                if result.returncode or peer_ips[family] not in result.stdout.split():
                    print("DNS response:", record, transport, result.stdout, flush=True)
                    print("DNS peer name:", vendor_api(socket, "status").get("Self", {}).get("DNSName"), flush=True)
                    print("DNS input:", dns_input(), flush=True)
                    print("DNS status:", product_api("services/resolver.dns.sock", "/v1/status"), flush=True)
                    print("DNS routing:", run(inside("ts-a", ["ip", "route", "get", "100.100.100.100"])), flush=True)
                assert result.returncode == 0 and peer_ips[family] in result.stdout.split(), (result.stdout, result.stderr)

    def dns_input():
        return product_api("control.sock", "/v1/services/service.resolver/dns-input")["upstreams"] or []

    def dns_ready():
        upstreams = product_api("services/resolver.dns.sock", "/v1/status")["resources"][0]["observed"].get("upstreams", "")
        return "100.100.100.100" in upstreams and "Expired" not in upstreams

    def dns_unavailable():
        result = run(inside("ts-lan", ["dig", "@10.42.0.1", "independent-peer.test.invalid", "A", "+noall", "+answer", "+comments", "+tries=1", "+time=2"]))
        assert "status: SERVFAIL" in result and peer_ips[4] not in result, result
        # The independent access-network DNS stays usable with no tailnet.
        for transport in ("+notcp", "+tcp"):
            result = run(inside("ts-lan", ["dig", "@10.42.0.1", "control.test", "A", transport, "+short", "+tries=1", "+time=2"]))
            assert "192.0.2.1" in result.splitlines(), result
        assert "independent-peer.test.invalid" not in (stage / "bootstrap-dns.log").read_text()

    try:
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/etc"])
        Path("/etc/resolv.conf").write_text("nameserver 192.0.2.1\n")
        Path("/etc/passwd").write_text("root:x:0:0:root:/root:/bin/sh\n")
        Path("/etc/group").write_text("root:x:0:\n")
        Path("/etc/hosts").write_text("127.0.0.1 localhost\n::1 localhost\n")
        Path("/run/netns").mkdir(parents=True)
        run(["mount", "-t", "tmpfs", "-o", "mode=700", "tmpfs", "/run/netns"])
        run(["ip", "link", "add", "underlay", "type", "bridge"])
        run(["ip", "link", "set", "underlay", "up"])
        for i, namespace in enumerate(("ts-control", "ts-a", "ts-b"), 1):
            run(["ip", "netns", "add", namespace])
            namespaces.append(namespace)
            run(["ip", "link", "add", "port" + str(i), "type", "veth", "peer", "name", "uplink0"])
            run(["ip", "link", "set", "port" + str(i), "master", "underlay"])
            run(["ip", "link", "set", "port" + str(i), "up"])
            run(["ip", "link", "set", "uplink0", "netns", namespace])
            run(inside(namespace, ["ip", "link", "set", "lo", "up"]))
            run(inside(namespace, ["ip", "link", "set", "uplink0", "up"]))
            if namespace != "ts-a":
                run(inside(namespace, ["ip", "addr", "add", "192.0.2." + str(i) + "/24", "dev", "uplink0"]))
        for address, device in (("fd0::1/64", "uplink0"), ("198.51.100.20/32", "lo"), ("2001:db8:ffff::20/128", "lo")):
            run(inside("ts-control", ["ip", "addr", "add", address, "dev", device]))
        cert, key = stage / "cert.pem", stage / "key.pem"
        run(["openssl", "req", "-config", "/dev/null", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-subj", "/CN=routerd-fixture", "-addext", "subjectAltName=IP:192.0.2.1,DNS:control.test"])
        access = stage / "policy.json"
        access.write_text(json.dumps({"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}],
            "nodeAttrs": [{"target": ["*"], "attr": ["magicdns-aaaa"]}], "tagOwners": {"tag:router": ["fixture@"]}}))
        config = {
            "policy": {"mode": "file", "path": str(access)},
            "server_url": "https://control.test:8443", "listen_addr": "192.0.2.1:8443",
            "noise": {"private_key_path": str(stage / "noise.key")},
            "prefixes": {"v4": "100.64.0.0/10", "v6": "fd7a:115c:a1e0::/48"},
            "database": {"type": "sqlite", "sqlite": {"path": str(stage / "db.sqlite")}},
            "derp": {"urls": [], "paths": [], "auto_update_enabled": False, "server": {
                "enabled": True, "region_id": 999, "region_code": "fixture", "region_name": "isolated DERP",
                "stun_listen_addr": "0.0.0.0:3478", "private_key_path": str(stage / "derp.key"),
                "automatically_add_embedded_derp_region": True, "ipv4": "192.0.2.1"}},
            "dns": {"override_local_dns": False, "magic_dns": True, "base_domain": "test.invalid"},
            "unix_socket": str(stage / "headscale.sock"), "tls_cert_path": str(cert), "tls_key_path": str(key),
            "disable_check_updates": True, "log": {"level": "warn"},
        }
        start("ts-control", ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-resolv", "--no-hosts", "--user=root", "--bind-interfaces", "--listen-address=192.0.2.1", "--address=/control.test/192.0.2.1", "--log-queries", "--log-facility=-"], "bootstrap-dns")
        cfg = stage / "headscale.json"
        cfg.write_text(json.dumps(config))
        head = [str(PEERS / "headscale"), "-c", str(cfg)]
        start("ts-control", head + ["serve"], "control")
        wait(lambda: (stage / "headscale.sock").exists())
        user = json.loads(run(inside("ts-control", head + ["users", "create", "fixture", "-o", "json"])))
        auth = json.loads(run(inside("ts-control", head + ["preauthkeys", "create", "--user", str(user["id"]), "--reusable", "-o", "json"])))
        run(["ip", "netns", "add", "ts-lan"])
        namespaces.append("ts-lan")
        run(inside("ts-lan", ["ip", "link", "set", "lo", "up"]))
        run(["ip", "link", "add", "lan0", "netns", "ts-a", "type", "veth", "peer", "name", "client0", "netns", "ts-lan"])
        run(inside("ts-lan", ["ip", "link", "set", "client0", "up"]))
        for address in ("10.42.0.2/24", "fd42::2/64"):
            run(inside("ts-lan", ["ip", "addr", "add", address, "dev", "client0"]))
        run(inside("ts-lan", ["ip", "route", "add", "default", "via", "10.42.0.1"]))
        run(inside("ts-lan", ["ip", "-6", "route", "add", "default", "via", "fd42::1"]))
        for name in ("product-run", "product-state"):
            (stage / name).mkdir(mode=0o700)
        authfile = stage / "auth.key"
        authfile.write_text(auth["key"])
        authfile.chmod(0o600)
        # Start from an independently enrolled external daemon's real state.
        # Stop its old network ownership before provisioning the native owner.
        seed = stage / "external-daemon"
        seed.mkdir(mode=0o700)
        seed_socket = seed / "local.sock"
        run(inside("ts-a", ["ip", "addr", "add", "192.0.2.2/24", "dev", "uplink0"]))
        external = start("ts-a", ["env", "TS_LOGS_DIR=" + str(seed), "SSL_CERT_FILE=" + str(cert), str(PEERS / "tailscaled"), "--state=" + str(seed / "tailscaled.state"), "--statedir=" + str(seed), "--socket=" + str(seed_socket), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "external-daemon")
        wait(seed_socket.exists)
        run(inside("ts-a", ["env", "SSL_CERT_FILE=" + str(cert), str(PEERS/"tailscale"), "--socket="+str(seed_socket), "up", "--timeout=15s", "--login-server=https://control.test:8443", "--hostname=product-a", "--auth-key=file:"+str(authfile), "--accept-dns=false", "--netfilter-mode=off", "--advertise-exit-node", "--advertise-routes=10.42.0.0/24,fd42::/64"]))
        wait(lambda: vendor_api(seed_socket,"status").get("BackendState")=="Running",30)
        seed_identity = vendor_api(seed_socket,"status")["Self"]
        external.terminate()
        external.wait(timeout=5)
        run(inside("ts-a", ["ip", "addr", "del", "192.0.2.2/24", "dev", "uplink0"]))
        target = stage / "product-state/services/tailscale"
        target.mkdir(mode=0o700, parents=True)
        shutil.copyfile(seed / "tailscaled.state", target / "tailscaled.state")
        (target / "tailscaled.state").chmod(0o600)
        legacy = PRODUCT.parents[1] / "internal/integration/testdata/tailscale-import.yaml"
        policyfile = stage / "import-policy.json"
        policy = {"schema_version": 1, "tailscale": {"TailscaleNode/overlay": {
            "interface": "tailscale0", "bootstrap_dns": ["192.0.2.1"], "control_ca_file": str(cert),
            "enrollment": "key_file", "auth_key_file": str(authfile)}}}
        def imported_native():
            policyfile.write_text(json.dumps(policy))
            file = stage / "imported.yaml"
            file.write_text(run([str(PRODUCT / "routerd"), "config", "migrate", "--policy", str(policyfile), str(legacy)]))
            plan = json.loads(run([str(PRODUCT / "routerd"), "config", "plan", str(file)]))
            return plan["native"]
        migrated = imported_native()
        imported = next(s for s in migrated["services"] if "tailscale" in s)
        firewall = migrated["network"]["firewall"]
        # New LAN DNS listener: authorize only its declared transport/port.
        firewall["input"]["rules"] += [{"match": {"in_interface": "lan0", "protocol": protocol, "destination_ports": [{"start": 53}]}, "action": "accept"} for protocol in ("udp", "tcp")]
        firewall["input"]["rules"] += [{"match": {"in_interface": "tailscale0", "protocol": protocol, "destination_ports": [{"start": port}]}, "action": "accept"} for protocol, port in (("tcp", 18080), ("udp", 18081))]
        firewall["forward"] = [{"from": "tailscale0", "to": "lan0"}, {"from": "tailscale0", "to": "uplink0"}, {"from": "lan0", "to": "tailscale0"}]
        module = "service." + imported["name"]
        resources = [
            {"id": "uplink", "interface": next(i for i in migrated["network"]["interfaces"] if i["name"] == "uplink0")},
            {"id": "lan", "interface": {"name": "lan0"}},
            {"id": "forwarding", "forwarding": {"ipv4": True, "ipv6": True, "ipv4_rp_filter": 0}},
            {"id": "firewall", "firewall": firewall},
            {"id": "exit-nat", "nat": [{"type": "masquerade", "match": {"family": family, "in_interface": "tailscale0", "out_interface": "uplink0"}} for family in (4, 6)]},
            {"id": "gate", "service": {"name": "gate", "health": {"interface": "uplink0", "target": "192.0.2.1", "protocol": "tcp", "port": 8443, "interval_ms": 200, "timeout_ms": 100, "healthy_threshold": 1, "unhealthy_threshold": 1}}},
            {"id": "tailnet", "when": {"fact": "service.gate.active", "max_age": "2s"}, "service": imported},
            {"id": "resolver", "service": {"name": "resolver", "dns": {"resource": "resolver", "spec": {
                "listen": [{"name": "lan", "addresses": ["10.42.0.1"], "port": 53}],
                "sources": [{"name": "tailnet", "kind": "forward", "match": ["test.invalid"], "viaInterface": "tailscale0",
                    "upstreamFrom": [{"resource": "TailscaleNode/" + imported["name"], "field": "dnsServers"}]},
                    {"name": "access", "kind": "forward", "match": ["."], "viaInterface": "uplink0", "upstreams": ["udp://192.0.2.1:53"]}]}}}},
            {"id": "exit4", "route": {"destination": "198.51.100.20/32", "interface": "uplink0", "gateway": "192.0.2.1"}},
            {"id": "exit6", "route": {"destination": "2001:db8:ffff::20/128", "interface": "uplink0", "gateway": "fd0::1"}},
        ]
        for i, (interface, address) in enumerate((("uplink0", "192.0.2.2/24"), ("uplink0", "fd0::2/64"), ("lan0", "10.42.0.1/24"), ("lan0", "fd42::1/64"))):
            resources.append({"id": "address" + str(i), "address": {"interface": interface, "cidr": address}})
        # WAN source policies follow upstream's lookup5270, preserving the
        # subnet client's return path to tailnet peers in both address families.
        for family, prefix, gateway in ((4, "0.0.0.0/0", "192.0.2.1"), (6, "::/0", "fd0::1")):
            resources += [
                {"id": "wan-default" + str(family), "route": {"destination": prefix, "interface": "uplink0", "gateway": gateway, "table": 100}},
                {"id": "wan-policy" + str(family), "policy_rule": {"family": family, "priority": 6000, "table": 100, "in_interface": "lan0"}},
            ]
        document = {"schema_version": 2, "resources": resources}
        configfile = stage / "product.json"
        configfile.write_text(json.dumps(document))
        boot = {"platform": "host", "config": str(configfile), "runtime_dir": str(stage / "product-run"), "state_dir": str(stage / "product-state"), "ssh": {"user": "admin"},
                "workers": {role: str(PRODUCT / ("routerd-logging" if role == "logging" else "routerd-modules")) for role in ("logging", "network", "control", "management", "health", "tailscale", "dns")},
                "engines": {"tailscale": str(PRODUCT.parent / "tailscale-engine.tar")}}
        # Exercise the signed catalog's actual role/image/source binding. These
        # private input copies and extracted runtime are removed with the stage.
        inputs = stage / "release-input"
        inputs.mkdir(mode=0o700)
        for name in ("routerd", "routerd-modules", "routerd-logging", "routerd-bundle"):
            shutil.copyfile(PRODUCT / name, inputs / name)
            (inputs / name).chmod(0o755)
        for name in ("tailscale-engine.tar", "tailscale-sources.tar"):
            shutil.copyfile(PRODUCT.parent / name, inputs / name)
            (inputs / name).chmod(0o444)
        signing = stage / "signing.pem"
        run(["openssl", "genpkey", "-algorithm", "ED25519", "-out", str(signing)])
        signing.chmod(0o600)
        publisher = str(inputs / "routerd-bundle")
        trust = stage / "trust.json"
        trust.write_text(run([publisher, "trust", "--private-key-file", str(signing)]))
        trust.chmod(0o600)
        layout = {"boot_abi": 1, "worker_abi": 5, "entrypoint": "routerd",
            "roles": [{"role": role, "artifact": "routerd-logging" if role == "logging" else "routerd-modules"} for role in boot["workers"]],
            "engines": [{"role": "tailscale", "artifact": "tailscale-image", "sources": "tailscale-sources"}],
            "artifacts": [{"name": name, "kind": "native", "command": name, "file": str(inputs / name)} for name in ("routerd", "routerd-modules", "routerd-logging")] +
                [{"name": "tailscale-image", "kind": "data", "file": str(inputs / "tailscale-engine.tar")}, {"name": "tailscale-sources", "kind": "data", "file": str(inputs / "tailscale-sources.tar")}]}
        layoutfile = stage / "layout.json"
        layoutfile.write_text(json.dumps(layout))
        version = json.loads(run([str(inputs / "routerd"), "version"]))
        bundle = stage / "bundle.tar"
        packed = json.loads(run([publisher, "pack", "--private-key-file", str(signing), "--layout", str(layoutfile), "--release-id", version["release_id"], "--sequence", "1", "--target-arch", "amd64", "--output", str(bundle)]))
        selector = ["--root", str(stage / "releases"), "--trust-file", str(trust), "--target-arch", "amd64"]
        run([str(inputs / "routerd"), "release", "stage", *selector, "--file", str(bundle)])
        run([str(inputs / "routerd"), "release", "initialize", *selector, "--digest", packed["digest"]])
        boot.update(platform="appliance", workers={}, engines={}, release={"root": str(stage / "releases"), "trust_file": str(trust)})
        bootfile = stage / "boot.json"
        bootfile.write_text(json.dumps(boot))
        capture = start("ts-a", ["tcpdump", "-Z", "root", "--immediate-mode", "-U", "-n", "-i", "any", "-w", str(stage / "tailnet.pcap")], "capture")
        command = ["unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child=TERM", str(inputs / "routerd"), "boot", "--file", str(bootfile)]
        startup_start = time.monotonic()
        daemon = start("ts-a", command, "product")
        child_file = Path(f"/proc/{daemon.pid}/task/{daemon.pid}/children")
        wait(lambda: child_file.read_text().strip())
        pid1 = int(child_file.read_text().split()[0])
        wait(lambda: (stage / "product-run/control.sock").exists())
        socket_seconds = round(time.monotonic()-startup_start, 3)
        wait(lambda: observation().get("backend_state") == "Running", 35)
        assert product_api("supervisor.sock", "/v1/status")["pid"] == 1
        print("PASS signed engine/image/source catalog selected under PID 1", flush=True)
        original = observation()
        print("CONTROL startup:", socket_seconds, control_io(modules()["control"]["pid"]), flush=True)
        assert original["online"]
        assert original["dns_name"] == "product-a.test.invalid."
        assert original["tailnet"]["dns_domain"] == "test.invalid" and original["tailnet"]["magic_dns_enabled"]
        assert original["node_id"]==seed_identity["ID"] and original["addresses"]==seed_identity["TailscaleIPs"]
        print("PASS externally enrolled official daemon identity retained after stopped-state provisioning",flush=True)
        # The kernel scope has one owner even across different identity dirs.
        lock_probe = "import errno,socket\ns=socket.socket(socket.AF_UNIX,socket.SOCK_DGRAM)\ntry:s.bind('\\0routerd2.tailscale-owner')\nexcept OSError as e:assert e.errno==errno.EADDRINUSE\nelse:raise AssertionError('Tailscale kernel namespace was not claimed')"
        run(inside("ts-a",["python3","-c",lock_probe]))
        print("PASS product daemon joined private Headscale", original["addresses"], flush=True)
        base = stage / "peer"
        base.mkdir(mode=0o700)
        bootstrap = base / "bootstrap.json"
        bootstrap.write_text(json.dumps({"version": "alpha0", "locked": False, "enabled": False}))
        socket = base / "tailscaled.sock"
        start("ts-b", ["env", "TS_DEBUG_FIREWALL_MODE=nftables", "TS_LOGS_DIR=" + str(base), "SSL_CERT_FILE=" + str(cert), str(PEERS / "tailscaled"), "--state=" + str(base / "state"), "--statedir=" + str(base), "--socket=" + str(socket), "--tun=tailscale0", "--port=41641", "--no-logs-no-support", "--config=" + str(bootstrap)], "peer")
        wait(socket.exists)
        vendor_api(socket, "start", {"AuthKey": auth["key"], "UpdatePrefs": {"ControlURL": "https://control.test:8443", "Hostname": "independent-peer", "WantRunning": True, "CorpDNS": False, "RouteAll": True, "NetfilterMode": 0, "AdvertiseRoutes": ["10.43.0.0/24", "fd43::/64"]}})
        wait(lambda: vendor_api(socket, "status").get("BackendState") == "Running", 30)
        peer_ips = {ipaddress.ip_address(value).version: value for value in vendor_api(socket, "status")["TailscaleIPs"]}
        for address in ("10.43.0.1/32", "fd43::1/128"):
            run(inside("ts-b", ["ip", "addr", "add", address, "dev", "lo"]))
        nodes = json.loads(run(inside("ts-control", head + ["nodes", "list", "-o", "json"])))
        router = next(node for node in nodes if node["name"] == "product-a")
        remote = next(node for node in nodes if node["name"] == "independent-peer")
        run(inside("ts-control", head + ["nodes", "approve-routes", "-i", str(remote["id"]), "-r", "10.43.0.0/24,fd43::/64"]))
        for family,prefix in (("-4","10.43.0.0/24"),("-6","fd43::/64")):
            assert not any(route.get("dst")==prefix for route in json.loads(run(inside("ts-a",["ip",family,"-j","route","show","table","52"]))))
        run(inside("ts-control", head + ["nodes", "approve-routes", "-i", str(router["id"]), "-r", "10.42.0.0/24,fd42::/64,0.0.0.0/0,::/0"]))
        for namespace, host in (("ts-lan", "10.42.0.2"), ("ts-lan", "fd42::2"), ("ts-control", "198.51.100.20"), ("ts-control", "2001:db8:ffff::20"), ("ts-b", "10.43.0.1"), ("ts-b", "fd43::1"), ("ts-a", original["addresses"][0]), ("ts-a", original["addresses"][1])):
            name="app-" + host.replace(":", "_")
            start(namespace, ["python3", "-u", "-c", ECHO_SERVER, host], name)
            wait(lambda: "READY" in (stage/(name+".log")).read_text())
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        traffic("fd42::2", peer_ips[6])
        print("PASS independent peer -> product -> LAN IPv4/IPv6 TCP and UDP", flush=True)
        wait(lambda: len(dns_input()) == 1)
        wait(dns_ready)
        # Span several producer and consumer refreshes with uncached queries.
        for _ in range(12):
            resolve_peer()
            time.sleep(.15)
        assert Path("/etc/resolv.conf").read_text() == "nameserver 192.0.2.1\n"
        print("PASS LAN UDP/TCP native DNS resolved independent peer A/AAAA through MagicDNS without changing OS resolver", flush=True)
        cli = [str(PEERS / "tailscale"), "--socket=" + str(socket)]
        run(inside("ts-b", cli + ["set", "--exit-node=" + original["addresses"][0], "--exit-node-allow-lan-access=true"]))
        wait(lambda: pingable("198.51.100.20"))
        traffic("198.51.100.20", "192.0.2.2")
        traffic("2001:db8:ffff::20", "fd0::2")
        print("PASS IPv4/IPv6 exit-node TCP/UDP; independent server observed product WAN NAT addresses", flush=True)
        direct = run(inside("ts-b", cli + ["ping", "--c=4", "--timeout=1s", original["addresses"][0]]))
        assert "192.0.2.2:41641" in direct, direct
        relay_rules = stage / "relay.nft"
        relay_rules.write_text("table inet fixture { chain input { type filter hook input priority 0; policy accept; udp dport 41641 drop; }; chain output { type filter hook output priority 0; policy accept; udp sport 41641 drop; }; }\n")
        run(inside("ts-b", ["nft", "-f", str(relay_rules)]))
        def relayed():
            result = subprocess.run(inside("ts-b", cli + ["ping", "--until-direct=false", "--c=1", "--timeout=1s", original["addresses"][0]]), capture_output=True, text=True, timeout=3)
            return result.returncode == 0 and "DERP(fixture)" in result.stdout
        wait(relayed, 25)
        traffic("10.42.0.2", peer_ips[4])
        traffic("fd42::2", peer_ips[6])
        traffic("198.51.100.20", "192.0.2.2")
        traffic("2001:db8:ffff::20", "fd0::2")
        run(inside("ts-b", ["nft", "delete", "table", "inet", "fixture"]))
        print("PASS direct and forced DERP paths with real subnet/exit IPv4/IPv6 TCP/UDP", flush=True)
        old = modules()[module]
        signal_worker(old["pid"], signal.SIGKILL)
        wait(lambda: modules()[module]["generation"] > old["generation"] and observation().get("backend_state") == "Running", 30)
        assert observation()["node_id"] == original["node_id"] and observation()["addresses"] == original["addresses"]
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        traffic("2001:db8:ffff::20", "fd0::2")
        rules = json.loads(run(inside("ts-a", ["ip", "-j", "rule"])))
        assert sum(rule.get("priority") == 5270 for rule in rules) == 1
        print("PASS worker SIGKILL: identity and forwarding recovered; one table52 lookup rule", flush=True)
        # Remove enrollment material from current intent. Restart must use the
        # persistent node identity, not silently enroll another node with a key.
        saved = copy.deepcopy(document)
        policy["tailscale"]["TailscaleNode/overlay"]["enrollment"] = "saved_identity"
        policy["tailscale"]["TailscaleNode/overlay"].pop("auth_key_file")
        saved_service = next(s for s in imported_native()["services"] if "tailscale" in s)
        next(r for r in saved["resources"] if r["id"] == "tailnet")["service"] = saved_service
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 1, "request_id": "saved-identity", "config": saved}, timeout=12)
        wait(lambda: observation().get("backend_state") == "Running" and modules()[module]["generation"] > old["generation"] + 1, 30)
        authfile.unlink()
        assert observation()["node_id"] == original["node_id"]
        # Observe route withdrawal at the independent peer, with exit routing
        # disabled so it cannot hide withdrawal of the advertised LAN prefix.
        run(inside("ts-b", cli + ["set", "--exit-node="]))
        trial = copy.deepcopy(saved)
        trial_node = next(r["service"]["tailscale"] for r in trial["resources"] if r["id"] == "tailnet")
        trial_node.update(hostname="product-trial", advertise_routes=["10.42.0.0/24"], advertise_exit_node=False)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 2, "request_id": "tailnet-trial", "config": trial, "confirm_timeout_seconds": 10}, timeout=12)
        def peer_routes():
            peers = vendor_api(socket, "status").get("Peer", {})
            entry = next((p for p in peers.values() if p["ID"] == original["node_id"]), {})
            return entry.get("AllowedIPs") or []
        wait(lambda: "fd42::/64" not in peer_routes() and "0.0.0.0/0" not in peer_routes())
        assert not pingable("fd42::2")
        wait(lambda: product_api("control.sock", "/v1/config")["revision"] == 4, 20)
        wait(lambda: "fd42::/64" in peer_routes() and "0.0.0.0/0" in peer_routes(), 30)
        wait(lambda: pingable("fd42::2"))
        assert observation()["node_id"] == original["node_id"]
        traffic("fd42::2", peer_ips[6])
        print("PASS confirmed rollback restored independent peer routes and IPv6 traffic with the same identity", flush=True)
        old_control = modules()["control"]
        signal_worker(old_control["pid"], signal.SIGKILL)
        wait(lambda: modules()["control"]["generation"] > old_control["generation"] and modules()["control"]["ready"], 30)
        wait(lambda: observation().get("backend_state") == "Running", 30)
        assert observation()["node_id"] == original["node_id"]
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        assert json.loads(run([str(inputs / "routerd"), "poweroff", "--runtime-dir", str(stage / "product-run")]))["accepted"]
        daemon.wait(timeout=12)
        assert daemon.returncode == -signal.SIGINT
        command = ["unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child=TERM", str(inputs / "routerd"), "boot", "--file", str(bootfile)]
        daemon = start("ts-a", command, "product")
        child_file = Path(f"/proc/{daemon.pid}/task/{daemon.pid}/children")
        wait(lambda: child_file.read_text().strip())
        pid1 = int(child_file.read_text().split()[0])
        wait(lambda: (stage / "product-run/control.sock").exists())
        wait(lambda: observation().get("backend_state") == "Running", 30)
        assert observation()["node_id"] == original["node_id"] and observation()["addresses"] == original["addresses"]
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        print("PASS control and whole-daemon restart retained identity and real forwarding without an auth key file", flush=True)
        # Stop control while the worker keeps running; the last applied
        # condition deadline must expire without a new control notification.
        authority = product_api("control.sock", "/v1/services/" + module + "/tailscale-input")["authority"]
        assert authority["nanos"] > time.clock_gettime_ns(time.CLOCK_BOOTTIME)
        control = modules()["control"]
        signal_worker(control["pid"], signal.SIGSTOP)
        frozen.append(control["pid"])
        try:
            wait(lambda: not any(link["ifname"] == "tailscale0" for link in json.loads(run(inside("ts-a", ["ip", "-j", "link"])))))
            assert time.clock_gettime_ns(time.CLOCK_BOOTTIME) <= authority["nanos"] + 1_000_000_000
            assert modules()["control"]["generation"] == control["generation"]
            wait(lambda: "Expired" in product_api("services/resolver.dns.sock", "/v1/status")["resources"][0]["observed"].get("upstreams", ""))
            dns_unavailable()
        finally:
            signal_worker(control["pid"], signal.SIGCONT)
            frozen.remove(control["pid"])
        wait(lambda: observation().get("backend_state") == "Running", 30)
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        wait(dns_ready)
        resolve_peer()
        print("PASS original condition authority expired with control stopped, then actual forwarding and LAN DNS recovered", flush=True)
        run(inside("ts-control", head + ["nodes", "expire", "-i", str(router["id"])]))
        wait(lambda: observation().get("backend_state") == "NeedsLogin", 30)
        wait(lambda: not dns_input())
        wait(lambda: "100.100.100.100" not in product_api("services/resolver.dns.sock", "/v1/status")["resources"][0]["observed"].get("upstreams", ""))
        dns_unavailable()
        assert not pingable("10.42.0.2")
        old = modules()[module]
        signal_worker(old["pid"], signal.SIGKILL)
        wait(lambda: modules()[module]["generation"] > old["generation"] and observation().get("backend_state") == "NeedsLogin", 30)
        assert not pingable("10.42.0.2")
        print("PASS revoked node cannot restore old forwarding by restarting its worker without enrollment material", flush=True)
        # Reauthenticate an expired existing identity with a newly issued key.
        # No vendor state file is deleted, edited or replaced.
        renewed = json.loads(run(inside("ts-control", head + ["preauthkeys", "create", "--user", str(user["id"]), "--reusable", "-o", "json"])))
        renewed_file = stage / "renewed.key"
        renewed_file.write_text(renewed["key"])
        renewed_file.chmod(0o600)
        policy["tailscale"]["TailscaleNode/overlay"].update(enrollment="key_file", auth_key_file=str(renewed_file))
        accepted_source = stage / "accepted-routes.yaml"
        accepted_source.write_text(legacy.read_text().replace("acceptRoutes: false", "acceptRoutes: true"))
        legacy = accepted_source
        reauthenticated = copy.deepcopy(saved)
        next(r for r in reauthenticated["resources"] if r["id"] == "tailnet")["service"] = next(s for s in imported_native()["services"] if "tailscale" in s)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 4, "request_id": "reauthenticated", "config": reauthenticated}, timeout=12)
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("online"), 30)
        assert observation()["node_id"] == original["node_id"] and observation()["addresses"] == original["addresses"]
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        traffic("fd42::2", peer_ips[6])
        wait(lambda: len(dns_input()) == 1)
        wait(dns_ready)
        resolve_peer()
        print("PASS new enrollment key restored the same identity, forwarding and LAN DNS", flush=True)
        for family,prefix in (("-4","10.43.0.0/24"),("-6","fd43::/64")):
            wait(lambda: any(route.get("dst")==prefix for route in json.loads(run(inside("ts-a",["ip",family,"-j","route","show","table","52"])))))
        traffic("10.43.0.1", "10.42.0.2", "ts-lan")
        traffic("fd43::1", "fd42::2", "ts-lan")
        print("PASS imported acceptRoutes:true installed independent peer prefixes and routed LAN IPv4/IPv6 TCP/UDP without NAT", flush=True)
        # First establish incoming host traffic before enabling shieldsUp.
        for host in original["addresses"]:
            traffic(host, peer_ips[ipaddress.ip_address(host).version])
        shielded_source = stage / "shielded.yaml"
        shielded_source.write_text(legacy.read_text().replace("shieldsUp: false", "shieldsUp: true"))
        legacy = shielded_source
        shielded = copy.deepcopy(reauthenticated)
        next(r for r in shielded["resources"] if r["id"] == "tailnet")["service"] = next(s for s in imported_native()["services"] if "tailscale" in s)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 5, "request_id": "shields-up", "config": shielded, "confirm_timeout_seconds": 30}, timeout=12)
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("online"), 30)
        for host in original["addresses"] + ["10.42.0.2", "fd42::2"]:
            run(inside("ts-b", ["python3", "-c", BLOCKED_CLIENT, host]))
        traffic("10.43.0.1", "10.42.0.2", "ts-lan")
        traffic("fd43::1", "fd42::2", "ts-lan")
        print("PASS shieldsUp blocked new inbound host/subnet IPv4/IPv6 TCP/UDP and retained outbound LAN flows", flush=True)
        wait(lambda: product_api("control.sock", "/v1/config")["revision"] == 7, 40)
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("online"), 30)
        wait(lambda: pingable("10.42.0.2"))
        for host in original["addresses"] + ["10.42.0.2", "fd42::2"]:
            traffic(host, peer_ips[ipaddress.ip_address(host).version])
        assert observation()["node_id"] == original["node_id"]
        print("PASS confirmed rollback restored inbound host/subnet traffic with the same identity", flush=True)
        legacy = accepted_source
        tagged_source = stage / "tagged.yaml"
        tagged_source.write_text(legacy.read_text().replace("hostname: product-a", "hostname: product-a\n        advertiseTags: [tag:router]"))
        legacy = tagged_source
        tagged = copy.deepcopy(reauthenticated)
        next(r for r in tagged["resources"] if r["id"] == "tailnet")["service"] = next(s for s in imported_native()["services"] if "tailscale" in s)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 7, "request_id": "tagged", "config": tagged}, timeout=12)
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("online"), 30)
        assert not observation().get("tags")
        print("PASS requested tags were not reported as assigned with a personal enrollment key", flush=True)
        # Headscale's preauth contract assigns tags from the approved key.
        # Expire the personal registration before explicit tagged enrollment.
        run(inside("ts-control", head + ["nodes", "expire", "-i", str(router["id"])]))
        wait(lambda: observation().get("backend_state") == "NeedsLogin", 30)
        tag_key = json.loads(run(inside("ts-control", head + ["preauthkeys", "create", "--tags", "tag:router", "-o", "json"])))
        tagged_keyfile = stage / "tagged.key"
        tagged_keyfile.write_text(tag_key["key"])
        tagged_keyfile.chmod(0o600)
        policy["tailscale"]["TailscaleNode/overlay"]["auth_key_file"] = str(tagged_keyfile)
        approved = copy.deepcopy(tagged)
        next(r for r in approved["resources"] if r["id"] == "tailnet")["service"] = next(s for s in imported_native()["services"] if "tailscale" in s)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 8, "request_id": "approved-tag", "config": approved}, timeout=12)
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("tags") == ["tag:router"], 30)
        tagged_id = observation()["node_id"]
        nodes = json.loads(run(inside("ts-control", head + ["nodes", "list", "-o", "json"])))
        tagged_node = next(n for n in nodes if str(n["id"]) == tagged_id)
        assert tagged_node["tags"] == ["tag:router"], tagged_node
        run(inside("ts-control", head + ["nodes", "approve-routes", "-i", tagged_id, "-r", "10.42.0.0/24,fd42::/64,0.0.0.0/0,::/0"]))
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        traffic("fd42::2", peer_ips[6])
        print("PASS approved tagged enrollment was exposed as observed tags with real IPv4/IPv6 forwarding", flush=True)
        # Removing a client request is not server-side tag revocation.
        legacy = accepted_source
        policy["tailscale"]["TailscaleNode/overlay"].update(enrollment="saved_identity")
        policy["tailscale"]["TailscaleNode/overlay"].pop("auth_key_file")
        retained = copy.deepcopy(approved)
        next(r for r in retained["resources"] if r["id"] == "tailnet")["service"] = next(s for s in imported_native()["services"] if "tailscale" in s)
        product_api("control.sock", "/v1/config/commit", {"expected_revision": 9, "request_id": "retained-server-tags", "config": retained}, timeout=12)
        tagged_keyfile.unlink()
        renewed_file.unlink()
        wait(lambda: observation().get("backend_state") == "Running" and observation().get("tags") == ["tag:router"], 30)
        assert observation()["node_id"] == tagged_id
        wait(lambda: pingable("10.42.0.2"))
        traffic("10.42.0.2", peer_ips[4])
        print("PASS saved identity retained server-assigned tags after removing the client request and enrollment files", flush=True)
        # Observe independent peer metadata too, not only the local node.
        run(inside("ts-control", head + ["nodes", "tag", "-i", str(remote["id"]), "-t", "tag:router"]))
        wait(lambda: any(p["id"] == str(remote["id"]) and p.get("tags") == ["tag:router"] and p.get("dns_name") == "independent-peer.test.invalid." for p in observation().get("peers", [])))
        traffic("10.43.0.1", "10.42.0.2", "ts-lan")
        traffic("fd43::1", "fd42::2", "ts-lan")
        print("PASS independent peer DNS name and assigned tags were observed with both-family LAN forwarding", flush=True)
        run(inside("ts-control", head + ["nodes", "expire", "-i", tagged_id]))
        wait(lambda: observation().get("backend_state") == "NeedsLogin", 30)
        assert not pingable("10.42.0.2")
        old = modules()[module]
        signal_worker(old["pid"], signal.SIGKILL)
        wait(lambda: modules()[module]["generation"] > old["generation"] and observation().get("backend_state") == "NeedsLogin", 30)
        assert not pingable("10.42.0.2")
        wait(lambda: not dns_input())
        wait(lambda: "100.100.100.100" not in product_api("services/resolver.dns.sock", "/v1/status")["resources"][0]["observed"].get("upstreams", ""))
        dns_unavailable()
        print("PASS admin-expired tagged identity could not regain forwarding or tailnet DNS by restarting without an auth key", flush=True)
        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=3)
        packets = run(["tcpdump", "-Z", "root", "-nn", "-r", str(stage / "tailnet.pcap")])
        for host in ("10.42.0.2", "fd42::2", "198.51.100.20", "2001:db8:ffff::20"):
            for port in (18080, 18081):
                assert host + "." + str(port) in packets
        assert "A? control.test." in packets
        assert ".41641 >" in packets
        assert "A? independent-peer.test.invalid." in packets
        assert "AAAA? independent-peer.test.invalid." in packets
        assert "100.100.100.100.53" in packets
        assert Path("/etc/resolv.conf").read_text() == "nameserver 192.0.2.1\n"
        print("PASS capture: LAN and WAN TCP/UDP plus encrypted underlay packets", flush=True)
    except BaseException:
        supervisor = stage / "product-run/supervisor.sock"
        if supervisor.exists():
            status = subprocess.run(["curl", "--max-time", "2", "-fsS", "--unix-socket", str(supervisor), "http://localhost/v1/status"], capture_output=True, text=True)
            print("SUPERVISOR:", status.stdout or status.stderr, flush=True)
            if status.returncode == 0:
                control = next((m for m in json.loads(status.stdout)["modules"] if m["name"] == "control"), None)
                if control and control.get("pid") and Path(f"/proc/{pid1}/root/proc/{control['pid']}/io").exists():
                    print("CONTROL failed-start I/O:", control_io(control["pid"]), flush=True)
        for log in stage.glob("*.log"):
            lines=log.read_text(errors="replace").splitlines()[-25:]
            print(log.name, "\n".join("[private enrollment detail omitted]" if "/register/" in line or "tskey-" in line else line for line in lines), flush=True)
        raise
    finally:
        for pid in frozen:
            signal_worker(pid, signal.SIGCONT)
        for process in reversed(children):
            if process.poll() is None:
                process.terminate()
        for process in reversed(children):
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for namespace in reversed(namespaces):
            subprocess.run(["ip", "netns", "del", namespace], capture_output=True)


if __name__ == "__main__":
    if sys.argv[1:] == ["--inside"]:
        with tempfile.TemporaryDirectory(prefix="routerd-tailscale-") as directory:
            peer(Path(directory))
    elif sys.argv[1:]:
        raise SystemExit("usage: tailscale-peer.py")
    else:
        prefix = [] if os.geteuid() == 0 else ["sudo", "-n"]
        raise SystemExit(subprocess.run(prefix + ["unshare", "--mount", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", "python3", "-I", str(Path(__file__).resolve()), "--inside"]).returncode)
