#!/usr/bin/env python3
"""Exercise the actual router's time client/server against independent DNS/NTP peers.

Private network/mount namespaces and -x/-Q keep the host clock unchanged.
An independent LAN HTTP client also compares the time console with the actual
service observation, including its original expiry and fresh acquisition after recovery.
This host-mode check does not qualify appliance clock adjustment.
"""
import argparse
import ipaddress
from html.parser import HTMLParser
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tarfile
import time
from urllib.parse import quote



def run(args, timeout=10):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        print(result.stdout + result.stderr, end="", flush=True)
        raise subprocess.CalledProcessError(result.returncode, args, result.stdout, result.stderr)
    return result.stdout


def peer(stage, profile):
    learned = profile != "static"
    children = []
    frozen = []
    namespaces = ['ntp-router', 'ntp-source', 'ntp-client']
    run(['mount', '--make-rprivate', '/'])
    Path('/run/netns').mkdir(exist_ok=True)
    run(['mount', '-t', 'tmpfs', '-o', 'mode=700', 'tmpfs', '/run/netns'])
    def inside(namespace, args):
        return ['ip', 'netns', 'exec', namespace] + args
    def start(namespace, args, name):
        with (stage / (name + '.log')).open('a') as log:
            p = subprocess.Popen(inside(namespace, args), stdout=log, stderr=log)
        children.append(p)
        return p
    def wait(condition, seconds=30):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            if condition():
                return
            time.sleep(0.2)
        raise RuntimeError('NTP communication condition timed out')
    try:
        for name in namespaces:
            run(['ip', 'netns', 'add', name])
            run(['ip', '-n', name, 'link', 'set', 'lo', 'up'])
        for device, ns, address in [('wan0', 'ntp-source', '192.0.2.2/24'), ('lan0', 'ntp-client', '198.51.100.2/24')]:
            run(['ip', 'link', 'add', device, 'netns', 'ntp-router', 'type', 'veth', 'peer', 'name', 'eth0', 'netns', ns])
            run(['ip', '-n', ns, 'link', 'set', 'eth0', 'up'])
            run(['ip', '-n', ns, 'address', 'add', address, 'dev', 'eth0'])
        run(['ip', '-n', 'ntp-source', 'address', 'add', '192.0.2.53/24', 'dev', 'eth0'])
        if learned:
            run(['ip', '-n', 'ntp-source', '-6', 'address', 'add', '2001:db8:123::2/64', 'dev', 'eth0'])
        vendor = stage / 'vendor'
        vendor.mkdir()
        with tarfile.open(stage / 'chrony-engine.tar') as archive:
            archive.extractall(vendor, filter='data')
        loader = str(vendor / 'lib/ld-musl-x86_64.so.1')
        command = [loader, '--library-path', str(vendor / 'lib') + ':' + str(vendor / 'usr/lib'), str(vendor / 'usr/sbin/chronyd')]
        dns_vendor = stage / 'dnsmasq-vendor'
        dns_vendor.mkdir()
        with tarfile.open(stage / 'dnsmasq-engine.tar') as archive:
            archive.extractall(dns_vendor, filter='data')
        dns_loader = str(dns_vendor / 'lib/ld-musl-x86_64.so.1')
        dnsmasq = str(dns_vendor / 'usr/sbin/dnsmasq')
        dns_args = [dns_loader, '--library-path', str(dns_vendor / 'lib') + ':' + str(dns_vendor / 'usr/lib'),
                    str(dnsmasq), '--no-daemon', '--conf-file=/dev/null', '--no-resolv', '--no-hosts',
                    '--bind-interfaces', '--listen-address=192.0.2.53', '--port=53',
                    '--address=/ntp.jst.mfeed.ad.jp/192.0.2.2', '--local=/ntp.jst.mfeed.ad.jp/',
                    '--address=/ntp.nict.jp/192.0.2.2', '--local=/ntp.nict.jp/']
        start('ntp-source', dns_args, 'dns')
        def dns_ready():
            result = subprocess.run(inside('ntp-source', ['dig', '@192.0.2.53', 'ntp.jst.mfeed.ad.jp', 'A', '+time=1', '+tries=1', '+short']),
                                    capture_output=True, text=True)
            return result.returncode == 0 and result.stdout.strip() == '192.0.2.2'
        try:
            wait(dns_ready)
        except RuntimeError:
            result = subprocess.run(inside('ntp-source', ['dig', '@192.0.2.53', 'ntp.jst.mfeed.ad.jp', 'A', '+time=1', '+tries=1']),
                                    capture_output=True, text=True)
            print('Independent DNS peer query failed:', result.stdout, result.stderr, flush=True)
            for namespace in ('ntp-source', 'ntp-router'):
                state = subprocess.run(inside(namespace, ['ip', '-brief', 'address']), capture_output=True, text=True)
                print(namespace, state.stdout, state.stderr, flush=True)
            sockets = subprocess.run(inside('ntp-source', ['ss', '-lun']), capture_output=True, text=True)
            print('Independent DNS peer sockets:', sockets.stdout, sockets.stderr, flush=True)
            print('Independent DNS peer log:', (stage / 'dns.log').read_text(), flush=True)
            raise
        source_state = stage / 'source'
        source_state.mkdir(mode=0o700)
        source_config = stage / 'source.conf'
        source_config.write_text(f'local stratum 8\nbindaddress 192.0.2.2\nallow 192.0.2.0/24\ncmdport 0\nbindcmdaddress {source_state}/command.sock\npidfile {source_state}/pid\ndriftfile {source_state}/drift\ndumpdir {source_state}\nlogdir {source_state}\n')
        if learned:
            with source_config.open('a') as out:
                out.write('bindaddress 2001:db8:123::2\nallow 2001:db8:123::/64\n')
            (stage / 'dhcp6.leases').touch()
            (stage / 'dhcp6.conf').write_text('default-lease-time 18; preferred-lifetime 12; option dhcp-renewal-time 5; option dhcp-rebinding-time 9; option dhcp6.sntp-servers 2001:db8:123::2; subnet6 2001:db8:123::/64 { prefix6 2001:db8:400:: 2001:db8:400:f0:: /60; }\n')
            provider_args = [str(stage / 'dhcpd'), '-6', '-f', '-d', '-cf', str(stage / 'dhcp6.conf'), '-lf', str(stage / 'dhcp6.leases'), '-pf', str(stage / 'dhcp6.pid'), 'eth0']
        if not learned:
            source_peer = start('ntp-source', command + ['-d', '-x', '-u', 'root', '-f', str(source_config)], 'source')
        product = stage / 'routerd'
        modules = stage / 'routerd-modules'
        resources = []
        for device, address in [('wan0', '192.0.2.1/24'), ('lan0', '198.51.100.1/24')]:
            resources += [{'id': device, 'interface': {'name': device}}, {'id': device + '-address', 'depends_on': [device], 'address': {'interface': device, 'cidr': address}}]
        console_resource = {'id': 'console', 'service': {'name': 'console', 'web_console': {
            'listen': '198.51.100.1:8088', 'base_path': '/ops/', 'title': 'routerd2'}}}
        resources.append(console_resource)
        time_node = {'sources': [{'servers': [{'address': 'ntp.jst.mfeed.ad.jp'}, {'address': 'ntp.nict.jp'}]}],
                     'dns': ['192.0.2.53'], 'listen': ['198.51.100.1'], 'allow': ['198.51.100.0/24']}
        resources.append({'id': 'time', 'service': {'name': 'time', 'ntp': time_node}})
        if learned:
            resources += [
                {'id': 'wan6-address', 'address': {'interface': 'wan0', 'cidr': '2001:db8:123::1/64'}},
                {'id': 'gateway', 'service': {'name': 'gateway', 'vrrp': {'interface': 'lan0', 'vip_interface': 'time-vip', 'family': 'ipv4', 'source_address': '198.51.100.1', 'unicast_peers': ['198.51.100.2'], 'virtual_router_id': 42, 'priority': 100, 'advert_interval_centiseconds': 100, 'ipv4_checksum': 'rfc5798', 'addresses': ['198.51.100.100/24'], 'accept_mode': True}}},
                {'id': 'time-info', 'when': {'fact': 'service.gateway.active'}, 'depends_on': ['wan6-address'], 'service': {'name': 'time-info', 'dhcp6': {'interface': 'wan0', 'iaid': 1, 'request_prefix': True, 'prefix_length': 60}}},
            ]
            time_node['sources'][0]['sources_from'] = ['time-info']
        configuration = stage / 'config.json'
        configuration.write_text(json.dumps({'schema_version': 2, 'resources': resources}))
        boot = {'platform': 'host', 'config': str(configuration), 'runtime_dir': str(stage / 'run'), 'state_dir': str(stage / 'state'),
                'workers': {role: str(modules) for role in ('network', 'control', 'management', 'ntp', 'web_console')}, 'engines': {'ntp': str(stage / 'chrony-engine.tar')}}
        boot['workers']['logging'] = str(stage / 'routerd-logging')
        if learned:
            boot['workers']['dhcp6'] = str(modules)
            boot['workers']['ra_client'] = str(modules)
            boot['workers']['vrrp'] = str(modules)
            boot['workers']['dns'] = str(modules)
        (stage / 'boot.json').write_text(json.dumps(boot))
        outage_rules = stage / 'outage.nft'
        outage_rules.write_text('table inet ntp_outage {\n chain input { type filter hook input priority 0; policy accept; iifname "eth0" udp dport 123 counter drop; }\n chain output { type filter hook output priority 0; policy accept; oifname "eth0" udp sport 123 counter drop; }\n}\n')
        run(inside('ntp-source', ['nft', '-f', str(outage_rules)]))
        capture = start('ntp-router', ['tcpdump', '-Z', 'root', '-n', '-i', 'any', '-U', '-w', str(stage / 'ntp.pcap'), 'udp port 53 or udp port 123 or udp port 546 or udp port 547 or ip proto 112'], 'capture')
        router = start('ntp-router', [str(product), 'boot', '--file', str(stage / 'boot.json')], 'router')
        if learned:
            # The product brings up its WAN end before peer DAD can finish.
            wait(lambda: any(a.get('local') == '2001:db8:123::2' and not a.get('tentative') for link in json.loads(run(['ip', '-n', 'ntp-source', '-j', '-6', 'address', 'show', 'dev', 'eth0'])) for a in link['addr_info']))
            provider = start('ntp-source', provider_args, 'dhcp6')
            source_peer = start('ntp-source', command + ['-d', '-x', '-u', 'root', '-f', str(source_config)], 'source')
        def query(path, socket='control.sock'):
            return json.loads(run(inside('ntp-router', ['curl', '--fail-with-body', '--max-time', '3', '-sS', '--unix-socket', str(stage / 'run' / socket), 'http://routerd' + path])))
        def time_worker():
            return next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')
        def engine_pid(worker):
            candidates = set()
            for task in Path(f'/proc/{worker["pid"]}/task').iterdir():
                candidates.update((task / 'children').read_text().split())
            return next(int(pid) for pid in candidates if Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')[0] == b'chronyd')
        def console_time(service='service.time', current=True):
            class TimePage(HTMLParser):
                def __init__(self):
                    super().__init__()
                    self.observation = {}
                    self.values = {}
                    self.field = None
                    self.text = []
                def handle_starttag(self, tag, attributes):
                    attributes = dict(attributes)
                    if tag == 'section' and 'data-time-observed' in attributes:
                        self.observation = attributes
                    if tag == 'dd':
                        self.field = attributes.get('data-time-field')
                        if self.field:
                            self.values[self.field] = ''
                def handle_endtag(self, tag):
                    if tag == 'dd':
                        self.field = None
                def handle_data(self, data):
                    self.text.append(data)
                    if self.field:
                        self.values[self.field] += data
            before = None
            body = ''
            page = None
            def same_observation():
                nonlocal before, body, page
                before = query('/v1/services/' + service)['snapshot']
                body = run(inside('ntp-client', ['curl', '--noproxy', '*', '--fail-with-body', '--max-time', '3', '-sS',
                                                 'http://198.51.100.1:8088/ops/time?service=' + quote(service)]))
                page = TimePage()
                page.feed(body)
                after = query('/v1/services/' + service)['snapshot']
                return before['observed_nanos'] == after['observed_nanos']
            # The producer samples every two seconds. Compare one unchanged
            # observation across the private API and the independent HTTP read.
            wait(same_observation, 5)
            assert int(page.observation['data-time-observed']) == before['observed_nanos'], page.observation
            assert int(page.observation['data-time-expires']) == before['expires_nanos'], page.observation
            assert page.observation['data-time-current'] == str(current).lower(), page.observation
            live = before['observed_nanos'] <= time.clock_gettime_ns(time.CLOCK_BOOTTIME) < before['expires_nanos']
            assert live == current, before
            text = ''.join(page.text)
            state = '観測期限切れ' if not current else '同期済み' if before['synchronized'] else '未同期'
            assert page.values['state'].strip() == state, page.values
            if not current:
                assert '現在の同期状態は未確認です。' in text, text
            assert ipaddress.ip_address(page.values['source'].strip()) == ipaddress.ip_address(before['source']), page.values
            for field in ('stratum', 'offset_seconds', 'rms_offset_seconds', 'frequency_ppm', 'skew_ppm',
                          'root_delay_seconds', 'root_dispersion_seconds', 'leap_status'):
                assert float(page.values[field].strip()) == float(before[field]), (field, page.values, before)
            print('PASS independent LAN HTTP time observation', json.dumps({'service': service, 'source': before['source'],
                  'state': state, 'observed_nanos': before['observed_nanos'], 'expires_nanos': before['expires_nanos'],
                  'offset_seconds': before['offset_seconds']}), flush=True)

        observed = None
        expected_source = '2001:db8:123::2' if learned else '192.0.2.2'
        def synchronized(after=0):
            nonlocal observed
            if not (stage / 'run/control.sock').exists():
                return False
            try:
                status = query('/v1/services')
            except subprocess.CalledProcessError as error:
                # The supervisor can replace control after the deliberate pause.
                # Only curl's observed connection failure returns to the same wait.
                if error.returncode == 7:
                    return False
                raise
            instance = next((s for s in status['instances'] if s['name'] == 'service.time'), None)
            snapshot = instance.get('snapshot', {}) if instance is not None else {}
            current = (snapshot.get('synchronized'), snapshot.get('source'), snapshot.get('stratum'), snapshot.get('last_error'))
            if current != observed:
                print('Observed product time state', current, flush=True)
                observed = current
            return (snapshot.get('synchronized', False)
                    and ipaddress.ip_address(snapshot.get('source', '::')) == ipaddress.ip_address(expected_source)
                    and snapshot.get('observed_nanos', 0) > after)
        def upstream_drops():
            counters = json.loads(run(inside('ntp-source', ['nft', '-j', 'list', 'table', 'inet', 'ntp_outage'])))
            return sum(expr['counter']['packets'] for item in counters['nftables'] for expr in item.get('rule', {}).get('expr', []) if 'counter' in expr)
        def blocked_upstream():
            if not (stage / 'run/control.sock').exists():
                return False
            try:
                status = query('/v1/services')
            except subprocess.CalledProcessError as error:
                if error.returncode == 7:
                    return False
                raise
            instance = next((item for item in status['instances'] if item['name'] == 'service.time'), None)
            snapshot = instance.get('snapshot', {}) if instance is not None else {}
            return instance is not None and not snapshot.get('synchronized', False) and upstream_drops() > 0
        wait(blocked_upstream)
        dropped = upstream_drops()
        startup_worker = time_worker()
        startup_chronyd = engine_pid(startup_worker)
        restored_started = time.monotonic()
        run(inside('ntp-source', ['nft', 'delete', 'table', 'inet', 'ntp_outage']))
        wait(synchronized, 150)
        assert engine_pid(time_worker()) == startup_chronyd, 'chronyd restarted instead of recovering from the restored upstream'
        snapshot = query('/v1/services/service.time')['snapshot']
        assert ipaddress.ip_address(snapshot['source']) == ipaddress.ip_address(expected_source) and not snapshot['adjusting_clock']
        assert snapshot['stratum'] == 9 and snapshot['expires_nanos'] > snapshot['observed_nanos']
        print('PASS actual supervisor/control/network/time worker: synchronized to independent stratum-8 chronyd, typed tracking and finite boot observation, host clock adjustment disabled', flush=True)
        print('PASS upstream NTP UDP loss and recovery: observed', dropped, 'dropped packets; fresh sample arrived in', round(time.monotonic() - restored_started, 3), 'seconds on the same chronyd', flush=True)
        console_time()
        stopped_time = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')
        # Freeze the supervisor too: its original five-second heartbeat budget
        # must not race the unchanged five-second observation expiry. Control,
        # the HTTP console and chronyd remain available.
        os.kill(router.pid, signal.SIGSTOP)
        frozen.append(router.pid)
        os.kill(stopped_time['pid'], signal.SIGSTOP)
        frozen.append(stopped_time['pid'])
        wait(lambda: time.clock_gettime_ns(time.CLOCK_BOOTTIME) >= query('/v1/services/service.time')['snapshot']['expires_nanos'])
        expired_observation = query('/v1/services/service.time')['snapshot']['observed_nanos']
        console_time(current=False)
        for pid in reversed(frozen):
            os.kill(pid, signal.SIGCONT)
        frozen.clear()
        wait(lambda: synchronized(expired_observation))
        resumed_time = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')
        console_time()
        print('PASS original time observation expired while its worker and supervisor stopped; HTTP recovered with a fresh observation',
              json.dumps({'before_pid': stopped_time['pid'], 'after_pid': resumed_time['pid'],
                          'before_generation': stopped_time['generation'], 'after_generation': resumed_time['generation']}), flush=True)
        client_config = stage / 'client.conf'
        client_config.write_text('server 198.51.100.1 iburst\ncmdport 0\n')
        output = run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
        print('PASS independent chronyd -Q client received LAN NTP time without adjusting the host clock', flush=True)
        worker = time_worker()
        chronyd_pid = engine_pid(worker)
        if learned:
            peer_config = stage / 'keepalived.conf'
            peer_config.write_text('global_defs {\n router_id TIME_PEER\n}\nvrrp_instance TIME {\n state BACKUP\n interface eth0\n virtual_router_id 42\n priority 200\n advert_int 1\n version 3\n accept\n unicast_src_ip 198.51.100.2\n unicast_peer {\n 198.51.100.1\n }\n virtual_ipaddress {\n 198.51.100.100/24 dev eth0\n }\n}\n')
        if profile == 'learned':
            info = query('/v1/services/service.time-info')['snapshot']
            print('Observed learned-source original deadline', info['prefixes'][0]['valid_until'], flush=True)
            provider.send_signal(signal.SIGSTOP)
            expected_source = '192.0.2.2'
            wait(synchronized)
            console_time()
            assert next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')['pid'] == worker['pid']
            assert engine_pid(worker) == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            print('PASS original DHCPv6 information/lease expiry selected the static fallback while the learned NTP endpoint remained reachable; same time worker and LAN response', flush=True)
            provider.send_signal(signal.SIGCONT)
            expected_source = '2001:db8:123::2'
            wait(synchronized)
            console_time()
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            assert engine_pid(worker) == chronyd_pid
            print('PASS independent DHCPv6 reacquisition restored the learned source and LAN time response without restarting chronyd', flush=True)
            election_peer = start('ntp-client', [str(stage / 'keepalived'), '--dont-fork', '--dont-respawn', '--log-console', '--no-syslog', '--vrrp', '--use-file=' + str(peer_config), '--pid=' + str(stage / 'keepalived.pid'), '--vrrp_pid=' + str(stage / 'vrrp.pid')], 'vrrp')
            wait(lambda: query('/v1/services/service.gateway')['snapshot']['state'] == 'BACKUP')
            wait(lambda: all(module['name'] != 'service.time-info' for module in query('/v1/status', 'supervisor.sock')['modules']))
            expected_source = '192.0.2.2'
            wait(synchronized)
            console_time()
            assert engine_pid(worker) == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            election_peer.terminate()
            election_peer.wait(timeout=5)
            expected_source = '2001:db8:123::2'
            wait(synchronized)
            console_time()
            assert engine_pid(worker) == chronyd_pid
            print('PASS independent keepalived elected master: native VRRP backup stopped conditional DHCPv6, kept LAN time on fallback, then peer stop restored learning in the same chronyd', flush=True)
            sources = query('/v1/services/service.time/ntp-sources?generation=' + str(worker['generation']))['groups'][0]['servers']
            deadline = min(server['valid_until']['nanos'] for server in sources)
            assert deadline > time.clock_gettime_ns(time.CLOCK_BOOTTIME)
            control = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'control')
            # Freeze PID 1 too so its watchdog cannot replace the stopped
            # coordinator. The time worker and clock engine continue running.
            os.kill(router.pid, signal.SIGSTOP)
            frozen.append(router.pid)
            response = None
            def supervisor_observation_expired():
                nonlocal response
                response = run(inside('ntp-router', ['curl', '--max-time', '3', '-sS', '--unix-socket', str(stage / 'run/control.sock'), '-o', '/dev/null', '-w', '%{http_code}', 'http://routerd/v1/services/service.time/ntp-sources?generation=' + str(worker['generation'])])).strip()
                return response == '503'
            remaining = (deadline - time.clock_gettime_ns(time.CLOCK_BOOTTIME)) / 1_000_000_000
            if remaining <= 0:
                raise RuntimeError('learned NTP source expired before the supervisor observation check')
            wait(supervisor_observation_expired, remaining)
            assert time.clock_gettime_ns(time.CLOCK_BOOTTIME) < deadline
            assert '2001:db8:123::2' in Path(f'/proc/{chronyd_pid}/root/run/sources/learned.sources').read_text()
            print('PASS stale supervisor observation returned unavailable while the original learned source remained live', flush=True)
            os.kill(control['pid'], signal.SIGSTOP)
            frozen.append(control['pid'])
            log_position = len((stage / 'router.log').read_text())
            def engine_fallback():
                return 'Selected source 192.0.2.2' in (stage / 'router.log').read_text()[log_position:]
            wait(engine_fallback)
            assert time.clock_gettime_ns(time.CLOCK_BOOTTIME) >= deadline
            assert engine_pid(worker) == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            print('PASS stopped supervisor/control: original source deadline selected fallback in the same chronyd and independent LAN client still received time', flush=True)
            resumed_at = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
            for pid in reversed(frozen):
                os.kill(pid, signal.SIGCONT)
            frozen.clear()
            wait(lambda: synchronized(resumed_at))
            worker = time_worker()
            chronyd_pid = engine_pid(worker)
            console_time()
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            print('PASS supervisor/control recovery: fresh DHCPv6 NTP sample and independent LAN client response', flush=True)

        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=3)
        packets = run(['tcpdump', '-nn', '-r', str(stage / 'ntp.pcap')])
        assert '192.0.2.2.123' in packets and '198.51.100.1.123' in packets
        assert '192.0.2.53.53' in packets and 'ntp.jst.mfeed.ad.jp' in packets
        print('PASS packet capture contains homert hostname DNS lookup, upstream NTP and LAN NTP requests/replies', flush=True)
    finally:
        for pid in reversed(frozen):
            os.kill(pid, signal.SIGCONT)
        if learned and 'provider' in locals() and provider.poll() is None:
            provider.send_signal(signal.SIGCONT)
        for child in reversed(children):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
        for name in reversed(namespaces):
            subprocess.run(['ip', 'netns', 'delete', name], capture_output=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--inside', type=Path)
    parser.add_argument('--dhcp6-server', type=Path)
    parser.add_argument('--profile', choices=('static', 'learned'), default='static')
    parser.add_argument('--output', type=Path, default=Path('dist/ntp-peer'))
    args = parser.parse_args()
    if args.profile != 'static' and args.dhcp6_server is None:
        parser.error('learned profile requires --dhcp6-server')
    if args.inside:
        peer(args.inside, args.profile)
        return
    project = Path(__file__).resolve().parents[1]
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    stage = Path(run(root + ['mktemp', '-d', '/tmp/routerd-ntp.XXXXXX']).strip())
    try:
        for name, source in [('peer.py', Path(__file__)), ('chrony-engine.tar', project / 'dist/chrony-engine.tar'),
                             ('dnsmasq-engine.tar', project / 'dist/dnsmasq-engine.tar')] + [(name, project / 'dist/bin' / name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
            run(root + ['install', '-m', '755' if name.startswith('routerd') else '644', str(source), str(stage / name)])
        extra = []
        if args.dhcp6_server:
            run(root + ['install', '-m', '500', str(args.dhcp6_server), str(stage / 'dhcpd')])
            run(root + ['install', '-m', '500', shutil.which('keepalived'), str(stage / 'keepalived')])
            extra = ['--dhcp6-server', str(stage / 'dhcpd')]
        result = subprocess.run(root + ['unshare', '--mount', '--net', '--fork', 'python3', str(stage / 'peer.py'), '--inside', str(stage), '--profile', args.profile] + extra, capture_output=True, text=True, timeout=300)
        args.output.mkdir(parents=True, exist_ok=True)
        (args.output / 'peer.log').write_text(result.stdout + result.stderr)
        for name in ('router.log', 'source.log', 'dhcp6.log', 'vrrp.log', 'dns.log', 'capture.log', 'ntp.pcap'):
            artifact = subprocess.run(root + ['cat', str(stage / name)], capture_output=True)
            if artifact.returncode == 0:
                (args.output / name).write_bytes(artifact.stdout)
        print(result.stdout, end='')
        if result.returncode:
            raise RuntimeError('NTP communication failed; see ' + str(args.output))
    finally:
        run(root + ['python3', '-I', '-c', 'import shutil,sys;shutil.rmtree(sys.argv[1])', str(stage)])


if __name__ == '__main__':
    main()
