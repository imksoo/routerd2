#!/usr/bin/env python3
"""Exercise the appliance PPP image with independent RP-PPPoE and ISC DHCP peers.

The actual factory kernel/initramfs boots as PID 1 with two virtio NICs. Private
TAP bridges connect its WAN to the AC and its LAN to a separate Linux client.
Only fixture configuration/password/SSH public key are added to the initramfs.
Requires root/sudo, QEMU x86_64, pppoe-server, pppd, iproute2, tcpdump, curl,
OpenSSH, dnsmasq, dig, mke2fs and an independent ISC dhcpd supplied with
--dhcp6-server. A fresh regular-file ext4 disk supplies explicit persistent state.
"""
import argparse
import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import time


def peer(stage, power_action):
    children = []
    names = []

    def run(a, timeout=8, input=None):
        return subprocess.run(a, check=True, capture_output=True, text=True, timeout=timeout, input=input).stdout

    def inside(ns, a):
        return ['ip', 'netns', 'exec', ns] + a

    def start(ns, a, name):
        out = (stage / (name + '.log')).open('a')
        p = subprocess.Popen(inside(ns, a) if ns else a, stdout=out, stderr=subprocess.STDOUT)
        out.close()
        children.append(p)
        return p

    def wait(f, seconds=40):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if f():
                return
            time.sleep(0.3)
        raise RuntimeError('communication deadline')
    try:
        run(['mount', '--make-rprivate', '/'])
        run(['mount', '-t', 'tmpfs', 'tmpfs', '/run'])
        etc = stage / 'etc'
        etc.mkdir()
        (etc / 'options').write_text('')
        for name in ['pap-secrets', 'chap-secrets']:
            (etc / name).write_text('routerd2 * routerd2-fixture *\n')
            (etc / name).chmod(0o600)
        (etc / 'ipv6-up').write_text('#!/bin/sh\nexec /usr/sbin/ip -6 route replace 2001:db8:80::/56 dev "$1"\n')
        (etc / 'ipv6-up').chmod(0o700)
        run(['mount', '--bind', str(etc), '/etc/ppp'])
        ac = 'qac' + str(os.getpid())
        lan = 'qlan' + str(os.getpid())
        for ns in [ac, lan]:
            run(['ip', 'netns', 'add', ns])
            names.append(ns)
            run(['ip', '-n', ns, 'link', 'set', 'lo', 'up'])
        for tap, br, ns, dev in [('tapwan', 'wanbr', ac, 'server0'), ('taplan', 'lanbr', lan, 'client0')]:
            run(['ip', 'link', 'add', br, 'type', 'bridge'])
            run(['ip', 'link', 'set', br, 'up'])
            run(['ip', 'tuntap', 'add', tap, 'mode', 'tap'])
            run(['ip', 'link', 'set', tap, 'master', br])
            run(['ip', 'link', 'set', tap, 'up'])
            run(['ip', 'link', 'add', br + 'p', 'type', 'veth', 'peer', 'name', dev, 'netns', ns])
            run(['ip', 'link', 'set', br + 'p', 'master', br])
            run(['ip', 'link', 'set', br + 'p', 'up'])
            run(['ip', '-n', ns, 'link', 'set', dev, 'up'])
        run(['ip', '-n', lan, 'addr', 'add', '10.42.0.2/24', 'dev', 'client0'])
        run(['ip', '-n', lan, 'route', 'add', 'default', 'via', '10.42.0.1'])
        run(['ip', '-n', ac, 'addr', 'add', '198.51.100.20/32', 'dev', 'lo'])
        run(['ip', '-n', ac, '-6', 'addr', 'add', '2001:db8:ffff::20/128', 'dev', 'lo'])
        run(inside(ac, ['sysctl', '-qw', 'net.ipv6.conf.all.forwarding=1']))
        (stage / 'server.conf').write_text('auth\nrequire-chap\nrefuse-pap\n+ipv6\nmtu 1454\nmru 1454\nms-dns 192.0.2.1\nlogfd 1\nhide-password\nlcp-echo-interval 1\nlcp-echo-failure 3\n')
        start(ac, ['pppoe-server', '-F', '-k', '-I', 'server0', '-C', 'ROUTERD2-AC', '-S', 'ROUTERD2-LAB', '-L', '192.0.2.1', '-R', '192.0.2.2', '-N', '1', '-O', str(stage / 'server.conf')], 'ac')
        for addr in ['198.51.100.20', '2001:db8:ffff::20']:
            start(ac, ['python3', '-u', '-m', 'http.server', '18080', '--bind', addr, '--directory', str(etc)], 'http')
            family = 'AF_INET6' if ':' in addr else 'AF_INET'
            code = f'import socket\ns=socket.socket(socket.{family},socket.SOCK_DGRAM);s.bind(({addr!r},18081))\nwhile True:\n b,a=s.recvfrom(4096);s.sendto(b,a)'
            start(ac, ['python3', '-u', '-c', code], 'udp')
        wan_capture = start(None, ['tcpdump', '--immediate-mode', '-Z', 'root', '-U', '-n', '-i', 'tapwan', '-w', str(stage / 'wan.pcap')], 'capture')
        lan_capture = start(None, ['tcpdump', '--immediate-mode', '-Z', 'root', '-U', '-n', '-i', 'taplan', '-w', str(stage / 'lan.pcap')], 'lan-capture')
        overlay = stage / 'overlay'
        (overlay / 'etc/routerd').mkdir(parents=True)
        run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(stage / 'key')])
        boot = json.loads((stage / 'boot-base.json').read_text())
        boot['kernel_modules'] = json.loads((stage / 'input/manifest.json').read_text())['kernel_modules']
        boot['ssh']['authorized_keys'] = [(stage / 'key.pub').read_text().strip()]
        boot['state_volume'] = {'device': '/dev/vda', 'wait_timeout_seconds': 10}
        disk = stage / 'state.ext4'
        with disk.open('xb') as out:
            out.truncate(256 * 1024 * 1024)
        disk.chmod(0o600)
        run(['mke2fs', '-q', '-F', '-t', 'ext4', '-E', 'lazy_itable_init=0,lazy_journal_init=0', str(disk)], 20)
        (overlay / 'etc/routerd/boot.json').write_text(json.dumps(boot))
        (overlay / 'etc/routerd/password').write_text('routerd2-fixture\n')
        (overlay / 'etc/routerd/password').chmod(0o600)
        service = dict(interface='eth0', ppp_interface='ppp-wan', username='routerd2', password_file='/etc/routerd/password', auth_method='chap', ipv6=True, service_name='ROUTERD2-LAB', ac_name='ROUTERD2-AC', mtu=1454, mru=1454, lcp_echo_interval=1, lcp_echo_failure=3)
        resources = [{'id': 'wan', 'interface': {'name': 'eth0'}}, {'id': 'lan', 'interface': {'name': 'eth1'}}, {'id': 'lan-address', 'address': {'interface': 'eth1', 'cidr': '10.42.0.1/24'}}, {'id': 'forwarding', 'forwarding': {'ipv4': True, 'ipv6': True, 'ipv4_rp_filter': 0}}, {'id': 'firewall', 'firewall': {'forward': [{'from': 'eth1', 'to': 'ppp-wan'}], 'masquerade': [{'source': '10.42.0.0/24', 'out_interface': 'ppp-wan'}]}}, {'id': 'internet', 'service': {'name': 'internet', 'pppoe': service}}, {'id': 'pd', 'depends_on': ['internet', 'lan'], 'when': {'fact': 'service.internet.active'}, 'service': {'name': 'pd', 'dhcp6': {'interface': 'ppp-wan', 'iaid': 1, 'request_prefix': True, 'prefix_length': 60, 'delegations': [{'name': 'lan', 'interface': 'eth1', 'subnet_id': 1, 'advertise': True}]}}}]
        resources.append({"id":"dns","service":{"name":"dns","dns":{"spec":{"listen":[{"name":"lan","addresses":["10.42.0.1"],"port":53}],"sources":[{"name":"provider","kind":"forward","match":["."],"upstreamFrom":[{"resource":"PPPoESession/internet","field":"dnsServers"}],"queryTimeout":"1s"}]}}}})
        start(ac,["dnsmasq","--keep-in-foreground","--conf-file=/dev/null","--no-hosts","--no-resolv","--bind-dynamic","--listen-address=192.0.2.1","--address=/ppp.example/198.51.100.77","--local-ttl=0"],"dns")
        (overlay / 'etc/routerd/config.json').write_text(json.dumps({'schema_version': 2, 'resources': resources}))
        spec = importlib.util.spec_from_file_location('mk', stage / 'mkinitramfs.py')
        mk = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mk)
        mk.write_archive(overlay, stage / 'overlay.gz', 0)
        shutil.copyfile(stage / 'input/initramfs.cpio.gz', stage / 'initrd.gz')
        with (stage / 'initrd.gz').open('ab') as out:
            out.write((stage / 'overlay.gz').read_bytes())
        shutil.copyfile(stage / 'input/vmlinuz', stage / 'vmlinuz')
        q = start(None, ['qemu-system-x86_64', '-machine', 'q35', '-accel', 'tcg', '-cpu', 'max', '-m', '512M', '-smp', '2', '-display', 'none', '-monitor', 'none', '-nodefaults', '-serial', 'file:' + str(stage / 'serial.log'), '-kernel', str(stage / 'vmlinuz'), '-initrd', str(stage / 'initrd.gz'), '-drive', 'file=' + str(disk) + ',if=virtio,format=raw', '-append', 'console=ttyS0,115200 panic=-1', '-netdev', 'tap,id=wan,ifname=tapwan,script=no,downscript=no', '-device', 'virtio-net-pci,netdev=wan,mac=52:54:00:12:34:56', '-netdev', 'tap,id=lan,ifname=taplan,script=no,downscript=no', '-device', 'virtio-net-pci,netdev=lan,mac=52:54:00:12:34:57'], 'qemu')
        print('Private appliance peer stage', stage, flush=True)

        def booted():
            if q.poll() is not None:
                raise RuntimeError('QEMU exited')
            text = (stage / 'serial.log').read_text(errors='replace') if (stage / 'serial.log').exists() else ''
            if 'Kernel panic' in text:
                raise RuntimeError('guest panic')
            return 'ROUTERD_READY' in text
        wait(booted, 75)
        ssh = ['ssh', '-F', '/dev/null', '-i', str(stage / 'key'), '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'UserKnownHostsFile=' + str(stage / 'known-hosts'), '-o', 'ConnectTimeout=2', 'admin@10.42.0.1']

        def query(cmd):
            return json.loads(run(inside(lan, ssh + [cmd]), 12))

        def pd_ready():
            return any(x['name'] == 'service.pd' and x.get('snapshot', {}).get('state') == 'bound' for x in query('show services')['instances']) and query('show config')['healthy']
        status = query('show status')
        assert status['pid'] == 1 and status['platform'] == 'appliance'
        expected_release = json.loads((stage / 'input/manifest.json').read_text())['release_id']
        assert status['release_id'] == expected_release
        assert all(m['release_id'] == expected_release for m in status['modules'] if m['required'])

        def traffic():
            for addr in ['198.51.100.20', '[2001:db8:ffff::20]']:
                assert run(inside(lan, ['curl', '--noproxy', '*', '--max-time', '4', '-sS', '-o', '/dev/null', '-w', '%{http_code}', f'http://{addr}:18080/'])) == '200'
            for family, addr in [('AF_INET', '198.51.100.20'), ('AF_INET6', '2001:db8:ffff::20')]:
                run(inside(lan, ['python3', '-c', f'import socket;s=socket.socket(socket.{family},socket.SOCK_DGRAM);s.settimeout(3);s.connect(({addr!r},18081));s.send(b"actual-appliance");assert s.recv(100)==b"actual-appliance"']))

            for transport in ([],["+tcp"]):
                assert run(inside(lan,["dig","@10.42.0.1","peer.ppp.example","A","+tries=1","+time=2","+short"]+transport)).strip()=="198.51.100.77"

        def ppp_ready():
            return bool([x for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp')])
        wait(ppp_ready)
        dhcpd = str(stage / 'dhcpd')
        (stage / 'dhcp6.conf').write_text('default-lease-time 18; preferred-lifetime 12; option dhcp-renewal-time 5; option dhcp-rebinding-time 9; subnet6 2001:db8:ffff::/64 { prefix6 2001:db8:80:: 2001:db8:80:f0:: /60; }\n')
        (stage / 'dhcp6.leases').touch()
        mac = json.loads(run(['ip', '-n', ac, '-j', 'link', 'show', 'dev', 'server0']))[0]['address']
        with (stage / 'dhcp6.conf').open('a') as out:
            out.write('server-duid LL ethernet ' + mac + ';\n')

        def server():
            ppp = next((x['ifname'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp')))
            wait(lambda: any((a.get('scope') == 'link' and (not a.get('tentative')) for x in json.loads(run(['ip', '-n', ac, '-j', '-6', 'addr', 'show', 'dev', ppp])) for a in x['addr_info'])))
            run(['ip', '-n', ac, '-6', 'addr', 'replace', '2001:db8:ffff::1/64', 'dev', ppp])
            wait(lambda: any((a.get('scope') == 'global' and (not a.get('tentative')) for x in json.loads(run(['ip', '-n', ac, '-j', '-6', 'addr', 'show', 'dev', ppp])) for a in x['addr_info'])))
            return start(ac, [dhcpd, '-6', '-f', '-d', '-cf', str(stage / 'dhcp6.conf'), '-lf', str(stage / 'dhcp6.leases'), '-pf', str(stage / 'dhcp6.pid'), ppp], 'dhcp6')
        d = server()

        def client_ready():
            return any((a.get('scope') == 'global' and (not a.get('tentative')) for x in json.loads(run(['ip', '-n', lan, '-j', '-6', 'addr', 'show', 'dev', 'client0'])) for a in x['addr_info']))
        wait(client_ready)
        traffic()
        print('PASS factory appliance: CHAP, IPv4/IPv6 TCP/UDP, ISC PD and independent Linux SLAAC client', flush=True)
        before = query('show services pd')['snapshot']['renew_at']
        wait(lambda: query('show services pd')['snapshot']['renew_at'] != before, 15)
        traffic()
        print('PASS appliance DHCPv6 Renew preserved forwarding', flush=True)
        if power_action == 'reboot':
            resources[1]['interface']['mtu'] = 1400
            request = {'expected_revision': query('show config')['revision'], 'request_id': 'appliance-persistent-mtu', 'config': {'schema_version': 2, 'resources': resources}}
            committed = json.loads(run(inside(lan, ssh + ['configure']), 12, json.dumps(request)))
            assert committed['revision'] == request['expected_revision'] + 1
            wait(pd_ready)
            traffic()
        old = next((x for x in query('show status')['modules'] if x['name'] == 'service.internet'))
        old_pd = query('show services pd')['generation']
        dns_generation=next(x['generation'] for x in query('show status')['modules'] if x['name']=='service.dns')
        old_index = next((x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp')))
        releases = (stage / 'dhcp6.log').read_text().count('releases prefix ')
        duid = query('show services pd')['snapshot']['client_duid']
        revision = query('show config')['revision']
        restart_at = time.time()
        assert query('restart service.internet')['accepted']
        wait(lambda: any((x['name'] == 'service.internet' and x['generation'] > old['generation'] and x['ready'] for x in query('show status')['modules'])), 40)
        wait(lambda: any((x['ifname'].startswith('ppp') and x['ifindex'] != old_index for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])))))
        assert (stage / 'dhcp6.log').read_text().count('releases prefix ') == releases, 'operator restart released the PD lease'
        d.terminate()
        d.wait(timeout=5)
        d = server()
        wait(lambda: any((x['name'] == 'service.pd' and x.get('generation', 0) > old_pd and (x.get('snapshot', {}).get('state') == 'bound') for x in query('show services')['instances'])) and query('show config')['healthy'])
        wait(client_ready)
        traffic()
        print('PASS appliance PPP worker/session restart reacquired PD and restored IPv4/IPv6 TCP/UDP', flush=True)
        assert next(x['generation'] for x in query('show status')['modules'] if x['name']=='service.dns')==dns_generation
        assert query('show services pd')['snapshot']['client_duid'] == duid
        assert query('show config')['revision'] == revision
        before_control = {x['name']: x for x in query('show status')['modules']}
        control_peer_index = next(x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp'))
        releases = (stage / 'dhcp6.log').read_text().count('releases prefix ')
        control_restart_at = time.time()
        assert query('restart control')['accepted']
        wait(lambda: any(x['name'] == 'control' and x['generation'] > before_control['control']['generation'] and x['ready'] for x in query('show status')['modules']), 60)
        wait(lambda: any(x['name'] == 'service.internet' and x['generation'] > before_control['service.internet']['generation'] and x['ready'] for x in query('show status')['modules']))
        assert (stage / 'dhcp6.log').read_text().count('releases prefix ') == releases, 'control restart released the PD lease'
        wait(lambda: any(x['ifname'].startswith('ppp') and x['ifindex'] != control_peer_index for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show']))))
        assert next(x for x in query('show status')['modules'] if x['name'] == 'network')['pid'] == before_control['network']['pid']
        assert next(x for x in query('show status')['modules'] if x['name'] == 'management')['pid'] == before_control['management']['pid']
        d.terminate()
        d.wait(timeout=5)
        d = server()
        wait(pd_ready)
        wait(client_ready)
        traffic()
        assert query('show services pd')['snapshot']['client_duid'] == duid
        assert query('show config')['revision'] == revision
        print('PASS control restart withdrew old PD before PPP/control replacement, retained network/management and restored both-family TCP/UDP/DNS', flush=True)
        for capture in (wan_capture, lan_capture):
            capture.send_signal(signal.SIGINT)
            capture.wait(timeout=3)
        def packets(name, since):
            records = []
            for line in run(['tcpdump', '-nn', '-tt', '-vv', '-r', str(stage / name)]).splitlines():
                if line and line[0].isdigit():
                    records.append(line)
                elif records:
                    records[-1] += ' ' + line.strip()
            return [(float(line.split()[0]), line) for line in records if float(line.split()[0]) >= since]
        def withdrawal_order(wan_name, lan_name, since):
            wan_packets = packets(wan_name, since)
            assert not any('dhcp6 release' in line for _, line in wan_packets), 'normal stop released the PD lease'
            padts = [stamp for stamp, line in wan_packets if 'PPPoE PADT' in line]
            withdrawals = [stamp for stamp, line in packets(lan_name, since) if 'router advertisement' in line and 'router lifetime 0s' in line and 'valid time 0s, pref. time 0s' in line]
            assert withdrawals and padts and min(withdrawals) < min(padts), 'PPP teardown preceded RA withdrawal'
        withdrawal_order('wan.pcap', 'lan.pcap', restart_at)
        withdrawal_order('wan.pcap', 'lan.pcap', control_restart_at)
        print('PASS actual appliance RA withdrawal before PADT without Release; stable DUID, configuration and DNS worker', flush=True)
        (stage / 'status.json').write_text(json.dumps(query('show status')))
        shutdown_captures = [start(None, ['tcpdump', '--immediate-mode', '-Z', 'root', '-U', '-n', '-i', nic, '-w', str(stage / name)], name)
                             for nic, name in [('tapwan', 'shutdown-wan.pcap'), ('taplan', 'shutdown-lan.pcap')]]
        wait(lambda: all((stage / name).exists() and (stage / name).stat().st_size >= 24 for name in ('shutdown-wan.pcap', 'shutdown-lan.pcap')))
        power_at = time.time()
        power_status = query('show status')
        power_index = next(x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp'))
        assert query(power_action)['accepted']
        if power_action == 'poweroff':
            assert q.wait(timeout=40) == 0, 'QEMU failed during poweroff'
        else:
            def rebooted():
                if q.poll() is not None:
                    raise RuntimeError('QEMU exited during reboot')
                text = (stage / 'serial.log').read_text(errors='replace')
                assert 'Kernel panic' not in text
                return text.count('Linux version ') >= 2 and text.rfind('ROUTERD_READY') > text.rfind('Linux version ')
            wait(rebooted, 75)
        serial = (stage / 'serial.log').read_text(errors='replace')
        assert 'Kernel panic' not in serial
        marker = 'Power down' if power_action == 'poweroff' else 'Restarting system'
        assert marker in serial, 'kernel did not perform requested ' + power_action
        for capture in shutdown_captures:
            capture.send_signal(signal.SIGINT)
            capture.wait(timeout=3)
        withdrawal_order('shutdown-wan.pcap', 'shutdown-lan.pcap', power_at)
        print('PASS appliance CLI ' + power_action + ': RA withdrawal and PADT without Release before kernel power action', flush=True)
        if power_action == 'reboot':
            wait(lambda: any(x['ifname'].startswith('ppp') and x['ifindex'] != power_index for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show']))))
            d.terminate()
            d.wait(timeout=5)
            d = server()
            wait(pd_ready)
            wait(client_ready)
            traffic()
            assert query('show status')['instance_id'] != power_status['instance_id']
            config = query('show config')
            assert config['revision'] == revision
            assert next(r for r in config['config']['resources'] if r['id'] == 'lan')['interface']['mtu'] == 1400
            assert query('show services pd')['snapshot']['client_duid'] == duid
            print('PASS second appliance boot: ext4 persisted committed revision/MTU, DUID and pinned SSH host key; new PPP/PD restored both-family TCP/UDP/DNS', flush=True)
            assert query('poweroff')['accepted']
            assert q.wait(timeout=40) == 0
            assert 'Power down' in (stage / 'serial.log').read_text(errors='replace')

    except Exception:
        if 'query' in locals() and q.poll() is None:
            for command in ('show status', 'show config', 'show services', 'show logs level INFO limit 30'):
                try:
                    print('Failure diagnostic ' + command + ': ' + json.dumps(query(command)), flush=True)
                except Exception as error:
                    print('Failure diagnostic unavailable: ' + str(error), flush=True)
        raise
    finally:
        for p in reversed(children):
            if p.poll() is None:
                p.terminate()
                try:
                    p.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait(timeout=5)
        for ns in reversed(names):
            for pid in run(['ip', 'netns', 'pids', ns]).split():
                try:
                    os.kill(int(pid), signal.SIGTERM)
                except ProcessLookupError:
                    pass
            run(['ip', 'netns', 'delete', ns])
        print('Diagnostic stage:', stage, flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-dir", type=Path, default=Path("dist/image"))
    parser.add_argument("--output", type=Path, default=Path("dist/pppoe-appliance"))
    parser.add_argument("--dhcp6-server", type=Path)
    parser.add_argument("--inside", type=Path)
    parser.add_argument("--power-action", choices=("poweroff", "reboot"), default="poweroff")
    args = parser.parse_args()
    if args.inside:
        def deadline(*_):
            raise TimeoutError("appliance PPP communication deadline")
        def terminated(*_):
            raise KeyboardInterrupt()
        signal.signal(signal.SIGALRM, deadline)
        signal.signal(signal.SIGTERM, terminated)
        signal.alarm(180)
        peer(args.inside, args.power_action)
        return
    if args.dhcp6_server is None or not args.dhcp6_server.is_file():
        parser.error("--dhcp6-server must name an independent ISC dhcpd executable")
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    def run(command):
        return subprocess.run(root + command, check=True, text=True, capture_output=True, timeout=10).stdout
    stage = run(["mktemp", "-d", "/tmp/routerd-pppoe-appliance.XXXXXX"]).strip()
    args.output.mkdir(parents=True, exist_ok=True)
    try:
        run(["mkdir", "-m", "700", stage + "/input"])
        for source, target in [
            (Path(__file__), "peer.py"),
            (Path(__file__).parent.parent / "images/mkinitramfs.py", "mkinitramfs.py"),
            (Path(__file__).parent.parent / "images/boot.json", "boot-base.json"),
            (args.dhcp6_server, "dhcpd"),
            *[(args.image_dir / name, "input/" + name) for name in ("vmlinuz", "initramfs.cpio.gz", "manifest.json")],
        ]:
            run(["install", "-m", "500", str(source.resolve()), stage + "/" + target])
        with (args.output / "peer.log").open("w") as output:
            result = subprocess.run(root + ["unshare", "--mount", "--net", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", stage, "--power-action", args.power_action], stdout=output, stderr=subprocess.STDOUT, timeout=240)
        for name in ("serial.log", "wan.pcap", "lan.pcap", "shutdown-wan.pcap", "shutdown-lan.pcap", "status.json"):
            artifact = subprocess.run(root + ["cat", stage + "/" + name], capture_output=True, timeout=10)
            if artifact.returncode == 0:
                (args.output / name).write_bytes(artifact.stdout)
        if result.returncode:
            raise RuntimeError("appliance PPP failed; see " + str(args.output / "peer.log"))
        print("PASS appliance PPP/PD/TCP/UDP/Renew/session recovery; " + str(args.output / "peer.log"))
    finally:
        run(["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
