#!/usr/bin/env python3
"""Rehearse one stopped-owner routerd state migration through a fresh PVE VM.

This fixture creates all former-owner state in its private namespaces.  It never
reads a homert guest or its state.  The former owner is an official tailscaled
node plus dnsmasq; its sticky SQLite input uses the documented old-router table
format.  A fresh routerd2 BIOS disk imports only the selected state,
then boots as PID 1 in an owned isolated PVE VM.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import shlex

ROOT = Path(os.environ.get("ROUTERD2_ROOT", Path(__file__).resolve().parents[1]))

ECHO_SERVER = r'''import socket,sys,threading
address=sys.argv[1]; family=socket.AF_INET6 if ':' in address else socket.AF_INET
def reply(conn):
    with conn:
        body=conn.recv(4096); conn.sendall(body)
tcp=socket.socket(family,socket.SOCK_STREAM); tcp.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); tcp.bind((address,18080)); tcp.listen()
def accept():
    while True:
        conn,_=tcp.accept(); threading.Thread(target=reply,args=(conn,),daemon=True).start()
threading.Thread(target=accept,daemon=True).start()
udp=socket.socket(family,socket.SOCK_DGRAM); udp.bind((address,18081)); print('READY',flush=True)
while True:
    body,peer=udp.recvfrom(4096); udp.sendto(body,peer)
'''
ECHO_CLIENT = r'''import socket,sys
address,source=sys.argv[1:]; family=socket.AF_INET6 if ':' in address else socket.AF_INET
for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
    sock=socket.socket(family,kind); sock.settimeout(3); sock.bind((source,0)); sock.connect((address,port)); sock.sendall(b'routerd2-real-traffic'); response=sock.recv(4096); assert response==b'routerd2-real-traffic',response; sock.close()
'''


def call(argv, timeout=30, input=None):
    result = subprocess.run(argv, input=input, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("%s: %s%s" % (shlex.join(argv), result.stdout[-1200:], result.stderr[-1200:]))
    return result.stdout


def pve_prepare(stage, target, jump, identity):
    owner = os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name
    known_hosts = Path(pwd.getpwnam(owner).pw_dir) / ".ssh" / "known_hosts"
    ssh = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + str(known_hosts), "-o", "GlobalKnownHostsFile=/dev/null"]
    if jump:
        ssh += ["-J", jump]
    if identity:
        ssh += ["-o", "IdentitiesOnly=yes", "-i", str(identity)]
    ssh += [target]
    remote = call(ssh + ["mktemp -d /tmp/routerd2-migration.XXXXXX"]).strip()
    vmid = int(json.loads(call(ssh + ["pvesh get /cluster/nextid --output-format json"])))
    control_dir = Path(tempfile.mkdtemp(prefix="routerd2-migration-ssh."))
    name = "routerd2-initial-state-" + str(int(time.time()))
    meta = {"ssh": ssh, "target": target, "remote": remote, "vmid": vmid, "name": name,
            "user": pwd.getpwuid(os.getuid()).pw_name, "control": str(control_dir / "master"),
            "wan": str(control_dir / "wan.sock"), "lan": str(control_dir / "lan.sock")}
    create = ["qm", "create", str(vmid), "--name", name, "--tags", "routerd2-test",
              "--description", "Temporary isolated routerd2 initial-state migration rehearsal; no production bridge or NIC",
              "--machine", "q35", "--cpu", "host", "--cores", "4", "--sockets", "1", "--memory", "4096",
              "--balloon", "0", "--kvm", "1", "--ostype", "l26", "--vga", "none", "--tablet", "0", "--onboot", "0"]
    call(ssh + [shlex.join(create)], 60)
    master = ssh[:-1] + ["-M", "-N", "-f", "-S", meta["control"], "-o", "ExitOnForwardFailure=yes",
                           "-L", meta["wan"] + ":" + remote + "/wan.sock",
                           "-L", meta["lan"] + ":" + remote + "/lan.sock", target]
    subprocess.run(master, check=True, timeout=30, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    (stage / "kvm.json").write_text(json.dumps(meta)); (stage / "kvm.json").chmod(0o600)
    return meta


def pve_remove(stage):
    path = stage / "kvm.json"
    if not path.exists():
        return
    meta = json.loads(path.read_text())
    ssh = meta["ssh"]
    config = call(ssh + ["qm config " + str(meta["vmid"])])
    if "name: " + meta["name"] + "\n" not in config:
        raise RuntimeError("owned PVE test VM identity changed; preserve it")
    for command in ("qm stop " + str(meta["vmid"]), "qm destroy " + str(meta["vmid"]), "rm -rf -- " + shlex.quote(meta["remote"])):
        call(ssh + [command], 60)
    subprocess.run(["ssh", "-S", meta["control"], "-O", "exit", meta["target"]], capture_output=True, timeout=15)
    shutil.rmtree(Path(meta["control"]).parent, ignore_errors=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--pve-host")
    parser.add_argument("--pve-jump")
    parser.add_argument("--pve-identity", type=Path)
    args = parser.parse_args()
    if args.inside:
        peer(args.inside)
        return
    if not args.pve_host:
        parser.error("--pve-host is required: this rehearsal boots a fresh isolated PVE VM")
    stage = Path(tempfile.mkdtemp(prefix="routerd2-initial-state."))
    try:
        pve_prepare(stage, args.pve_host, args.pve_jump, args.pve_identity)
        for name in ("routerd", "routerd-modules", "routerd-logging"):
            shutil.copy2(ROOT / "dist/bin" / name, stage / name)
            (stage / name).chmod(0o500)
        shutil.copytree(ROOT / "dist/image", stage / "image", copy_function=shutil.copy2)
        shutil.copytree(ROOT / "dist/peer-env/tailscale", stage / "tailscale", copy_function=shutil.copy2)
        shutil.copy2(ROOT / "scripts/qemu-stream-tap.py", stage / "qemu-stream-tap.py")
        shutil.copy2(Path(__file__), stage / "peer.py")
        environment = os.environ | {"ROUTERD2_ROOT": str(ROOT)}
        subprocess.run(["sudo", "-n", "env", "ROUTERD2_ROOT=" + str(ROOT), "unshare", "--mount", "--net", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", sys.executable, "-I", str(stage / "peer.py"), "--inside", str(stage)], check=True, timeout=900, env=environment)
    finally:
        try:
            pve_remove(stage)
        finally:
            # The private root namespace writes its fixture inputs as root.
            # Remove the owned, per-run directory even after a failed fixture.
            subprocess.run(["sudo", "-n", "python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1], ignore_errors=True)", str(stage)], check=False)


def peer(stage):
    children, namespaces, relays = [], [], []
    selected = Path("/var/lib/routerd2-initial-state-" + str(os.getpid()))
    def run(argv, timeout=20, input=None): return call(argv, timeout, input)
    def inside(ns, argv): return ["ip", "netns", "exec", ns] + argv
    def start(ns, argv, label):
        log = (stage / (label + ".log")).open("a")
        process = subprocess.Popen(inside(ns, argv) if ns else argv, stdout=log, stderr=subprocess.STDOUT)
        log.close(); children.append(process); return process
    def wait(check, seconds=45):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if check(): return
            time.sleep(.25)
        raise RuntimeError("migration communication deadline")
    def digest(path):
        h = hashlib.sha256()
        with path.open("rb") as source:
            for block in iter(lambda: source.read(1024 * 1024), b""): h.update(block)
        return h.digest()
    def selected_copy(source, target):
        st = source.lstat()
        if not source.is_file() or source.is_symlink() or not stat.S_ISREG(st.st_mode):
            raise RuntimeError("migration accepts selected regular files only")
        before = digest(source); shutil.copyfile(source, target); target.chmod(0o600)
        if before != digest(target): raise RuntimeError("migration copy digest mismatch")
    try:
        import stat
        run(["mount", "--make-rprivate", "/"])
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/run"])
        run(["mount", "-t", "tmpfs", "-o", "mode=755", "tmpfs", "/var/lib/dhcpcd"])
        for bridge in ("wanbr", "lanbr"):
            run(["ip", "link", "add", bridge, "type", "bridge"]); run(["ip", "link", "set", bridge, "up"])
        for ns in ("migration-wan", "migration-old", "migration-client", "migration-new-client", "migration-peer"):
            run(["ip", "netns", "add", ns]); namespaces.append(ns); run(["ip", "-n", ns, "link", "set", "lo", "up"])
        for host, bridge, ns, dev in (("wanport", "wanbr", "migration-wan", "wan0"), ("oldwan", "wanbr", "migration-old", "wan0"),
                                      ("oldlan", "lanbr", "migration-old", "lan0"), ("clientport", "lanbr", "migration-client", "eth0"), ("newclientport", "lanbr", "migration-new-client", "eth0"),
                                      ("peerport", "wanbr", "migration-peer", "eth0")):
            run(["ip", "link", "add", host, "type", "veth", "peer", "name", dev, "netns", ns]); run(["ip", "link", "set", host, "master", bridge]); run(["ip", "link", "set", host, "up"]); run(inside(ns, ["ip", "link", "set", dev, "up"]))
        for tap, bridge in (("tapwan", "wanbr"), ("taplan", "lanbr")):
            run(["ip", "tuntap", "add", tap, "mode", "tap"]); run(["ip", "link", "set", tap, "master", bridge]); run(["ip", "link", "set", tap, "up"])
        run(inside("migration-wan", ["ip", "addr", "add", "192.0.2.1/24", "dev", "wan0"])); run(inside("migration-wan", ["ip", "-6", "addr", "add", "fd00::1/64", "dev", "wan0"]))
        run(inside("migration-wan", ["ip", "route", "add", "10.42.0.0/24", "via", "192.0.2.2"]))
        run(inside("migration-wan", ["ip", "-6", "route", "add", "fd42::/64", "via", "fd00::2"]))
        run(inside("migration-old", ["ip", "addr", "add", "192.0.2.2/24", "dev", "wan0"])); run(inside("migration-old", ["ip", "-6", "addr", "add", "fd00::2/64", "dev", "wan0"])); run(inside("migration-old", ["ip", "addr", "add", "10.42.0.1/24", "dev", "lan0"])); run(inside("migration-old", ["ip", "-6", "addr", "add", "fd42::1/64", "dev", "lan0"]))
        for ns, mac in (("migration-client", "02:aa:bb:cc:dd:01"), ("migration-new-client", "02:aa:bb:cc:dd:02")):
            run(inside(ns, ["ip", "link", "set", "eth0", "down"])); run(inside(ns, ["ip", "link", "set", "eth0", "address", mac])); run(inside(ns, ["ip", "link", "set", "eth0", "up"]))
        run(inside("migration-client", ["ip", "-6", "addr", "add", "fd42::2/64", "dev", "eth0"])); run(inside("migration-client", ["ip", "-6", "route", "add", "default", "via", "fd42::1"]))
        run(inside("migration-peer", ["ip", "addr", "add", "192.0.2.3/24", "dev", "eth0"])); run(inside("migration-peer", ["ip", "-6", "addr", "add", "fd00::3/64", "dev", "eth0"]))
        for ns, device in (("migration-wan", "wan0"), ("migration-old", "wan0"), ("migration-old", "lan0"), ("migration-client", "eth0"), ("migration-peer", "eth0")):
            wait(lambda ns=ns, device=device: not run(inside(ns, ["ip", "-6", "-o", "addr", "show", "tentative", "dev", device])).strip(), 20)
        # Independent Headscale and peer; both use the official bundled binaries.
        control = stage / "control"; control.mkdir(mode=0o700)
        cert, key = control / "headscale.pem", control / "headscale.key"
        run(["openssl", "req", "-config", "/dev/null", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(key), "-out", str(cert), "-subj", "/CN=migration", "-addext", "subjectAltName=IP:192.0.2.1"])
        policy = control / "policy.json"; policy.write_text(json.dumps({"acls": [{"action": "accept", "src": ["*"], "dst": ["*:*"]}]}))
        headcfg = {"policy": {"mode": "file", "path": str(policy)}, "server_url": "https://192.0.2.1:8443", "listen_addr": "192.0.2.1:8443", "noise": {"private_key_path": str(control / "noise.key")}, "prefixes": {"v4": "100.64.0.0/10", "v6": "fd7a:115c:a1e0::/48"}, "database": {"type": "sqlite", "sqlite": {"path": str(control / "db.sqlite")}}, "derp": {"urls": [], "paths": [], "auto_update_enabled": False, "server": {"enabled": True, "region_id": 999, "region_code": "migration", "region_name": "migration", "stun_listen_addr": "0.0.0.0:3478", "private_key_path": str(control / "derp.key"), "automatically_add_embedded_derp_region": True, "ipv4": "192.0.2.1"}}, "dns": {"override_local_dns": False, "magic_dns": True, "base_domain": "test.invalid"}, "unix_socket": str(control / "headscale.sock"), "tls_cert_path": str(cert), "tls_key_path": str(key), "disable_check_updates": True, "log": {"level": "warn"}}
        config = control / "headscale.json"; config.write_text(json.dumps(headcfg)); head = [str(stage / "tailscale/headscale"), "-c", str(config)]
        start("migration-wan", head + ["serve"], "headscale"); wait(lambda: (control / "headscale.sock").exists())
        upstream = start("migration-wan", ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-resolv", "--no-hosts", "--bind-interfaces", "--listen-address=192.0.2.1", "--address=/migration.test/192.0.2.77"], "wan-dns")
        start("migration-wan", ["python3", "-u", "-c", ECHO_SERVER, "192.0.2.1"], "wan-echo4")
        start("migration-wan", ["python3", "-u", "-c", ECHO_SERVER, "fd00::1"], "wan-echo6")
        def wan_echo_ready():
            logs = [(stage / name).read_text(errors="replace") for name in ("wan-echo4.log", "wan-echo6.log")]
            if any("Traceback" in text for text in logs):
                raise RuntimeError("WAN echo setup failed: " + repr([text[-400:] for text in logs]))
            return all("READY" in text for text in logs)
        wait(wan_echo_ready)
        user = json.loads(run(inside("migration-wan", head + ["users", "create", "fixture", "-o", "json"])))
        auth = json.loads(run(inside("migration-wan", head + ["preauthkeys", "create", "--user", str(user["id"]), "--reusable", "-o", "json"])))
        authfile = control / "auth.key"; authfile.write_text(auth["key"]); authfile.chmod(0o600)
        peerstate = stage / "peer"; peerstate.mkdir(mode=0o700); peersock = peerstate / "tailscaled.sock"
        peerd = start("migration-peer", ["env", "SSL_CERT_FILE=" + str(cert), "TS_LOGS_DIR=" + str(peerstate), str(stage / "tailscale/tailscaled"), "--state=" + str(peerstate / "tailscaled.state"), "--statedir=" + str(peerstate), "--socket=" + str(peersock), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "tail-peer")
        wait(peersock.exists); peercli = [str(stage / "tailscale/tailscale"), "--socket=" + str(peersock)]
        run(inside("migration-peer", ["env", "SSL_CERT_FILE=" + str(cert)] + peercli + ["up", "--timeout=15s", "--login-server=https://192.0.2.1:8443", "--hostname=independent-peer", "--auth-key=file:" + str(authfile), "--accept-dns=false", "--netfilter-mode=off"]), 25)
        wait(lambda: json.loads(run(inside("migration-peer", peercli + ["status", "--json"]))).get("BackendState") == "Running")
        # The former owner's services all run before the migration stop boundary.
        old = stage / "old"; old.mkdir(mode=0o700); oldstate = old / "tailscaled.state"; oldsock = old / "tailscaled.sock"
        oldtail = start("migration-old", ["env", "SSL_CERT_FILE=" + str(cert), "TS_LOGS_DIR=" + str(old), str(stage / "tailscale/tailscaled"), "--state=" + str(oldstate), "--statedir=" + str(old), "--socket=" + str(oldsock), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "old-tailscaled")
        wait(oldsock.exists)
        oldcli = [str(stage / "tailscale/tailscale"), "--socket=" + str(oldsock)]
        run(inside("migration-old", ["env", "SSL_CERT_FILE=" + str(cert)] + oldcli + ["up", "--timeout=15s", "--login-server=https://192.0.2.1:8443", "--hostname=former-owner", "--auth-key=file:" + str(authfile), "--accept-dns=false", "--netfilter-mode=off"]), 25)
        wait(lambda: json.loads(run(inside("migration-old", oldcli + ["status", "--json"]))).get("BackendState") == "Running")
        old_id = json.loads(run(inside("migration-old", oldcli + ["status", "--json"]))) ["Self"]["ID"]
        oldlease = old / "dnsmasq.leases"
        olddhcp = start("migration-old", ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-resolv", "--no-hosts", "--interface=lan0", "--bind-interfaces", "--dhcp-authoritative", "--dhcp-leasefile=" + str(oldlease), "--dhcp-range=10.42.0.50,10.42.0.100,255.255.255.0,10m", "--dhcp-option=option:router,10.42.0.1", "--dhcp-option=option:dns-server,10.42.0.1", "--address=/migration.test/10.42.0.1"], "old-dnsmasq")
        # A real old client obtains the input lease before it is imported.
        hook = stage / "client-hook.sh"; events = stage / "client-events"
        hook.write_text('#!/bin/sh\ncase "$reason" in BOUND|RENEW|REBIND) printf "%s %s %s %s\\n" "$reason" "$new_ip_address" "$new_routers" "$new_domain_name_servers" >> "' + str(events) + '";; esac\n') ; hook.chmod(0o500)
        clientconf = stage / "dhcpcd.conf"; clientconf.write_text("noipv4ll\noption domain_name_servers\n")
        client = start("migration-client", ["dhcpcd", "-B", "-d", "-f", str(clientconf), "-c", str(hook), "eth0"], "legacy-client")
        wait(lambda: events.exists() and bool(events.read_text().strip()), 60)
        if olddhcp.poll() is not None or client.poll() is not None:
            raise RuntimeError("old DHCP path exited: " + (stage / "old-dnsmasq.log").read_text(errors="replace")[-1200:] + (stage / "legacy-client.log").read_text(errors="replace")[-1200:])
        lease_line = events.read_text().splitlines()[-1].split(); leased_ip = lease_line[1]
        start("migration-client", ["python3", "-u", "-c", ECHO_SERVER, leased_ip], "lan-echo4")
        start("migration-client", ["python3", "-u", "-c", ECHO_SERVER, "fd42::2"], "lan-echo6")
        wait(lambda: "READY" in (stage / "lan-echo4.log").read_text() and "READY" in (stage / "lan-echo6.log").read_text())
        run(inside("migration-client", ["dig", "@10.42.0.1", "migration.test", "A", "+short"]))
        # The legacy daemon leaves routerd's documented SQLite sticky record.
        # Its source package needs a Go 1.25 toolchain, unavailable in this isolated
        # namespace, so create the same on-disk row the old daemon persists.
        import sqlite3
        sticky = old / "dhcp-sticky.db"
        with sqlite3.connect(sticky) as db:
            db.executescript("""CREATE TABLE dhcp_sticky_leases (mac TEXT NOT NULL, ip TEXT NOT NULL, hostname TEXT, family TEXT NOT NULL, allocated_at INTEGER NOT NULL DEFAULT 0, released_at INTEGER NOT NULL DEFAULT 0, last_request_at INTEGER NOT NULL DEFAULT 0, sticky_until INTEGER NOT NULL DEFAULT 0);""")
            now = time.time_ns(); db.execute("INSERT INTO dhcp_sticky_leases VALUES(?,?,?,?,?,?,?,?)", ("02:aa:bb:cc:dd:01", leased_ip, "legacy", "ipv4", now, 0, now, now + 3 * 86400 * 1_000_000_000))
        sticky.chmod(0o600)
        # Copy only selected regular inputs after stopping old ownership.
        olddhcp.terminate(); olddhcp.wait(timeout=10)
        oldtail.send_signal(signal.SIGINT); oldtail.wait(timeout=20)
        assert not json.loads(run(inside("migration-old", ["ip", "-j", "route", "show", "table", "52"]))), "former owner still owns table 52"
        for command in (["ss", "-H", "-lun", "sport", "=", ":41641"], ["ss", "-H", "-lun", "sport", "=", ":67"]):
            assert not run(inside("migration-old", command)).strip(), "former owner still owns a UDP listener"
        # tailscaled releases its routes and socket but leaves an inert TUN in
        # this kernel.  It is an old-owner resource, so remove it only after
        # the process, table and listener have proved absent.
        if subprocess.run(inside("migration-old", ["ip", "link", "show", "tailscale0"]), capture_output=True).returncode == 0:
            run(inside("migration-old", ["ip", "link", "delete", "tailscale0"]))
        wait(lambda: subprocess.run(inside("migration-old", ["ip", "link", "show", "tailscale0"]), capture_output=True).returncode != 0, 10)
        # A stopped daemon is not enough: its old interface addresses would
        # still answer ARP/NDP and make a new owner appear unreachable.
        for command in (["ip", "addr", "del", "10.42.0.1/24", "dev", "lan0"], ["ip", "-6", "addr", "del", "fd42::1/64", "dev", "lan0"], ["ip", "addr", "del", "192.0.2.2/24", "dev", "wan0"], ["ip", "-6", "addr", "del", "fd00::2/64", "dev", "wan0"]):
            run(inside("migration-old", command))
        run(inside("migration-client", ["ip", "neigh", "flush", "dev", "eth0"]))
        selected.mkdir(mode=0o700); selected.chmod(0o700); (selected / "services/tailscale").mkdir(parents=True, mode=0o700)
        sshkey = old / "ssh_host_ed25519_key"; run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(sshkey)])
        former_source = {path: digest(path) for path in (oldstate, oldlease, sticky, sshkey)}
        selected_copy(oldstate, selected / "services/tailscale/tailscaled.state"); selected_copy(sshkey, selected / "ssh_host_ed25519_key")
        # The new intent accepts the same old LAN lease and sticky history before first boot.
        configfile = stage / "config.json"; resources = [
            {"id":"wan", "interface":{"name":"eth0"}}, {"id":"lan", "interface":{"name":"eth1"}},
            {"id":"wan4", "address":{"interface":"eth0","cidr":"192.0.2.2/24"}}, {"id":"wan6", "address":{"interface":"eth0","cidr":"fd00::2/64"}},
            {"id":"lan4", "address":{"interface":"eth1","cidr":"10.42.0.1/24"}}, {"id":"lan6", "address":{"interface":"eth1","cidr":"fd42::1/64"}},
            {"id":"default4", "route":{"destination":"0.0.0.0/0","interface":"eth0","gateway":"192.0.2.1"}}, {"id":"default6", "route":{"destination":"::/0","interface":"eth0","gateway":"fd00::1"}},
            {"id":"forward", "forwarding":{"ipv4":True,"ipv6":True,"ipv4_rp_filter":0}}, {"id":"nat", "nat":[{"type":"masquerade","match":{"family":4,"source":"10.42.0.0/24","out_interface":"eth0"}}]},
            {"id":"dhcp", "service":{"name":"leases","dhcp_server":{"interface":"eth1","server_address":"10.42.0.1","authoritative":True,"sticky_hold_days":3,"primary":{"prefix":"10.42.0.0/24","start":"10.42.0.50","end":"10.42.0.100","lease_seconds":600,"router":"10.42.0.1","dns_servers":["10.42.0.1"]}}}},
            {"id":"dns", "service":{"name":"dns","dns":{"spec":{"listen":[{"name":"lan","addresses":["10.42.0.1","fd42::1"],"port":53}],"sources":[{"name":"upstream","kind":"forward","match":["."],"upstreams":["udp://192.0.2.1:53"]}]}}}},
            {"id":"tail", "service":{"name":"tailnet","tailscale":{"interface":"tailscale0","server_url":"https://192.0.2.1:8443","hostname":"former-owner","control_ca_file":"/etc/routerd/headscale.pem"}}}]
        configfile.write_text(json.dumps({"schema_version":2,"resources":resources})); run([str(stage / "routerd"), "config", "validate", str(configfile)])
        call([str(stage / "routerd"), "dhcp", "import-leases", "--config", str(configfile), "--service", "leases", "--state-dir", str(selected), str(oldlease)])
        call([str(stage / "routerd"), "dhcp", "import-sticky", "--config", str(configfile), "--service", "leases", "--state-dir", str(selected), str(sticky)])
        # Build a new disk only; no legacy disk is modified.
        overlay = stage / "etc"; (overlay / "routerd").mkdir(parents=True); shutil.copy2(cert, overlay / "routerd/headscale.pem")
        keypair = stage / "admin"; run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(keypair)])
        boot = json.loads((ROOT / "images/boot.json").read_text()); boot["state_volume"] = {"device":"/dev/vda2","wait_timeout_seconds":10}; boot["ssh"]["authorized_keys"] = [keypair.with_suffix(".pub").read_text().strip()]
        (overlay / "routerd/boot.json").write_text(json.dumps(boot))
        diskout = stage / "disk"; call(["python3", str(ROOT / "scripts/build-disk-image.py"), "--image-dir", str(stage / "image"), "--output", str(diskout), "--size-gib", "2", "--state-dir", str(selected), "--config", str(configfile), "--etc-dir", str(overlay)], 180)
        manifest = json.loads((diskout / "manifest.json").read_text()); assert manifest["initial_state_provisioned"]
        loop = run(["losetup", "--find", "--show", "--partscan", str(diskout / "routerd.raw")]).strip()
        mounted = stage / "mounted-state"; mounted.mkdir(mode=0o700)
        try:
            run(["mount", "-o", "ro", loop + "p2", str(mounted)])
            for relative in ("services/tailscale/tailscaled.state", "ssh_host_ed25519_key"):
                source, copied = selected / relative, mounted / relative
                stat_result = copied.lstat()
                if not copied.is_file() or copied.is_symlink() or stat_result.st_uid != 0 or stat_result.st_mode & 0o777 != 0o600 or digest(source) != digest(copied):
                    raise RuntimeError("offline selected state inspection failed")
        finally:
            subprocess.run(["umount", str(mounted)], capture_output=True)
            run(["losetup", "--detach", loop])
        # Upload the complete BIOS disk into the already-owned isolated PVE VM.
        meta = json.loads((stage / "kvm.json").read_text()); remote = Path(meta["remote"]); controlssh = ["runuser", "-u", meta["user"], "--", "ssh", "-S", meta["control"], "-o", "BatchMode=yes", meta["target"]]
        with subprocess.Popen(["gzip", "-c", str(diskout / "routerd.raw")], stdout=subprocess.PIPE) as archive:
            result = subprocess.run(controlssh + ["umask 077; gzip -d > " + shlex.quote(str(remote / "routerd.raw"))], stdin=archive.stdout, capture_output=True, timeout=180)
            archive.stdout.close()
            if result.returncode or archive.wait(timeout=20): raise RuntimeError("upload BIOS disk failed")
        qargs = ["-serial", "file:" + str(remote / "serial.log"), "-drive", "file=" + str(remote / "routerd.raw") + ",if=virtio,format=raw"]
        for name, mac in (("wan", "52:54:00:12:34:56"), ("lan", "52:54:00:12:34:57")):
            qargs += ["-netdev", "stream,id=" + name + ",server=on,addr.type=unix,addr.path=" + str(remote / (name+".sock")), "-device", "virtio-net-pci,netdev=" + name + ",mac=" + mac]
        run(controlssh + [shlex.join(["qm", "set", str(meta["vmid"]), "--args", shlex.join(qargs)])], 45)
        product = start(None, controlssh + ["qm start " + str(meta["vmid"]) + "; pid=$(cat /run/qemu-server/" + str(meta["vmid"]) + ".pid); exec tail --pid=$pid -f " + shlex.quote(str(remote / "serial.log"))], "pve-serial")
        wait(lambda: subprocess.run(controlssh + ["test -S " + shlex.quote(str(remote / "wan.sock")) + " -a -S " + shlex.quote(str(remote / "lan.sock"))], capture_output=True).returncode == 0, 30)
        for name in ("wan", "lan"): relays.append(start(None, [sys.executable, "-I", str(stage / "qemu-stream-tap.py"), "--tap", "tap"+name, "--socket", meta[name]], "relay-"+name))
        capture = start(None, ["tcpdump", "-l", "-n", "-i", "tapwan", "host", "192.0.2.1", "or", "udp", "port", "41641", "or", "port", "18080", "or", "port", "18081"], "wan-capture")
        wait(lambda: "ROUTERD_READY" in run(controlssh + ["cat " + shlex.quote(str(remote / "serial.log"))]), 120)
        # The independent official peer is the migration authority: the saved
        # node must reappear with the same immutable ID, rather than enrolling
        # a replacement identity.  It avoids trusting our management snapshot.
        def migrated_peer():
            status = json.loads(run(inside("migration-peer", peercli + ["status", "--json"])))
            return next((item for item in status.get("Peer", {}).values() if item.get("ID") == old_id), {})
        try:
            wait(lambda: migrated_peer().get("Online"), 60)
        except RuntimeError as failure:
            # Report only current, non-secret diagnostic metadata.  Raw logs
            # and the bounded capture remain in this owned directory and are
            # deleted by the enclosing cleanup.
            peer_state = json.loads(run(inside("migration-peer", peercli + ["status", "--json"])))
            node_state = json.loads(run(inside("migration-wan", head + ["nodes", "list", "-o", "json"])))
            node = next((item for item in node_state if str(item.get("id")) == str(old_id)), {})
            serial = run(controlssh + ["tail -n 160 " + shlex.quote(str(remote / "serial.log"))], 20)
            terms = ("tailscale", "tailnet", "error", "fail", "ready", "routerd")
            serial = [line[-240:] for line in serial.splitlines() if any(term in line.lower() for term in terms)][-24:]
            packets = (stage / "wan-capture.log").read_text(errors="replace").splitlines()[-24:]
            raise RuntimeError("saved identity did not return: peer_backend=%s old_node_online=%s old_node_expired=%s serial=%s wan_packets=%s" % (peer_state.get("BackendState"), node.get("online"), node.get("expired"), serial, packets)) from failure
        product_tail = next(address for address in migrated_peer()["TailscaleIPs"] if "." in address)
        run(inside("migration-peer", peercli + ["ping", "--timeout=10s", product_tail]), 15)
        wait(lambda: migrated_peer().get("Active"), 15)
        # DHCP renewal is issued by the original client after the new owner is ready.
        client.send_signal(signal.SIGHUP); wait(lambda: len(events.read_text().splitlines()) >= 2, 30)
        if events.read_text().splitlines()[-1].split()[1] != leased_ip:
            raise RuntimeError("original DHCP client did not retain its selected lease")
        new_events = stage / "new-client-events"
        new_hook = stage / "new-client-hook.sh"
        new_hook.write_text('#!/bin/sh\ncase "$reason" in BOUND|RENEW|REBIND) printf "%s %s\\n" "$reason" "$new_ip_address" >> "' + str(new_events) + '";; esac\n'); new_hook.chmod(0o500)
        # dhcpcd's control socket is keyed only by interface name.  Give the
        # independent client its own /run mount as real separate machines do.
        new_client = start("migration-new-client", ["unshare", "--mount", "--propagation", "private", "--fork", "sh", "-c", "mount -t tmpfs -o mode=755 tmpfs /run && exec \"$@\"", "sh", "dhcpcd", "-B", "-d", "-f", str(clientconf), "-c", str(new_hook), "eth0"], "new-client")
        try:
            wait(lambda: new_events.exists() and bool(new_events.read_text().strip()), 45)
        except RuntimeError as failure:
            raise RuntimeError("new DHCP client did not obtain a lease: " + (stage / "new-client.log").read_text(errors="replace")[-800:]) from failure
        if new_client.poll() is not None:
            raise RuntimeError("new DHCP client exited")
        new_ip = new_events.read_text().splitlines()[-1].split()[1]
        if new_ip == leased_ip:
            raise RuntimeError("new client reused the migrated lease")
        for transport in ([], ["+tcp"]):
            answer = run(inside("migration-client", ["dig", "@10.42.0.1", "migration.test", "A", "+short", *transport])).strip()
            if answer != "192.0.2.77":
                raise RuntimeError("DNS response did not traverse the new router")
        for ns, destination, source in (("migration-client", "192.0.2.1", leased_ip), ("migration-client", "fd00::1", "fd42::2"), ("migration-wan", leased_ip, "192.0.2.1"), ("migration-wan", "fd42::2", "fd00::1")):
            run(inside(ns, ["python3", "-c", ECHO_CLIENT, destination, source]), 20)
        wait(lambda: "18080" in (stage / "wan-capture.log").read_text(errors="replace") and "18081" in (stage / "wan-capture.log").read_text(errors="replace"), 15)
        run(controlssh + ["qm stop " + str(meta["vmid"])], 45)
        wait(lambda: "status: stopped" in run(controlssh + ["qm status " + str(meta["vmid"])]), 30)
        if any(digest(path) != expected for path, expected in former_source.items()):
            raise RuntimeError("new owner modified former-owner state")
        for command in (["ip", "addr", "add", "192.0.2.2/24", "dev", "wan0"], ["ip", "-6", "addr", "add", "fd00::2/64", "dev", "wan0"], ["ip", "addr", "add", "10.42.0.1/24", "dev", "lan0"], ["ip", "-6", "addr", "add", "fd42::1/64", "dev", "lan0"]):
            run(inside("migration-old", command))
        wait(lambda: not run(inside("migration-old", ["ip", "-6", "-o", "addr", "show", "tentative", "dev", "wan0"])).strip(), 20)
        revived_tail = start("migration-old", ["env", "SSL_CERT_FILE=" + str(cert), "TS_LOGS_DIR=" + str(old), str(stage / "tailscale/tailscaled"), "--state=" + str(oldstate), "--statedir=" + str(old), "--socket=" + str(oldsock), "--tun=tailscale0", "--port=41641", "--no-logs-no-support"], "revived-tailscaled")
        wait(oldsock.exists)
        revived_dhcp = start("migration-old", ["dnsmasq", "--keep-in-foreground", "--conf-file=/dev/null", "--no-resolv", "--no-hosts", "--interface=lan0", "--bind-interfaces", "--dhcp-authoritative", "--dhcp-leasefile=" + str(oldlease), "--dhcp-range=10.42.0.50,10.42.0.100,255.255.255.0,10m", "--dhcp-option=option:router,10.42.0.1", "--dhcp-option=option:dns-server,10.42.0.1", "--address=/migration.test/10.42.0.1"], "revived-dnsmasq")
        wait(lambda: json.loads(run(inside("migration-old", oldcli + ["status", "--json"]))).get("BackendState") == "Running", 45)
        wait(lambda: migrated_peer().get("Online"), 45)
        run(inside("migration-peer", peercli + ["ping", "--timeout=10s", product_tail]), 15)
        nodes = json.loads(run(inside("migration-wan", head + ["nodes", "list", "-o", "json"])))
        if sum(str(node.get("id")) == str(old_id) for node in nodes) != 1 or revived_tail.poll() is not None or revived_dhcp.poll() is not None:
            raise RuntimeError("former owner revival did not preserve one owner")
        print("PASS isolated initial-state migration: stopped owner, selected state imported, same Tailscale ID/ping, DHCP renewal and distinct client lease, DNS UDP/TCP, LAN/WAN IPv4/IPv6 TCP/UDP, then stopped new PID1 and revived unchanged former state with one owner", flush=True)
    finally:
        for process in reversed(relays + children):
            if process.poll() is None: process.terminate()
        for process in reversed(relays + children):
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired: process.kill(); process.wait()
        for ns in reversed(namespaces): subprocess.run(["ip", "netns", "del", ns], capture_output=True)
        shutil.rmtree(selected, ignore_errors=True)


if __name__ == "__main__":
    main()
