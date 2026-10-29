#!/usr/bin/env python3
"""Exercise the appliance PPP image with independent RP-PPPoE and ISC DHCP peers.

The actual factory kernel/initramfs boots as PID 1 with two virtio NICs. Private
TAP bridges connect its WAN to the AC and its LAN to a separate Linux client.
Only fixture configuration/password/SSH public key are added to the initramfs.
Requires root/sudo, QEMU x86_64, pppoe-server, pppd, iproute2, tcpdump, curl,
OpenSSH, dhcpcd, dnsmasq, dig, mke2fs and an independent ISC dhcpd supplied with
--dhcp6-server. A fresh regular-file ext4 disk supplies explicit persistent state.
"""
import argparse
import datetime
import importlib.util
import ipaddress
import json
import os
import re
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import tarfile
import time


def peer(stage, power_action, updates, profile):
    children = []
    names = []

    def run(a, timeout=8, input=None):
        result = subprocess.run(a, capture_output=True, text=True, timeout=timeout, input=input)
        if result.returncode:
            raise RuntimeError(f'{a[0]} exited {result.returncode}: {result.stdout}{result.stderr}')
        return result.stdout

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
        run(['mount', '-t', 'tmpfs', 'tmpfs', '/var/lib/dhcpcd'])
        resolver = stage / 'client-resolv.conf'
        resolver.write_text('')
        target = Path('/etc/resolv.conf').resolve()
        if not target.exists():
            if not target.is_relative_to('/run'):
                raise RuntimeError('resolver target is absent outside private /run')
            target.parent.mkdir(parents=True, exist_ok=True)
            target.touch()
        run(['mount', '--bind', str(resolver), str(target)])
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
        # A separate management station remains available when client DHCP
        # startup is the failing behavior. The application client stays dynamic.
        run(['ip', 'address', 'add', '10.42.0.254/24', 'dev', 'lanbr'])
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
        # This fixture selects the PPP/DHCP profile; Tailscale/HA engines have
        # separate communication checks and are not part of these update bundles.
        boot['workers'].pop('tailscale', None)
        if profile != 'time':
            boot['workers'].pop('ntp', None)
        boot['engines'] = {role: boot['engines'][role] for role in (('pppoe', 'dhcp_server', 'ntp') if profile == 'time' else ('pppoe', 'dhcp_server'))}
        boot['kernel_modules'] = json.loads((stage / 'input/manifest.json').read_text())['kernel_modules']
        boot['ssh']['authorized_keys'] = [(stage / 'key.pub').read_text().strip()]
        boot['state_volume'] = {'device': '/dev/vda', 'wait_timeout_seconds': 10}
        disk = stage / 'state.ext4'
        with disk.open('xb') as out:
            out.truncate((1024 if updates else 256) * 1024 * 1024)
        disk.chmod(0o600)
        (overlay / 'etc/routerd/boot.json').write_text(json.dumps(boot))
        (overlay / 'etc/routerd/password').write_text('routerd2-fixture\n')
        (overlay / 'etc/routerd/password').chmod(0o600)
        service = dict(interface='eth0', ppp_interface='ppp-wan', username='routerd2', password_file='/etc/routerd/password', auth_method='chap', ipv6=True, service_name='ROUTERD2-LAB', ac_name='ROUTERD2-AC', mtu=1454, mru=1454, lcp_echo_interval=1, lcp_echo_failure=3)
        resources = [{'id': 'wan', 'interface': {'name': 'eth0'}}, {'id': 'lan', 'interface': {'name': 'eth1'}}, {'id': 'lan-address', 'address': {'interface': 'eth1', 'cidr': '10.42.0.1/24'}}, {'id': 'forwarding', 'forwarding': {'ipv4': True, 'ipv6': True, 'ipv4_rp_filter': 0}}, {'id': 'firewall', 'firewall': {'forward': [{'from': 'eth1', 'to': 'ppp-wan'}]}}, {'id': 'internet', 'service': {'name': 'internet', 'pppoe': service}}, {'id': 'pd', 'depends_on': ['internet', 'lan'], 'when': {'fact': 'service.internet.active'}, 'ready_when': {'fact': 'service.pd.active'}, 'service': {'name': 'pd', 'dhcp6': {'interface': 'ppp-wan', 'iaid': 1, 'request_prefix': True, 'prefix_length': 60, 'delegations': [{'name': 'lan', 'interface': 'eth1', 'subnet_id': 1}]}}}]
        resources.append({'id': 'nat', 'nat': [{'type': 'masquerade', 'match': {'family': 4, 'source': '10.42.0.0/24', 'out_interface': 'ppp-wan'}}]})
        resources.append({'id': 'pd-lan-ra', 'when': {'fact': 'service.pd.active'}, 'service': {'name': 'pd-lan-ra', 'ra': {'interface': 'eth1', 'router_lifetime_seconds': 1800, 'dns_lifetime_seconds': 1800, 'min_interval_seconds': 3, 'max_interval_seconds': 10, 'from': {'service': 'pd', 'delegation': 'lan'}}}})
        resources.append({"id":"dns","service":{"name":"dns","dns":{"spec":{"listen":[{"name":"lan","addresses":["10.42.0.1"],"port":53}],"sources":[{"name":"provider","kind":"forward","match":["."],"upstreamFrom":[{"resource":"PPPoESession/internet","field":"dnsServers"}],"queryTimeout":"1s"}]}}}})
        resources.append({'id': 'leases', 'service': {'name': 'leases', 'dhcp_server': {
            'interface': 'eth1', 'server_address': '10.42.0.1', 'authoritative': True, 'sticky_hold_days': 3,
            'primary': {'prefix': '10.42.0.0/24', 'start': '10.42.0.50', 'end': '10.42.0.100',
                        'lease_seconds': 120, 'router': '10.42.0.1', 'dns_servers': ['10.42.0.1']}}}})
        resources.append({'id': 'uplink-health', 'depends_on': ['internet'], 'when': {'fact': 'service.internet.active'}, 'service': {'name': 'uplink-health', 'health': {
            'interface': 'ppp-wan', 'target': '198.51.100.20', 'protocol': 'tcp', 'port': 18080,
            'interval_ms': 500, 'timeout_ms': 300, 'healthy_threshold': 1, 'unhealthy_threshold': 2}}})
        start(ac,["dnsmasq","--keep-in-foreground","--conf-file=/dev/null","--no-hosts","--no-resolv","--bind-dynamic","--listen-address=192.0.2.1","--address=/ppp.example/198.51.100.20","--address=/ppp.example/2001:db8:ffff::20","--local-ttl=0"],"dns")
        if profile == 'time':
            resources.append({'id': 'time', 'service': {'name': 'time', 'ntp': {'sources': [{'servers': [], 'sources_from': ['pd']}], 'listen': ['10.42.0.1'], 'allow': ['10.42.0.0/24']}}})
            vendor = stage / 'chrony'
            with tarfile.open(stage / 'chrony-engine.tar') as archive:
                archive.extractall(vendor, filter='data')
            chrony = [str(vendor / 'lib/ld-musl-x86_64.so.1'), '--library-path', str(vendor / 'lib') + ':' + str(vendor / 'usr/lib'), str(vendor / 'usr/sbin/chronyd')]
            (stage / 'time-source').mkdir(mode=0o700)
            (stage / 'time-source.conf').write_text('local stratum 8\nbindaddress 2001:db8:ffff::20\nallow ::/0\ncmdport 0\nbindcmdaddress ' + str(stage / 'time-source/command.sock') + '\npidfile ' + str(stage / 'time-source/pid') + '\n')
            start(ac, chrony + ['-d', '-x', '-u', 'root', '-f', str(stage / 'time-source.conf')], 'time-source')
        (overlay / 'etc/routerd/config.json').write_text(json.dumps({'schema_version': 2, 'resources': resources}))
        mkfs = ['mke2fs', '-q', '-F', '-t', 'ext4', '-E', 'lazy_itable_init=0,lazy_journal_init=0']
        if updates:
            run([str(updates / 'base/routerd'), 'config', 'validate', str(overlay / 'etc/routerd/config.json')])
            signing = stage / 'release-signing.pem'
            run(['openssl', 'genpkey', '-algorithm', 'ED25519', '-out', str(signing)])
            signing.chmod(0o600)
            publisher = str(stage / 'publisher')
            trust = stage / 'release-keys.json'
            trust.write_text(run([publisher, 'trust', '--private-key-file', str(signing)]))
            trust.chmod(0o600)
            (overlay / 'etc/routerd/release-keys.json').write_text(trust.read_text())
            (overlay / 'etc/routerd/release-keys.json').chmod(0o600)
            bundles, packed, versions = {}, {}, {}
            roles = [{'role': role, 'artifact': 'routerd-logging' if role == 'logging' else 'routerd-modules'} for role in boot['workers']]
            for sequence, name in enumerate(('base', 'next', 'damaged'), 1):
                versions[name] = json.loads(run([str(updates / name / 'routerd'), 'version']))['release_id']
                layout = {'boot_abi': 1, 'worker_abi': 5, 'entrypoint': 'routerd', 'roles': roles,
                          'engines': [{'role': role, 'artifact': role + '-image', 'sources': role + '-sources'} for role in ('pppoe', 'dhcp_server')],
                          'artifacts': [{'name': command, 'kind': 'native', 'command': command, 'file': str(updates / name / command)}
                                        for command in ('routerd', 'routerd-modules', 'routerd-logging')] +
                                       [{'name': role + '-' + kind, 'kind': 'data', 'file': str(stage / file)}
                                        for role, kind, file in (('pppoe', 'image', 'pppoe-engine.tar'), ('pppoe', 'sources', 'pppoe-sources.tar'),
                                                                 ('dhcp_server', 'image', 'dnsmasq-engine.tar'), ('dhcp_server', 'sources', 'dnsmasq-sources.tar'))]}
                layoutfile = stage / (name + '-layout.json')
                layoutfile.write_text(json.dumps(layout))
                bundles[name] = stage / (name + '.tar')
                packed[name] = json.loads(run([publisher, 'pack', '--private-key-file', str(signing), '--layout', str(layoutfile),
                                              '--release-id', versions[name], '--sequence', str(sequence), '--target-arch', 'amd64',
                                              '--output', str(bundles[name])], 120))
            initial = stage / 'initial-state'
            initial.mkdir(mode=0o700)
            selector = ['--root', str(initial / 'releases'), '--trust-file', str(trust), '--target-arch', 'amd64']
            installer = str(updates / 'base/routerd')
            baseline = json.loads(run([installer, 'release', 'stage', *selector, '--file', str(bundles['base'])], 120))
            run([installer, 'release', 'initialize', *selector, '--digest', baseline['digest']], 120)
            boot.update(workers={}, engines={}, release={'root': '/var/lib/routerd/releases', 'trust_file': '/etc/routerd/release-keys.json',
                                                        'ready_timeout_seconds': 90, 'stable_seconds': 2})
            (overlay / 'etc/routerd/boot.json').write_text(json.dumps(boot))
            mkfs += ['-d', str(initial)]
        run(mkfs + [str(disk)], 30)
        spec = importlib.util.spec_from_file_location('mk', stage / 'mkinitramfs.py')
        mk = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mk)
        mk.write_archive(overlay, stage / 'overlay.gz', 0)
        shutil.copyfile(stage / 'input/initramfs.cpio.gz', stage / 'initrd.gz')
        with (stage / 'initrd.gz').open('ab') as out:
            out.write((stage / 'overlay.gz').read_bytes())
        shutil.copyfile(stage / 'input/vmlinuz', stage / 'vmlinuz')
        boot_started = time.monotonic()
        rtc = ['-rtc', 'base=' + (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(minutes=20)).strftime('%Y-%m-%dT%H:%M:%S') + ',clock=host'] if profile == 'time' else []
        q = start(None, ['qemu-system-x86_64', '-machine', 'q35', '-accel', 'tcg', '-cpu', 'max', '-m', '512M', '-smp', '2', '-display', 'none', '-monitor', 'none', '-nodefaults', '-serial', 'file:' + str(stage / 'serial.log'), '-kernel', str(stage / 'vmlinuz'), '-initrd', str(stage / 'initrd.gz'), '-drive', 'file=' + str(disk) + ',if=virtio,format=raw', '-append', 'console=ttyS0,115200 panic=-1', '-netdev', 'tap,id=wan,ifname=tapwan,script=no,downscript=no', '-device', 'virtio-net-pci,netdev=wan,mac=52:54:00:12:34:56', '-netdev', 'tap,id=lan,ifname=taplan,script=no,downscript=no', '-device', 'virtio-net-pci,netdev=lan,mac=52:54:00:12:34:57'] + rtc, 'qemu')
        print('Private appliance peer stage', stage, flush=True)
        ssh = ['ssh', '-F', '/dev/null', '-i', str(stage / 'key'), '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'UserKnownHostsFile=' + str(stage / 'known-hosts'), '-o', 'ConnectTimeout=2', 'admin@10.42.0.1']

        def query(cmd):
            return json.loads(run(ssh + [cmd], 12))

        def booted():
            if q.poll() is not None:
                raise RuntimeError('QEMU exited')
            text = (stage / 'serial.log').read_text(errors='replace') if (stage / 'serial.log').exists() else ''
            if 'Kernel panic' in text:
                raise RuntimeError('guest panic')
            return 'ROUTERD_READY' in text
        wait(booted, 180 if updates else 75)
        print('Observed appliance boot readiness seconds', round(time.monotonic() - boot_started, 2), flush=True)
        dhcp_events = stage / 'client-dhcp-events'
        hook = stage / 'client-dhcp.sh'
        hook.write_text('#!/bin/sh\ncase "$reason" in BOUND|RENEW|REBIND|REBOOT)\n'
                        'printf "%s %s %s %s\\n" "$reason" "$new_ip_address" "$new_routers" "$new_domain_name_servers" >> "' + str(dhcp_events) + '";;\nesac\n'
                        'resolvconf=/nonexistent; skip_hooks="01-test 30-hostname 50-ntp.conf 50-timesyncd.conf dhcpcd.enter-hook dhcpcd.exit-hook"\n'
                        'export resolvconf skip_hooks\nexec /usr/lib/dhcpcd/dhcpcd-run-hooks "$@"\n')
        hook.chmod(0o500)
        client_config = stage / 'client-dhcp.conf'
        client_config.write_text('ipv6rs\nslaac hwaddr\nnoipv4ll\noption domain_name_servers, domain_name, domain_search\n')
        start(lan, ['dhcpcd', '-B', '-d', '-f', str(client_config), '-c', str(hook), 'client0'], 'client-dhcp')
        wait(lambda: dhcp_events.exists() and bool(dhcp_events.read_text().splitlines()))
        lease = dhcp_events.read_text().splitlines()[-1].split()
        assert ipaddress.ip_address(lease[1]) in ipaddress.ip_network('10.42.0.0/24')
        assert lease[2:] == ['10.42.0.1', '10.42.0.1'], 'appliance DHCP did not supply its router and DNS'
        client_dns = lease[3]
        wait(lambda: 'nameserver ' + client_dns in resolver.read_text())
        print('PASS independent LAN DHCP and standard resolver hook acquired address/gateway/DNS from the packaged engine under PID 1', flush=True)
        def pd_ready():
            return any(x['name'] == 'service.pd' and x.get('snapshot', {}).get('state') == 'bound' for x in query('show services')['instances']) and query('show config')['healthy']
        status = None
        def required_workers_ready():
            nonlocal status
            status = query('show status')
            return status['ready']
        wait(required_workers_ready)
        assert status['pid'] == 1 and status['platform'] == 'appliance'
        expected_release = versions['base'] if updates else json.loads((stage / 'input/manifest.json').read_text())['release_id']
        assert status['release_id'] == expected_release
        assert all(m['release_id'] == expected_release for m in status['modules'] if m['required'])

        def traffic():
            for family in ('-4', '-6'):
                try:
                    assert run(inside(lan, ['curl', family, '--noproxy', '*', '--max-time', '4', '-sS', '-o', '/dev/null', '-w', '%{http_code}', 'http://peer.ppp.example:18080/'])) == '200'
                except Exception as error:
                    raise RuntimeError(f'LAN TCP {family} failed: {error}') from error
            for family in ('AF_INET', 'AF_INET6'):
                try:
                    run(inside(lan, ['python3', '-c', f'import socket;s=socket.socket(socket.{family},socket.SOCK_DGRAM);s.settimeout(3);s.connect(socket.getaddrinfo("peer.ppp.example",18081,socket.{family},socket.SOCK_DGRAM)[0][4]);s.send(b"actual-appliance");assert s.recv(100)==b"actual-appliance"']))
                except Exception as error:
                    raise RuntimeError(f'LAN UDP {family} failed: {error}') from error

            for transport in ([], ["+tcp"]):
                response = run(inside(lan, ["dig", "@" + client_dns, "peer.ppp.example", "A", "+tries=1", "+time=2", "+noall", "+comments", "+answer"] + transport))
                assert "status: NOERROR" in response and re.search(r"\sA\s+198\.51\.100\.20(?:\s|$)", response), response

        def ppp_ready():
            return bool([x for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp')])
        wait(ppp_ready)
        dhcpd = str(stage / 'dhcpd')
        (stage / 'dhcp6.conf').write_text('default-lease-time 18; preferred-lifetime 12; option dhcp-renewal-time 5; option dhcp-rebinding-time 9; subnet6 2001:db8:ffff::/64 { prefix6 2001:db8:80:: 2001:db8:80:f0:: /60; }\n')
        if profile == 'time':
            with (stage / 'dhcp6.conf').open('a') as out:
                out.write('option dhcp6.sntp-servers 2001:db8:ffff::20;\n')
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
            addresses = json.loads(run(['ip', '-n', lan, '-j', '-6', 'addr', 'show', 'dev', 'client0']))
            routes = json.loads(run(['ip', '-n', lan, '-j', '-6', 'route', 'show', 'default']))
            return any(a.get('scope') == 'global' and not a.get('tentative') and not a.get('deprecated') for link in addresses for a in link['addr_info']) and any(route.get('dev') == 'client0' and route.get('gateway') for route in routes)
        wait(client_ready)
        traffic()
        print('PASS factory appliance: CHAP, IPv4/IPv6 TCP/UDP, ISC PD and independent Linux SLAAC client', flush=True)
        before = query('show services pd')['snapshot']['renew_at']
        wait(lambda: query('show services pd')['snapshot']['renew_at'] != before, 15)
        traffic()
        print('PASS appliance DHCPv6 Renew preserved forwarding', flush=True)
        if profile == 'time':
            (stage / 'time-client.conf').write_text('server 10.42.0.1 iburst\ncmdport 0\n')
            time_observation = None
            time_started = time.monotonic()
            def time_ready():
                nonlocal time_observation
                snap = query('show services time').get('snapshot', {})
                # Use the peer's unchanged maxdistance and the clock engine's
                # maxupdateskew; initial synchronization alone is insufficient.
                qualified = (snap.get('synchronized') and snap.get('adjusting_clock')
                             and ipaddress.ip_address(snap['source']) == ipaddress.ip_address('2001:db8:ffff::20')
                             and snap['root_delay_seconds'] / 2 + snap['root_dispersion_seconds'] < 3
                             and snap['skew_ppm'] < 1000)
                current = (snap.get('reference_time'), qualified)
                if current != time_observation:
                    print('Observed time quality', {'elapsed': round(time.monotonic() - time_started, 2), 'synchronized': snap.get('synchronized'), 'skew_ppm': snap.get('skew_ppm'), 'root_distance_seconds': snap.get('root_delay_seconds', 0) / 2 + snap.get('root_dispersion_seconds', 0), 'qualified': bool(qualified)}, flush=True)
                    time_observation = current
                return qualified
            def time_client():
                result = subprocess.run(inside(lan, chrony + ['-Q', '-t', '8', '-u', 'root', '-f', str(stage / 'time-client.conf')]), capture_output=True, text=True, timeout=12)
                output = result.stdout + result.stderr
                assert result.returncode == 0, output
                match = re.search(r'System clock wrong by ([+-]?[0-9.]+) seconds', output)
                assert match and abs(float(match[1])) < 1, output
                print('Observed independent LAN NTP offset seconds', match[1], flush=True)
            wait(time_ready, 180)
            assert next(m for m in query('show status')['modules'] if m['name'] == 'service.time')['generation'] == 1
            time_client()
            serial = (stage / 'serial.log').read_text(errors='replace')
            steps = re.findall(r'System clock was stepped by ([+-]?[0-9.]+) seconds', serial)
            assert any(abs(float(value)) > 1000 for value in steps), 'no actual correction of the deliberately slow guest clock'
            print('PASS packaged PID1 appliance learned ISC DHCPv6 NTP, corrected its clock by over1000 seconds, and served independent LAN time; host clock untouched', flush=True)
        if power_action == 'reboot':
            resources[1]['interface']['mtu'] = 1400
            request = {'expected_revision': query('show config')['revision'], 'request_id': 'appliance-persistent-mtu', 'config': {'schema_version': 2, 'resources': resources}}
            configure_at = time.monotonic()
            committed = json.loads(run(ssh + ['configure'], 30, json.dumps(request)))
            print('Appliance configuration applied in', round(time.monotonic() - configure_at, 2), 'seconds', flush=True)
            assert committed['revision'] == request['expected_revision'] + 1
            wait(pd_ready)
            traffic()
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
        duid = query('show services pd')['snapshot']['client_duid']
        revision = query('show config')['revision']
        if profile == 'worker-recovery':
            old = next((x for x in query('show status')['modules'] if x['name'] == 'service.internet'))
            old_pd = query('show services pd')['generation']
            dns_generation=next(x['generation'] for x in query('show status')['modules'] if x['name']=='service.dns')
            old_index = next((x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp')))
            releases = (stage / 'dhcp6.log').read_text().count('releases prefix ')
            restart_at = time.time()
            restart_started = time.monotonic()
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
            print('Observed PPP restart communication seconds', round(time.monotonic() - restart_started, 2), flush=True)
            print('PASS appliance PPP worker/session restart reacquired PD and restored IPv4/IPv6 TCP/UDP', flush=True)
            assert next(x['generation'] for x in query('show status')['modules'] if x['name']=='service.dns')==dns_generation
            assert query('show services pd')['snapshot']['client_duid'] == duid
            assert query('show config')['revision'] == revision
            before_control = {x['name']: x for x in query('show status')['modules']}
            control_peer_index = next(x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp'))
            releases = (stage / 'dhcp6.log').read_text().count('releases prefix ')
            d.terminate()
            d.wait(timeout=5)
            late_solicits = start(None, ['tcpdump', '-l', '-nn', '-tt', '-vv', '-i', 'tapwan', '-c', '2',
                                        'pppoes and ip6 and udp src port 546 and ip6[48] = 1'], 'late-solicits')
            control_restart_at = time.time()
            control_restart_started = time.monotonic()
            assert query('restart control')['accepted']
            wait(lambda: any(x['name'] == 'control' and x['generation'] > before_control['control']['generation'] and x.get('pid', 0) > 1 for x in query('show status')['modules']), 60)
            wait(lambda: any(x['name'] == 'service.internet' and x['generation'] > before_control['service.internet']['generation'] and x['ready'] for x in query('show status')['modules']))
            assert (stage / 'dhcp6.log').read_text().count('releases prefix ') == releases, 'control restart released the PD lease'
            wait(lambda: any(x['ifname'].startswith('ppp') and x['ifindex'] != control_peer_index for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show']))))
            assert next(x for x in query('show status')['modules'] if x['name'] == 'network')['pid'] == before_control['network']['pid']
            assert next(x for x in query('show status')['modules'] if x['name'] == 'management')['pid'] == before_control['management']['pid']
            wait(lambda: late_solicits.poll() is not None)
            assert late_solicits.returncode == 0, (stage / 'late-solicits.log').read_text()
            transactions = re.findall(r'dhcp6 solicit \(xid=([0-9a-f]+)', (stage / 'late-solicits.log').read_text())
            assert len(transactions) == 2 and transactions[0] == transactions[1], 'PD worker changed exchange during peer outage'
            late_server_at = time.time()
            d = server()
            wait(lambda: any(x['name'] == 'control' and x['generation'] > before_control['control']['generation'] and x['ready'] for x in query('show status')['modules']))
            wait(pd_ready)
            wait(client_ready)
            recovered_at = time.time()
            # Cross a complete ISP lease while using the actual client route.
            # Waiting for an address alone hid withdrawal of the default router.
            while time.time() - recovered_at < 20:
                assert client_ready(), 'recovered client lost its usable IPv6 source/default route'
                traffic()
                time.sleep(1)
            assert not any('router advertisement' in line and 'router lifetime 0s' in line for _, line in packets('lan.pcap', recovered_at)), 'live PD was withdrawn again after control recovery'
            assert query('show services pd')['snapshot']['client_duid'] == duid
            assert query('show config')['revision'] == revision
            print('Observed control restart and 20s stability check seconds', round(time.monotonic() - control_restart_started, 2), flush=True)
            print('PASS control restart withdrew old PD before PPP/control replacement, retained network/management and restored both-family TCP/UDP/DNS', flush=True)
            for capture in (wan_capture, lan_capture):
                capture.send_signal(signal.SIGINT)
                capture.wait(timeout=3)
            withdrawal_order('wan.pcap', 'lan.pcap', restart_at)
            withdrawal_order('wan.pcap', 'lan.pcap', control_restart_at)
            later = packets('wan.pcap', late_server_at)
            offered = min(t for t, packet in later if 'dhcp6 advertise' in packet)
            requested = min(t for t, packet in later if t >= offered and 'dhcp6 request' in packet)
            print('Observed recovered ISC Advertise -> Request seconds', round(requested - offered, 3), flush=True)
            print('PASS actual appliance RA withdrawal before PADT without Release; stable DUID, configuration and DNS worker', flush=True)
        else:
            # Update acceptance starts from working client communication. Worker
            # restart faults remain the separate default communication profile.
            for capture in (wan_capture, lan_capture):
                capture.send_signal(signal.SIGINT)
                capture.wait(timeout=3)
        (stage / 'status.json').write_text(json.dumps(query('show status')))
        shutdown_captures = [start(None, ['tcpdump', '--immediate-mode', '-Z', 'root', '-U', '-n', '-i', nic, '-w', str(stage / name)] + capture_filter, name)
                             for nic, name, capture_filter in [('tapwan', 'shutdown-wan.pcap', []), ('taplan', 'shutdown-lan.pcap', ['icmp or icmp6 or port 53 or port 18080 or port 18081'])]]
        wait(lambda: all((stage / name).exists() and (stage / name).stat().st_size >= 24 for name in ('shutdown-wan.pcap', 'shutdown-lan.pcap')))
        if profile == 'update':
            initial_selection = query('update status')
            interrupted = subprocess.Popen(ssh + ['update stage'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            children.append(interrupted)
            with bundles['next'].open('rb') as source:
                interrupted.stdin.write(source.read(3 << 20))
                interrupted.stdin.flush()
            interrupted.terminate()
            interrupted.stdin.close()
            interrupted.wait(timeout=10)
            traffic()
            assert query('update status')['generation'] == initial_selection['generation'], 'interrupted upload changed selection'
            print('PASS SSH upload interruption preserved current selection and client communication', flush=True)

            uploading = subprocess.Popen(ssh + ['update stage'], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            children.append(uploading)
            health = query('show services uplink-health')
            with bundles['next'].open('rb') as source:
                # Exceed the former SSH input limit and ordinary HTTP read
                # deadline while actually using forwarding and management.
                for _ in range(4):
                    uploading.stdin.write(source.read(1 << 20))
                    uploading.stdin.flush()
                    traffic()
                    assert query('update status')['generation'] == initial_selection['generation']
                    time.sleep(4)
                shutil.copyfileobj(source, uploading.stdin)
            uploading.stdin.close()
            uploading.stdin = None
            output, diagnostic = uploading.communicate(timeout=300)
            assert uploading.returncode == 0, diagnostic
            staged = json.loads(output)
            assert staged['digest'] == packed['next']['digest']
            current_health = query('show services uplink-health')
            assert current_health['generation'] == health['generation'] and current_health['sequence'] > health['sequence'], 'upload stopped Health observations'
            traffic()
            selection = query('update status')
            request = {'release': staged, 'expected_generation': selection['generation'], 'expected_revision': query('show config')['revision']}
            prepare_started = time.monotonic()
            with tempfile.TemporaryFile(mode='w+') as preparation:
                json.dump(request, preparation)
                preparation.seek(0)
                preparing = subprocess.Popen(ssh + ['update prepare'], stdin=preparation, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                children.append(preparing)
                while preparing.poll() is None:
                    if time.monotonic() - prepare_started > 300:
                        raise RuntimeError('signed update preparation deadline')
                    assert query('show config')['revision'] == request['expected_revision']
                    traffic()
                    time.sleep(1)
                output, diagnostic = preparing.communicate(timeout=5)
            assert preparing.returncode == 0, diagnostic
            prepared = json.loads(output)
            print('Observed signed preparation seconds', round(time.monotonic() - prepare_started, 2), flush=True)
            assert prepared['pending']['release']['digest'] == staged['digest'] and prepared['active'] == selection['active']
            traffic()
            print('PASS signed SSH upload returned its result/exit status; forwarding/Health continued; target preflight prepared the next runtime/configuration pair', flush=True)
        power_at = time.time()
        power_status = query('show status')
        power_index = next(x['ifindex'] for x in json.loads(run(['ip', '-n', ac, '-j', 'link', 'show'])) if x['ifname'].startswith('ppp'))
        power_started = time.monotonic()
        if profile == 'time':
            for interface, name in [('tapwan', 'wan'), ('taplan', 'lan')]:
                start(None, ['tcpdump', '--immediate-mode', '-Z', 'root', '-U', '-n', '-i', interface, '-w', str(stage / ('time-reboot-' + name + '.pcap'))], 'time-reboot-' + name)
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
            wait(rebooted, 180 if updates else 75)
            print('Observed kernel/runtime reboot readiness seconds', round(time.monotonic() - power_started, 2), flush=True)
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
            assert config['revision'] == revision + (1 if profile == 'update' else 0)
            assert next(r for r in config['config']['resources'] if r['id'] == 'lan')['interface']['mtu'] == 1400
            assert query('show services pd')['snapshot']['client_duid'] == duid
            print('PASS second appliance boot: ext4 persisted committed revision/MTU, DUID and pinned SSH host key; new PPP/PD restored both-family TCP/UDP/DNS', flush=True)
            if profile == 'time':
                time_observation = None
                time_started = time.monotonic()
                wait(time_ready, 180)
                time_client()
                console = (stage / 'serial.log').read_text(errors='replace')
                time_generation = next(m for m in query('show status')['modules'] if m['name'] == 'service.time')['generation']
                print('Observed second-boot time worker generation', time_generation, flush=True)
                assert 'read from /state/drift' in console[console.rfind('Linux version '):], 'chronyd did not restore persisted drift on second boot'
                print('PASS second appliance boot restored persisted chronyd drift, learned source, actual clock adjustment and LAN time', flush=True)
                history_before = query('show event-store')['capacity']
                health_before = query('show services uplink-health')['snapshot']
                coexist_started = time.monotonic()
                rounds = 0
                max_management = 0
                unhealthy_samples = 0
                while time.monotonic() - coexist_started < 60:
                    traffic()
                    time_client()
                    response_started = time.monotonic()
                    current_status = query('show status')
                    current_config = query('show config')
                    current_health = query('show services uplink-health')['snapshot']
                    current_store = query('show event-store')
                    max_management = max(max_management, time.monotonic() - response_started)
                    assert current_status['ready'] and current_health['active']
                    if not current_config['healthy']:
                        unhealthy_samples += 1
                        services = current_config['services']
                        print('Observed configuration health alarm', {'errors': {key: current_config[key] for key in ('last_error', 'event_journal_error', 'event_store_error', 'event_rule_error', 'derived_error', 'protocol_event_error') if current_config.get(key)}, 'service_phase': services['phase'], 'service_error': services.get('last_error', '')}, flush=True)
                    rounds += 1
                    print('Observed history/communication coexistence', {'utc': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'round': rounds, 'elapsed': round(time.monotonic() - coexist_started, 2), 'management_seconds': round(time.monotonic() - response_started, 2), 'configuration_healthy': current_config['healthy'], 'health_observed_nanos': current_health['observed_nanos'], 'tip': current_store['capacity']['tip_cursor'], 'archived_rows': current_store['capacity']['archived_events']['rows'], 'history_error': current_store.get('last_error', ''), 'health_history_dropped': current_health['history_dropped'], 'health_history_error': current_health.get('history_error', '')}, flush=True)
                (stage / 'control-records.json').write_text(json.dumps(query('show logs module control level WARN limit 200')))
                assert unhealthy_samples == 0, f'{unhealthy_samples} unhealthy configuration observations during steady communication'
                assert current_config['healthy'] and not current_store.get('last_error')
                assert current_health['observed_nanos'] > health_before['observed_nanos']
                assert current_health['history_dropped'] == health_before['history_dropped'] and not current_health.get('history_error'), current_health
                assert current_store['capacity']['tip_cursor'] > history_before['tip_cursor']
                assert current_store['capacity']['archived_events']['rows'] > history_before['archived_events']['rows']
                print('PASS advancing archived history with independent IPv4/IPv6 TCP/UDP,DNS,NTP,current Health and management; rounds', rounds, 'maximum combined management seconds', round(max_management, 2), flush=True)
                assert time_generation == 1, f'NTP worker restarted during startup: generation {time_generation}'
            if profile == 'update':
                assert query('show status')['release_id'] == versions['next']
                print('Observed selected trial supervisor: ' + json.dumps(query('show status')), flush=True)
                trial_health = None
                trial_log_sequence = 0
                def update_confirmed():
                    nonlocal trial_health, trial_log_sequence
                    config = query('show config')
                    services = config['services']
                    current = (config['healthy'], config['revision'], services['phase'], services.get('last_error'))
                    if current != trial_health:
                        print('Observed update trial health', round(time.monotonic() - power_started, 2), current, flush=True)
                        trial_health = current
                    records = query('show logs module supervisor level INFO limit 200')['records']
                    for record in records:
                        if record['sequence'] > trial_log_sequence and record['message'].startswith('runtime update trial'):
                            print('Observed update trial audit', record['timestamp'], record['message'], record.get('attributes'), flush=True)
                        trial_log_sequence = max(trial_log_sequence, record['sequence'])
                        if record['message'] == 'runtime update confirmed' and record.get('attributes', {}).get('release') == versions['next']:
                            return True
                    return False
                wait(update_confirmed, 120)
                assert query('update status').get('pending') is None
                assert query('update status')['active']['digest'] == staged['digest']
                print('PASS SSH-prepared signed update booted and confirmed with automatic client DNS, IPv4/IPv6 traffic and retained DUID/SSH identity', flush=True)
            if profile in ('update', 'corrupt-recovery'):
                with bundles['damaged'].open('rb') as source:
                    uploaded = subprocess.run(ssh + ['update stage'], stdin=source, capture_output=True, text=True, timeout=330)
                assert uploaded.returncode == 0, uploaded.stderr
                damaged = json.loads(uploaded.stdout)
                selection = query('update status')
                known_good = selection['active']
                known_release = query('show status')['release_id']
                recovery_revision = query('show config')['revision']
                request = {'release': damaged, 'expected_generation': selection['generation'], 'expected_revision': recovery_revision}
                json.loads(run(ssh + ['update prepare'], 300, json.dumps(request)))
            assert query('poweroff')['accepted']
            assert q.wait(timeout=40) == 0
            assert 'Power down' in (stage / 'serial.log').read_text(errors='replace')
            if profile in ('update', 'corrupt-recovery'):
                # PID 1 syncs before poweroff; ext4 may still need journal
                # replay. debugfs must not edit blocks that a later replay
                # would overwrite. Replay only the journal, then require a
                # clean filesystem before and after changing the executable.
                replay = subprocess.run(['e2fsck', '-p', '-E', 'journal_only', str(disk)], capture_output=True, text=True, timeout=30)
                assert replay.returncode in (0, 1), replay.stdout + replay.stderr
                run(['e2fsck', '-f', '-n', str(disk)], 30)
                path = '/releases/releases/' + damaged['digest'] + '/routerd'
                broken = stage / 'damaged-entrypoint'
                run(['debugfs', '-R', 'dump -p ' + path + ' ' + str(broken), str(disk)], 30)
                with broken.open('r+b') as file:
                    file.seek(4096)
                    value = file.read(1)
                    file.seek(4096)
                    file.write(bytes([value[0] ^ 1]))
                run(['debugfs', '-w', '-R', 'rm ' + path, str(disk)], 30)
                run(['debugfs', '-w', '-R', 'write ' + str(broken) + ' ' + path, str(disk)], 30)
                run(['debugfs', '-w', '-R', 'set_inode_field ' + path + ' mode 0100500', str(disk)], 30)
                run(['e2fsck', '-f', '-n', str(disk)], 30)
                (stage / 'serial.log').unlink()
                q = start(None, q.args, 'qemu-recovery')
                wait(booted, 180)
                wait(ppp_ready)
                d.terminate()
                d.wait(timeout=5)
                d = server()
                wait(pd_ready, 90)
                wait(client_ready)
                traffic()
                recovered = query('update status')
                print('Observed rejected-candidate selection: ' + json.dumps(recovered), flush=True)
                assert recovered['active'] == known_good and recovered.get('pending') is None
                assert recovered.get('last_failure'), 'corrupt trial left no durable failure'
                assert query('show status')['release_id'] == known_release
                assert query('show services pd')['snapshot']['client_duid'] == duid
                # Rejection precedes candidate execution and startup adoption.
                # The already adopted known-good configuration stays unchanged.
                assert query('show config')['revision'] == recovery_revision
                assert query('poweroff')['accepted']
                assert q.wait(timeout=40) == 0
                print('PASS corrupt signed trial automatically fell back to the confirmed runtime/configuration; SSH identity, DUID and automatic DNS/IPv4/IPv6 TCP/UDP recovered', flush=True)

    except Exception as failure:
        print('FAIL appliance communication: ' + repr(failure), flush=True)
        for command in (['ip', '-n', lan, '-j', 'address'], ['ip', '-n', lan, '-j', 'route'], ['ip', '-n', lan, '-j', '-6', 'route']):
            try:
                print('Failure client kernel ' + ' '.join(command) + ': ' + run(command), flush=True)
            except Exception as error:
                print('Failure client kernel unavailable: ' + str(error), flush=True)
        if 'query' in locals() and q.poll() is None:
            for command in ('show network routes', 'show network interfaces', 'show status', 'show config', 'show services', 'show logs module supervisor level INFO limit 60', 'show logs module kernel level WARN limit 60', 'show logs module control level INFO limit 30', 'show logs module service.pd level INFO limit 40', 'show logs module service.leases level INFO limit 30'):
                try:
                    print('Failure diagnostic ' + command + ': ' + json.dumps(query(command)), flush=True)
                except Exception as error:
                    print('Failure diagnostic unavailable: ' + str(error), flush=True)
        if profile == 'update' and 'power_started' in locals() and q.poll() is None:
            # Preserve the actual trial failure, including its bounded journal
            # cleanup, before fixture cleanup terminates the guest.
            until = time.monotonic() + 60
            while q.poll() is None and time.monotonic() < until:
                console = (stage / 'serial.log').read_text(errors='replace')
                if 'runtime update trial failed:' in console:
                    print(console[console.rfind('runtime update trial failed:'):].strip(), flush=True)
                    break
                time.sleep(0.25)
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
    parser.add_argument("--update-binaries", type=Path, help="base/next/damaged static command sets for signed SSH updates")
    parser.add_argument("--profile", choices=("update", "worker-recovery", "corrupt-recovery", "time"))
    args = parser.parse_args()
    args.profile = args.profile or ("update" if args.update_binaries else "worker-recovery")
    if args.profile in ('update', 'corrupt-recovery') and not args.update_binaries:
        parser.error("update/recovery profile requires --update-binaries")
    if args.profile in ('update', 'corrupt-recovery') and args.power_action != 'reboot':
        parser.error('--update-binaries requires --power-action reboot')
    if args.profile == 'time' and args.power_action != 'reboot':
        parser.error('time profile requires --power-action reboot')
    if args.inside:
        def deadline(*_):
            raise TimeoutError("appliance PPP communication deadline")
        def terminated(*_):
            raise KeyboardInterrupt()
        signal.signal(signal.SIGALRM, deadline)
        signal.signal(signal.SIGTERM, terminated)
        signal.alarm(1800 if args.update_binaries else 600 if args.profile == 'time' else 180)
        peer(args.inside, args.power_action, args.update_binaries, args.profile)
        return
    if args.dhcp6_server is None or not args.dhcp6_server.is_file():
        parser.error("--dhcp6-server must name an independent ISC dhcpd executable")
    root = [] if os.geteuid() == 0 else ["sudo", "-n"]
    def run(command):
        return subprocess.run(root + command, check=True, text=True, capture_output=True, timeout=10).stdout
    stage = run(["mktemp", "-d", "/tmp/routerd-pppoe-appliance.XXXXXX"]).strip()
    args.output.mkdir(parents=True, exist_ok=True)
    artifacts = ("control-records.json", "serial.log", "wan.pcap", "lan.pcap", "shutdown-wan.pcap", "shutdown-lan.pcap", "status.json", "ac.log", "dhcp6.log", "client-dhcp.log", "late-solicits.log", "time-source.log", "time-reboot-wan.pcap", "time-reboot-lan.pcap")
    for name in artifacts:
        (args.output / name).unlink(missing_ok=True)
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
        if args.profile == 'time':
            run(['install', '-m', '500', str((Path('dist') / 'chrony-engine.tar').resolve()), stage + '/chrony-engine.tar'])
        update_args = []
        if args.update_binaries:
            run(['mkdir', '-m', '700', stage + '/updates'])
            for name in ('base', 'next', 'damaged'):
                run(['mkdir', '-m', '700', stage + '/updates/' + name])
                for command in ('routerd', 'routerd-modules', 'routerd-logging'):
                    run(['install', '-m', '500', str((args.update_binaries / name / command).resolve()), stage + '/updates/' + name + '/' + command])
            for source, target in [(Path('dist/bin/routerd-bundle'), 'publisher'), *[(Path('dist') / name, name) for name in
                                  ('pppoe-engine.tar', 'pppoe-sources.tar', 'dnsmasq-engine.tar', 'dnsmasq-sources.tar')]]:
                run(['install', '-m', '500', str(source.resolve()), stage + '/' + target])
            update_args = ['--update-binaries', stage + '/updates']
        with (args.output / "peer.log").open("w") as output:
            result = subprocess.run(root + ["unshare", "--mount", "--net", "--propagation", "private", "python3", "-I", stage + "/peer.py", "--inside", stage, "--power-action", args.power_action] + ["--profile", args.profile] + update_args, stdout=output, stderr=subprocess.STDOUT, timeout=1860 if args.update_binaries else 660 if args.profile == 'time' else 240)
        for name in artifacts:
            artifact = subprocess.run(root + ["cat", stage + "/" + name], capture_output=True, timeout=10)
            if artifact.returncode == 0:
                (args.output / name).write_bytes(artifact.stdout)
        if result.returncode:
            raise RuntimeError("appliance PPP failed; see " + str(args.output / "peer.log"))
        if args.profile == 'time':
            print('PASS appliance learned NTP, clock correction, LAN time, persisted drift and reboot; ' + str(args.output / 'peer.log'))
        else:
            print(("PASS appliance signed update and fallback; " if args.profile == "update" else "PASS appliance PPP/PD/TCP/UDP/Renew/session recovery; ") + str(args.output / "peer.log"))
    finally:
        run(["python3", "-I", "-c", "import shutil,sys;shutil.rmtree(sys.argv[1])", stage])


if __name__ == "__main__":
    main()
