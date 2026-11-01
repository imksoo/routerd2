#!/usr/bin/env python3
"""Exercise two current appliance images through an actual isolated PVE HA failover.

Each run creates only two tagged, 4 vCPU/4 GiB VMs with private QEMU stream
NICs.  Independent Linux namespaces provide the LAN DHCP client and WAN TCP
and UDP peers.  No PVE bridge, existing VM, or production NIC is used.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import pwd
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import time


def call(command, timeout=30, input=None):
    result = subprocess.run(command, input=input, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError("%s: %s%s" % (command[0], result.stdout, result.stderr))
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pve-host", required=True)
    parser.add_argument("--pve-jump")
    parser.add_argument("--pve-identity", type=Path)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--stability-seconds", type=int, default=0,
                        help="keep both recovered appliances under independent traffic for this duration")
    parser.add_argument("--profile", choices=("ipv4", "egress"), default="ipv4")
    parser.add_argument("--dhcp6-server", type=Path, default=Path("/usr/sbin/dhcpd"),
                        help="independent ISC DHCPv6 server used by the egress profile")
    args = parser.parse_args()
    if args.stability_seconds < 0:
        parser.error("--stability-seconds must be non-negative")
    if args.profile == "egress" and not args.dhcp6_server.is_file():
        parser.error("--dhcp6-server must name an independent ISC dhcpd executable")
    networks = ("wan", "lan", "sync", "backup") if args.profile == "egress" else ("wan", "lan", "sync")
    if os.geteuid() != 0:
        raise SystemExit("run through sudo so the fixture can create its private network")
    image = args.image_dir.resolve()
    for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json"):
        if not (image / name).is_file():
            raise SystemExit("missing appliance image input: " + str(image / name))

    owner = os.environ.get("SUDO_USER") or pwd.getpwuid(os.getuid()).pw_name
    home = Path(pwd.getpwnam(owner).pw_dir)
    key = args.pve_identity or home / ".ssh/id_ed25519"
    known_hosts = home / ".ssh/known_hosts"
    ssh = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes",
           "-o", "UserKnownHostsFile=" + str(known_hosts), "-o", "GlobalKnownHostsFile=/dev/null",
           "-o", "IdentitiesOnly=yes", "-i", str(key)]
    if args.pve_jump:
        jump = args.pve_jump if "@" in args.pve_jump else owner + "@" + args.pve_jump
        ssh += ["-o", "ProxyCommand=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" +
                shlex.quote(str(known_hosts)) + " -o GlobalKnownHostsFile=/dev/null -o IdentitiesOnly=yes -i " +
                shlex.quote(str(key)) + " -W %h:%p " + shlex.quote(jump)]
    ssh += [args.pve_host]
    def remote(command, timeout=30):
        return call(ssh + [command], timeout)

    stage = Path(tempfile.mkdtemp(prefix="routerd2-pve-ha."))
    control_dir = Path(tempfile.mkdtemp(prefix="routerd2-pve-ha-ssh."))
    children, namespaces, taps, bridges, vmids = [], [], [], [], []
    remote_stage = ""
    master = control_dir / "master"
    relays = {"a": [], "b": []}
    serial = {}

    def run(command, timeout=30):
        return call(command, timeout)

    def start(command, name):
        out = (stage / (name + ".log")).open("a")
        process = subprocess.Popen(command, stdout=out, stderr=subprocess.STDOUT)
        out.close()
        children.append(process)
        return process

    def ns(name, command):
        return ["ip", "netns", "exec", name] + command

    def wait(check, seconds=60):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            if check():
                return
            time.sleep(0.25)
        raise TimeoutError("communication deadline")

    def ssh_control(command, timeout=30):
        return run(["ssh", "-S", str(master), "-o", "BatchMode=yes", args.pve_host, command], timeout)

    def node_config(node, index):
        other = "b" if node == "a" else "a"
        local = index + 2
        peer = 3 - index
        replication = {"listen_address": f"169.254.42.{local}:8443", "peer_address": f"169.254.42.{peer}:8443",
            "certificate_file": f"/etc/routerd/ha/{node}.crt", "private_key_file": f"/etc/routerd/ha/{node}.key",
            "peer_certificate_file": f"/etc/routerd/ha/{other}.crt"}
        resources = [
            {"id": "lan", "interface": {"name": "eth1"}},
            {"id": "wan", "interface": {"name": "eth0"}},
            {"id": "sync", "interface": {"name": "eth2"}},
            {"id": "lan-base", "address": {"interface": "eth1", "cidr": f"198.18.0.{local}/24"}},
            {"id": "sync-base", "address": {"interface": "eth2", "cidr": f"169.254.42.{local}/16"}},
            {"id": "forward", "forwarding": {"ipv4": True, "ipv4_rp_filter": 0, "conntrack_tcp_liberal": True}},
            {"id": "ha", "service": {"name": "ha", "vrrp": {
                "interface": "eth1", "family": "ipv4", "source_address": f"198.18.0.{local}",
                "unicast_peers": [f"198.18.0.{peer}"], "vip_interface": "lan-ha", "virtual_router_id": 42,
                "priority": 150 - index * 50, "preempt": False, "advert_interval_centiseconds": 100,
                "addresses": ["198.18.0.1/24"], "accept_mode": True,
                "conntrack_sync": {"interface": "eth2", "local_address": f"169.254.42.{local}", "peer_address": f"169.254.42.{peer}"}
            }}},
            {"id": "leases", "service": {"name": "leases", "dhcp_server": {
                "interface": "lan-ha", "ingress_interface": "eth1", "server_address": "198.18.0.1", "authoritative": True,
                "primary": {"prefix": "198.18.0.0/24", "start": "198.18.0.100", "end": "198.18.0.110", "lease_seconds": 60,
                            "router": "198.18.0.1", "dns_servers": ["198.18.0.1"]},
                "ha": {"owner": "ha", "replication": replication}
            }}},
            {"id": "wan-identity", "when": {"fact": "service.ha.elected"}, "link": {"name": "wan-ha", "kind": "macvlan", "parent": "eth0", "hardware_addr": "02:00:5e:00:01:43", "ipv6_addr_gen_mode": "none"}},
            {"id": "wan-interface", "interface": {"name": "wan-ha"}},
            {"id": "wan-address", "address": {"interface": "wan-ha", "cidr": "203.0.113.1/24"}},
            {"id": "firewall", "firewall": {"forward_chain": {"policy": "accept"}}},
            {"id": "nat", "nat": [{"type": "snat", "match": {"family": 4, "source": "198.18.0.0/24", "out_interface": "wan-ha"}, "address": "203.0.113.1"}]},
            {"id": "wan-route", "route": {"destination": "203.0.113.50/32", "interface": "wan-ha"}},
        ]
        if args.profile == "ipv4":
            resources.insert(4, {"id": "wan-base", "address": {"interface": "eth0", "cidr": f"203.0.113.{local}/24"}})
            return {"schema_version": 2, "resources": resources}
        resources[5]["forwarding"].update(ipv6=True, ipv4_ecmp_hash="source")
        resources[6]["service"]["vrrp"]["graceful_activation"] = {"ready_when": {"any": [
            {"all": [{"fact": "service.pd.active"}, {"fact": "service.health-primary.active"}]},
            {"fact": "service.health-backup.active"},
        ]}, "timeout": "1s"}
        pd_replication = dict(replication, listen_address=f"169.254.42.{local}:8444", peer_address=f"169.254.42.{peer}:8444")
        resources += [
            {"id": "wan-linklocal", "when": {"fact": "service.ha.elected"}, "address": {"interface": "wan-ha", "cidr": "fe80::1/64"}},
            {"id": "wan6-route", "when": {"fact": "service.ha.elected"}, "route": {"destination": "2001:db8:ffff::/64", "interface": "wan-ha", "gateway": "fe80::2"}},
            {"id": "pd", "service": {"name": "pd", "dhcp6": {"interface": "wan-ha", "request_prefix": True, "prefix_length": 60, "client_duid": "0003000102005e000143", "iaid": 7,
                "ha": {"owner": "ha", "replication": pd_replication}, "delegations": [
                    {"name": "lan", "interface": "eth1", "subnet_id": 1},
                    {"name": "b4", "interface": "wan-ha", "subnet_id": 1, "host_id": 33, "prefix_length": 128},
                ]}}},
            {"id": "pd-lan-ra", "when": {"fact": "service.ha.active"}, "service": {"name": "pd-lan-ra", "ra": {"interface": "eth1", "other_config": True, "router_lifetime_seconds": 10, "dns_lifetime_seconds": 1800, "min_interval_seconds": 3, "max_interval_seconds": 10, "from": {"service": "pd", "delegation": "lan"}}}},
            {"id": "lan-info", "when": {"fact": "service.ha.active"}, "service": {"name": "lan-info", "dhcp6_server": {
                "interface": "eth1", "server_duid": "000400112233445546778899aabbccddeeff", "dns_from": ["pd"], "search_from": ["pd"], "domain_search": ["local.test"], "refresh_seconds": 600
            }}},
            {"id": "backup", "interface": {"name": "eth3"}},
            {"id": "backup-parent-address", "address": {"interface": "eth3", "cidr": f"192.0.2.{local}/24"}},
            {"id": "backup-identity", "when": {"fact": "service.ha.elected"}, "link": {"name": "backup-ha", "kind": "macvlan", "parent": "eth3", "hardware_addr": "02:00:5e:00:01:44"}},
            {"id": "backup-interface", "interface": {"name": "backup-ha"}},
            {"id": "backup-address", "address": {"interface": "backup-ha", "cidr": "192.0.2.1/24"}},
            {"id": "internet", "when": {"fact": "service.pd.active"}, "ready_when": {"fact": "service.internet.active"}, "service": {"name": "internet", "ds_lite": {
                "interface": "wan-ha", "tunnel": "dslite0", "source_from": "pd", "source_delegation": "b4", "candidates": ["2001:db8:ffff::20"],
                "b4_peer": "192.0.0.1/32", "route_gateway": "192.0.0.1", "table": 110, "mtu": 1460}}},
            {"id": "backup-default", "route": {"destination": "0.0.0.0/0", "gateway": "192.0.2.50", "interface": "backup-ha", "table": 120}},
            {"id": "probe-primary", "policy_rule": {"family": 4, "priority": 110, "table": 110, "mark": 41}},
            {"id": "probe-backup", "policy_rule": {"family": 4, "priority": 120, "table": 120, "mark": 42}},
            {"id": "health-primary", "depends_on": ["internet", "probe-primary"], "when": {"fact": "service.internet.active"}, "service": {"name": "health-primary", "health": {
                "interface": "dslite0", "source_address": "192.0.0.2", "target": "203.0.113.50", "protocol": "tcp", "port": 18082, "mark": 41,
                "interval_ms": 200, "timeout_ms": 200, "healthy_threshold": 2, "unhealthy_threshold": 2}}},
            {"id": "health-backup", "depends_on": ["probe-backup"], "service": {"name": "health-backup", "health": {
                "interface": "backup-ha", "source_address": "192.0.2.1", "target": "203.0.113.50", "protocol": "tcp", "port": 18082, "mark": 42,
                "interval_ms": 200, "timeout_ms": 200, "healthy_threshold": 2, "unhealthy_threshold": 2}}},
            {"id": "egress", "egress_route": {"destination": "0.0.0.0/0", "table": 100, "metric": 100, "candidates": [
                {"preference": 120, "when": {"fact": "service.health-primary.active"}, "next_hop": {"interface": "dslite0", "gateway": "192.0.0.1", "weight": 1}},
                {"preference": 60, "when": {"fact": "service.health-backup.active"}, "next_hop": {"interface": "backup-ha", "gateway": "192.0.2.50", "weight": 1}},
            ]}},
            {"id": "lan-egress", "policy_rule": {"family": 4, "priority": 1000, "table": 100, "source": "198.18.0.0/24"}},
        ]
        next(resource for resource in resources if resource["id"] == "wan-address")["address"]["cidr"] = "203.0.113.1/32"
        next(resource for resource in resources if resource["id"] == "nat")["nat"] = [
            {"type": "snat", "match": {"family": 4, "source": "0.0.0.0/0", "out_interface": interface}, "address": address, "connection_mark": mark}
            for interface, address, mark in (("dslite0", "203.0.113.1", 41), ("backup-ha", "192.0.2.1", 42))
        ]
        resources = [resource for resource in resources if resource["id"] != "wan-route"]
        return {"schema_version": 2, "resources": resources}

    def write_node(node, index):
        root = stage / (node + "-overlay")
        (root / "etc/routerd/ha").mkdir(parents=True, mode=0o700)
        (root / "etc/routerd/config.json").write_text(json.dumps(node_config(node, index)))
        shutil.copyfile(stage / (node + ".crt"), root / "etc/routerd/ha" / (node + ".crt"))
        shutil.copyfile(stage / (node + ".key"), root / "etc/routerd/ha" / (node + ".key"))
        shutil.copyfile(stage / (("b" if node == "a" else "a") + ".crt"), root / "etc/routerd/ha" / (("b" if node == "a" else "a") + ".crt"))
        (root / "etc/routerd/ha" / (node + ".key")).chmod(0o600)
        boot = json.loads((Path("images/boot.json")).read_text())
        roles = ("logging", "network", "control", "management", "vrrp", "dhcp_server")
        if args.profile == "egress":
            roles += ("dhcp6", "dhcp6_server", "ra", "aftr", "health")
        boot["workers"] = {name: path for name, path in boot["workers"].items() if name in roles}
        boot["engines"] = {name: boot["engines"][name] for name in ("vrrp", "dhcp_server")}
        boot["state_volume"] = {"device": "/dev/vda", "wait_timeout_seconds": 10}
        boot["ssh"]["authorized_keys"] = [(stage / "management.pub").read_text().strip()]
        (root / "etc/routerd/boot.json").write_text(json.dumps(boot))
        spec = importlib.util.spec_from_file_location("mk", "images/mkinitramfs.py")
        mk = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mk)
        overlay = stage / (node + ".overlay.gz")
        mk.write_archive(root, overlay, 0)
        initrd = stage / (node + ".initrd.gz")
        shutil.copyfile(image / "initramfs.cpio.gz", initrd)
        with initrd.open("ab") as out:
            out.write(overlay.read_bytes())
        disk = stage / (node + ".state.ext4")
        with disk.open("xb") as out:
            out.truncate(256 * 1024 * 1024)
        run(["mke2fs", "-q", "-F", "-t", "ext4", "-E", "lazy_itable_init=0,lazy_journal_init=0", str(disk)])

    def start_node(node):
        for relay in relays[node]:
            if relay.poll() is None:
                relay.terminate()
                relay.wait(timeout=5)
        relays[node] = []
        vmid = vmids[0 if node == "a" else 1]
        ssh_control("qm start " + str(vmid), 60)
        paths = [remote_stage + "/" + node + "-" + network + ".sock" for network in networks]
        def sockets_ready():
            result = subprocess.run(
                ["ssh", "-S", str(master), "-o", "BatchMode=yes", args.pve_host,
                 "test " + " -a ".join("-S " + shlex.quote(path) for path in paths)],
                capture_output=True,
                timeout=10,
            )
            return result.returncode == 0
        wait(sockets_ready, 45)
        for network in networks:
            relay = start([sys.executable, "-I", "scripts/qemu-stream-tap.py", "--tap", "tap-" + node + "-" + network,
                           "--socket", str(control_dir / (node + "-" + network + ".sock"))], "relay-" + node + "-" + network)
            relays[node].append(relay)
        (stage / ("serial-" + node + ".log")).write_text("")
        serial[node] = start(["ssh", "-S", str(master), "-o", "BatchMode=yes", args.pve_host,
                              "tail --pid=$(cat /run/qemu-server/" + str(vmid) + ".pid) -c +1 -f " + shlex.quote(remote_stage + "/" + node + ".serial.log")], "serial-" + node)

    def stop_node(node):
        vmid = vmids[0 if node == "a" else 1]
        ssh_control("qm stop " + str(vmid) + " --skiplock --overrule-shutdown --timeout 0", 60)
        wait(lambda: "stopped" in ssh_control("qm status " + str(vmid), 10), 45)
        for relay in relays[node]:
            if relay.poll() is None:
                relay.terminate()
                relay.wait(timeout=5)
        relays[node] = []

    try:
        remote_stage = remote("mktemp -d /tmp/routerd2-ha-pve.XXXXXX").strip()
        for node, index in (("a", 0), ("b", 1)):
            call(["openssl", "req", "-x509", "-newkey", "ed25519", "-nodes", "-keyout", str(stage / (node + ".key")), "-out", str(stage / (node + ".crt")), "-days", "1", "-subj", "/CN=" + node, "-addext", "subjectAltName=IP:169.254.42." + str(index + 2), "-addext", "extendedKeyUsage=serverAuth,clientAuth"])
            (stage / (node + ".key")).chmod(0o600)
        call(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(stage / "management")])
        for node, index in (("a", 0), ("b", 1)):
            write_node(node, index)
        vmids = []
        forwards = []
        for node in ("a", "b"):
            for network in networks:
                forwards += ["-L", str(control_dir / (node + "-" + network + ".sock")) + ":" + remote_stage + "/" + node + "-" + network + ".sock"]
        command = ssh[:-1] + ["-M", "-N", "-f", "-S", str(master), "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15", *forwards, args.pve_host]
        subprocess.run(command, check=True, timeout=30)
        for node in ("a", "b"):
            vmid = int(json.loads(remote("pvesh get /cluster/nextid --output-format json")))
            vmids.append(vmid)
            name = "routerd2-ha-pve-" + node + "-" + str(int(time.time()))
            remote(shlex.join(["qm", "create", str(vmid), "--name", name, "--tags", "routerd2-test", "--description", "Temporary isolated routerd2 HA acceptance; no PVE bridge or production NIC", "--machine", "q35", "--cpu", "host", "--cores", "4", "--sockets", "1", "--memory", "4096", "--balloon", "0", "--kvm", "1", "--ostype", "l26", "--vga", "none", "--tablet", "0", "--onboot", "0"]), 60)
            for local, target in ((image / "vmlinuz", "vmlinuz"), (stage / (node + ".initrd.gz"), node + ".initrd.gz"), (stage / (node + ".state.ext4"), node + ".state.ext4")):
                with local.open("rb") as source:
                    subprocess.run(["ssh", "-S", str(master), "-o", "BatchMode=yes", args.pve_host,
                                    "umask 077; cat > " + shlex.quote(remote_stage + "/" + target)], stdin=source, check=True, timeout=180)
            qargs = ["-kernel", remote_stage + "/vmlinuz", "-initrd", remote_stage + "/" + node + ".initrd.gz", "-append", "console=ttyS0,115200 panic=-1", "-serial", "file:" + remote_stage + "/" + node + ".serial.log", "-drive", "file=" + remote_stage + "/" + node + ".state.ext4,if=virtio,format=raw"]
            for network, suffix in (("wan", "01"), ("lan", "02"), ("sync", "03"), ("backup", "04")):
                if network not in networks:
                    continue
                mac = "52:54:00:42:" + ("01" if node == "a" else "02") + ":" + suffix
                qargs += ["-netdev", "stream,id=" + network + ",server=on,addr.type=unix,addr.path=" + remote_stage + "/" + node + "-" + network + ".sock", "-device", "virtio-net-pci,netdev=" + network + ",mac=" + mac]
            remote(shlex.join(["qm", "set", str(vmid), "--args", shlex.join(qargs)]), 60)
        for bridge in ("ha-pve-" + network for network in networks):
            subprocess.run(["ip", "link", "del", bridge], capture_output=True)
            run(["ip", "link", "add", bridge, "type", "bridge"])
            run(["ip", "link", "set", bridge, "up"])
            bridges.append(bridge)
        for node in ("a", "b"):
            for network in networks:
                tap = "tap-" + node + "-" + network
                bridge = "ha-pve-" + network
                run(["ip", "tuntap", "add", tap, "mode", "tap"])
                run(["ip", "link", "set", tap, "master", bridge])
                run(["ip", "link", "set", tap, "up"])
                taps.append(tap)
        peers = [("ha-pve-client", "ha-pve-lan", "lan0", "hpc0"), ("ha-pve-server", "ha-pve-wan", "wan0", "hps0")]
        if args.profile == "egress":
            peers.append(("ha-pve-backup", "ha-pve-backup", "backup0", "hpb0"))
        for namespace, bridge, device, host in peers:
            subprocess.run(["ip", "link", "del", host], capture_output=True)
            subprocess.run(["ip", "netns", "del", namespace], capture_output=True)
            run(["ip", "netns", "add", namespace])
            namespaces.append(namespace)
            run(["ip", "-n", namespace, "link", "set", "lo", "up"])
            run(["ip", "link", "add", host, "type", "veth", "peer", "name", device, "netns", namespace])
            run(["ip", "link", "set", host, "master", bridge])
            run(["ip", "link", "set", host, "up"])
            run(["ip", "-n", namespace, "link", "set", device, "up"])
        if args.profile == "ipv4":
            run(ns("ha-pve-server", ["ip", "address", "add", "203.0.113.50/24", "dev", "wan0"]))
        else:
            run(ns("ha-pve-server", ["ip", "address", "add", "203.0.113.50/32", "dev", "lo"]))
            run(ns("ha-pve-server", ["ip", "-6", "address", "add", "fe80::2/64", "dev", "wan0"]))
            run(ns("ha-pve-server", ["ip", "-6", "address", "add", "2001:db8:ffff::20/64", "dev", "wan0"]))
            run(ns("ha-pve-server", ["ip", "-6", "route", "add", "2001:db8:80::/56", "via", "fe80::1", "dev", "wan0"]))
            run(ns("ha-pve-backup", ["ip", "address", "add", "192.0.2.50/24", "dev", "backup0"]))
            run(ns("ha-pve-backup", ["ip", "address", "add", "203.0.113.50/32", "dev", "lo"]))
        for network in ("lan", "sync") + (("wan", "backup") if args.profile == "egress" else ()):
            start(["tcpdump", "--immediate-mode", "-Z", "root", "-U", "-C", "64", "-W", "1", "-n", "-i", "tap-a-" + network,
                   "-w", str(stage / (network + ".pcap"))], "capture-" + network)
        (stage / "peer.py").write_text('''import socket,sys,threading
label=sys.argv[1].encode()
def echo(port, udp=False):
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM if udp else socket.SOCK_STREAM)
 s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("203.0.113.50",port))
 if udp:
  while True:
   message,peer=s.recvfrom(4096); s.sendto(message,peer)
 s.listen()
 while True:
  client,_=s.accept()
  while True:
   message=client.recv(4096)
   if not message: break
   client.sendall(message)
def marker(port, udp=False):
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM if udp else socket.SOCK_STREAM)
 s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("203.0.113.50",port))
 if udp:
  while True:
   _,peer=s.recvfrom(4096); s.sendto(label,peer)
 s.listen()
 while True:
  client,_=s.accept(); client.sendall(label); client.close()
for function,port,udp in ((echo,18080,False),(echo,18081,True),(marker,18082,False),(marker,18083,True)):
 threading.Thread(target=function,args=(port,udp),daemon=True).start()
threading.Event().wait()
''')
        start(ns("ha-pve-server", ["python3", "-u", str(stage / "peer.py"), "DSLite"]), "peer-primary")
        if args.profile == "egress":
            start(ns("ha-pve-backup", ["python3", "-u", str(stage / "peer.py"), "backup"]), "peer-backup")
            mac = json.loads(run(ns("ha-pve-server", ["ip", "-j", "link", "show", "dev", "wan0"])))[0]["address"]
            (stage / "dhcp6.conf").write_text("default-lease-time 60;\npreferred-lifetime 40;\noption dhcp-renewal-time 20;\noption dhcp-rebinding-time 40;\nsubnet6 2001:db8:ffff::/64 { prefix6 2001:db8:80:: 2001:db8:80:f0:: /60; }\nserver-duid LL ethernet " + mac + ";\n")
            (stage / "dhcp6.leases").touch()
            start(ns("ha-pve-server", [str(args.dhcp6_server), "-6", "-f", "-d", "-cf", str(stage / "dhcp6.conf"), "-lf", str(stage / "dhcp6.leases"), "-pf", str(stage / "dhcp6.pid"), "wan0"]), "dhcp6")
        start_node("a")
        start_node("b")
        wait(lambda: all("ROUTERD_READY" in (stage / ("serial-" + node + ".log")).read_text(errors="replace") for node in ("a", "b")), 120)
        hook = stage / "dhcp-client.sh"
        hook.write_text("#!/bin/sh\ncase \"$1\" in\nbound|renew)\n ip addr replace \"$ip/$mask\" dev \"$interface\"\n ip route replace default via \"$router\" dev \"$interface\"\n ;;\nesac\n")
        hook.chmod(0o500)
        dhcp = start(ns("ha-pve-client", ["busybox", "udhcpc", "-f", "-i", "lan0", "-s", str(hook), "-T", "2", "-t", "5", "-A", "2"]), "dhcp-client")
        def client_ready():
            result = subprocess.run(ns("ha-pve-client", ["ip", "-j", "addr", "show", "dev", "lan0"]), capture_output=True, text=True)
            return result.returncode == 0 and any(a.get("local", "").startswith("198.18.0.1") for x in json.loads(result.stdout) for a in x.get("addr_info", []) if a.get("family") == "inet")
        wait(client_ready, 60)
        def appliance(node, command):
            address = "198.18.0." + ("2" if node == "a" else "3")
            return json.loads(run(ns("ha-pve-client", ["ssh", "-F", "/dev/null", "-i", str(stage / "management"), "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + str(stage / "known-hosts"), "-o", "ConnectTimeout=3", "admin@" + address, command]), 10))
        wait(lambda: appliance("a", "show status")["ready"] and appliance("b", "show status")["ready"], 45)
        (stage / "continuous.py").write_text('''import socket,time,sys\ns=socket.create_connection(("203.0.113.50",18080),4); s.settimeout(15)\nu=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); u.settimeout(1); u.connect(("203.0.113.50",18081))\nwhile True:\n m=("p%f"%time.monotonic()).encode(); s.sendall(m); assert s.recv(len(m))==m; u.send(m)\n try: assert u.recv(100)==m\n except TimeoutError: pass\n print(time.monotonic(),flush=True); time.sleep(.2)\n''')
        flow = start(ns("ha-pve-client", ["python3", "-u", str(stage / "continuous.py")]), "continuous")
        def flow_lines():
            if not (stage / "continuous.log").exists():
                return []
            return [line for line in (stage / "continuous.log").read_text().splitlines() if line.replace(".", "", 1).isdigit()]
        wait(lambda: flow.poll() is None and len(flow_lines()) >= 8, 30)
        sync_size = (stage / "sync.pcap").stat().st_size
        def synced_flow():
            if (stage / "sync.pcap").stat().st_size <= sync_size:
                return False
            result = subprocess.run(["tcpdump", "-nn", "-r", str(stage / "sync.pcap"), "udp"], capture_output=True, text=True)
            return result.returncode == 0 and bool(result.stdout.strip())
        wait(synced_flow, 15)
        first = len(flow_lines())
        stop_node("a")
        wait(lambda: flow.poll() is None and len(flow_lines()) >= first + 8, 45)
        print("PASS PVE appliance a -> b power cut retained the independent TCP socket and UDP traffic", flush=True)
        b_resync = (stage / "serial-b.log").read_text(errors="replace").count("resync requested by other node")
        start_node("a")
        wait(lambda: "ROUTERD_READY" in (stage / "serial-a.log").read_text(errors="replace"), 120)
        wait(lambda: (stage / "serial-b.log").read_text(errors="replace").count("resync requested by other node") > b_resync, 30)
        restarted = appliance("a", "show status")
        if not restarted["ready"]:
            raise RuntimeError("restarted appliance a is not ready: " + json.dumps(restarted))
        second = len(flow_lines())
        stop_node("b")
        wait(lambda: flow.poll() is None and len(flow_lines()) >= second + 8, 45)
        print("PASS PVE appliance b -> a power cut retained the independent TCP socket and UDP traffic", flush=True)
        if args.stability_seconds:
            a_resync = (stage / "serial-a.log").read_text(errors="replace").count("resync requested by other node")
            start_node("b")
            wait(lambda: "ROUTERD_READY" in (stage / "serial-b.log").read_text(errors="replace"), 120)
            wait(lambda: (stage / "serial-a.log").read_text(errors="replace").count("resync requested by other node") > a_resync, 30)
            stable = len(flow_lines())
            until = time.monotonic() + args.stability_seconds
            while time.monotonic() < until:
                if flow.poll() is not None:
                    raise RuntimeError("continuous traffic stopped during stability observation")
                time.sleep(min(5, until - time.monotonic()))
            if len(flow_lines()) <= stable or "renew" not in (stage / "dhcp-client.log").read_text():
                raise RuntimeError("stability observation did not retain traffic and DHCP renewal")
            print("PASS PVE appliance stability seconds " + str(args.stability_seconds), flush=True)
        for process in children:
            if process.poll() is not None and process is flow:
                raise RuntimeError("continuous independent traffic failed")
        for process in children:
            if process.args and isinstance(process.args, list) and process.args[0] == "tcpdump" and process.poll() is None:
                process.send_signal(signal.SIGINT)
                process.wait(timeout=5)
        sync = run(["tcpdump", "-nn", "-r", str(stage / "sync.pcap"), "udp"])
        lan = run(["tcpdump", "-nn", "-r", str(stage / "lan.pcap"), "udp port 67 or udp port 68"])
        if not sync.strip() or "DHCP" not in lan:
            raise RuntimeError("capture did not show conntrack synchronization and client DHCP")
        print("PASS captures showed appliance DHCP and conntrack replication", flush=True)
    except Exception:
        for name in ("continuous.log", "peer.log", "dhcp-client.log"):
            path = stage / name
            if path.exists():
                print("failure " + name, flush=True)
                print(path.read_text(errors="replace")[-4000:], flush=True)
        for node in ("a", "b"):
            if remote_stage:
                try:
                    print("failure serial " + node, flush=True)
                    print(remote("tail -n 80 " + shlex.quote(remote_stage + "/" + node + ".serial.log"), 15), flush=True)
                except Exception:
                    pass
        raise
    finally:
        for node in ("a", "b"):
            if node in relays:
                for relay in relays[node]:
                    if relay.poll() is None:
                        relay.terminate()
        for process in reversed(children):
            if process.poll() is None:
                process.terminate()
        for process in reversed(children):
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
        for namespace in reversed(namespaces):
            subprocess.run(["ip", "netns", "del", namespace], capture_output=True)
        for link in ("hpc0", "hps0"):
            subprocess.run(["ip", "link", "del", link], capture_output=True)
        for tap in reversed(taps):
            subprocess.run(["ip", "link", "del", tap], capture_output=True)
        for bridge in reversed(bridges):
            subprocess.run(["ip", "link", "del", bridge], capture_output=True)
        for vmid in vmids:
            try:
                remote("qm stop " + str(vmid) + " --skiplock", 60)
                remote("qm destroy " + str(vmid), 60)
            except Exception:
                pass
        if remote_stage:
            try:
                remote("python3 -c " + shlex.quote("import shutil; shutil.rmtree(" + repr(remote_stage) + ")"), 30)
            except Exception:
                pass
        if master.exists():
            subprocess.run(["ssh", "-S", str(master), "-O", "exit", args.pve_host], capture_output=True, timeout=15)
        shutil.rmtree(control_dir, ignore_errors=True)
        shutil.rmtree(stage, ignore_errors=True)


if __name__ == "__main__":
    main()
