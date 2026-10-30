#!/usr/bin/env python3
"""Exercise the actual router's time client/server against independent chronyd.

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

import yaml


def run(args, timeout=10):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{args[0]}: {result.stderr[-2000:]}')
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
        if learned:
            run(['ip', '-n', 'ntp-source', '-6', 'address', 'add', '2001:db8:123::2/64', 'dev', 'eth0'])
        vendor = stage / 'vendor'
        vendor.mkdir()
        with tarfile.open(stage / 'chrony-engine.tar') as archive:
            archive.extractall(vendor, filter='data')
        loader = str(vendor / 'lib/ld-musl-x86_64.so.1')
        command = [loader, '--library-path', str(vendor / 'lib') + ':' + str(vendor / 'usr/lib'), str(vendor / 'usr/sbin/chronyd')]
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
        time_node = {'sources': [{'servers': [{'address': '192.0.2.2'}]}], 'listen': ['198.51.100.1'], 'allow': ['198.51.100.0/24']}
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
            boot['workers']['vrrp'] = str(modules)
            boot['workers']['dns'] = str(modules)
        (stage / 'boot.json').write_text(json.dumps(boot))
        capture = start('ntp-router', ['tcpdump', '-Z', 'root', '-n', '-i', 'any', '-U', '-w', str(stage / 'ntp.pcap'), 'udp port 123 or udp port 546 or udp port 547 or ip proto 112'], 'capture')
        router = start('ntp-router', [str(product), 'boot', '--file', str(stage / 'boot.json')], 'router')
        if learned:
            # The product brings up its WAN end before peer DAD can finish.
            wait(lambda: any(a.get('local') == '2001:db8:123::2' and not a.get('tentative') for link in json.loads(run(['ip', '-n', 'ntp-source', '-j', '-6', 'address', 'show', 'dev', 'eth0'])) for a in link['addr_info']))
            provider = start('ntp-source', provider_args, 'dhcp6')
            source_peer = start('ntp-source', command + ['-d', '-x', '-u', 'root', '-f', str(source_config)], 'source')
        def query(path, socket='control.sock'):
            return json.loads(run(inside('ntp-router', ['curl', '--fail-with-body', '--max-time', '3', '-sS', '--unix-socket', str(stage / 'run' / socket), 'http://routerd' + path])))
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
        def synchronized():
            nonlocal observed
            if not (stage / 'run/control.sock').exists():
                return False
            status = query('/v1/services')
            instance = next((s for s in status['instances'] if s['name'] == 'service.time'), None)
            snapshot = instance.get('snapshot', {}) if instance is not None else {}
            current = (snapshot.get('synchronized'), snapshot.get('source'), snapshot.get('stratum'), snapshot.get('last_error'))
            if current != observed:
                print('Observed product time state', current, flush=True)
                observed = current
            return snapshot.get('synchronized', False) and ipaddress.ip_address(snapshot.get('source', '::')) == ipaddress.ip_address(expected_source)
        wait(synchronized)
        snapshot = query('/v1/services/service.time')['snapshot']
        assert ipaddress.ip_address(snapshot['source']) == ipaddress.ip_address(expected_source) and not snapshot['adjusting_clock']
        assert snapshot['stratum'] == 9 and snapshot['expires_nanos'] > snapshot['observed_nanos']
        print('PASS actual supervisor/control/network/time worker: synchronized to independent stratum-8 chronyd, typed tracking and finite boot observation, host clock adjustment disabled', flush=True)
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
        wait(lambda: synchronized() and query('/v1/services/service.time')['snapshot']['observed_nanos'] > expired_observation)
        resumed_time = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')
        console_time()
        print('PASS original time observation expired while its worker and supervisor stopped; HTTP recovered with a fresh observation',
              json.dumps({'before_pid': stopped_time['pid'], 'after_pid': resumed_time['pid'],
                          'before_generation': stopped_time['generation'], 'after_generation': resumed_time['generation']}), flush=True)
        client_config = stage / 'client.conf'
        client_config.write_text('server 198.51.100.1 iburst\ncmdport 0\n')
        output = run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
        print('PASS independent chronyd -Q client received LAN NTP time without adjusting the host clock', flush=True)
        if learned:
            worker = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'service.time')
            def engine_pid():
                candidates = set()
                for task in Path(f'/proc/{worker["pid"]}/task').iterdir():
                    candidates.update((task / 'children').read_text().split())
                return next(int(pid) for pid in candidates if Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')[0] == b'chronyd')
            chronyd_pid = engine_pid()
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
            assert engine_pid() == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            print('PASS original DHCPv6 information/lease expiry selected the static fallback while the learned NTP endpoint remained reachable; same time worker and LAN response', flush=True)
            provider.send_signal(signal.SIGCONT)
            expected_source = '2001:db8:123::2'
            wait(synchronized)
            console_time()
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            assert engine_pid() == chronyd_pid
            print('PASS independent DHCPv6 reacquisition restored the learned source and LAN time response without restarting chronyd', flush=True)
            election_peer = start('ntp-client', [str(stage / 'keepalived'), '--dont-fork', '--dont-respawn', '--log-console', '--no-syslog', '--vrrp', '--use-file=' + str(peer_config), '--pid=' + str(stage / 'keepalived.pid'), '--vrrp_pid=' + str(stage / 'vrrp.pid')], 'vrrp')
            wait(lambda: query('/v1/services/service.gateway')['snapshot']['state'] == 'BACKUP')
            wait(lambda: all(module['name'] != 'service.time-info' for module in query('/v1/status', 'supervisor.sock')['modules']))
            expected_source = '192.0.2.2'
            wait(synchronized)
            console_time()
            assert engine_pid() == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            election_peer.terminate()
            election_peer.wait(timeout=5)
            expected_source = '2001:db8:123::2'
            wait(synchronized)
            console_time()
            assert engine_pid() == chronyd_pid
            print('PASS independent keepalived elected master: native VRRP backup stopped conditional DHCPv6, kept LAN time on fallback, then peer stop restored learning in the same chronyd', flush=True)
            sources = query('/v1/services/service.time/ntp-sources?generation=' + str(worker['generation']))['groups'][0]['servers']
            deadline = min(server['valid_until']['nanos'] for server in sources)
            assert deadline > time.clock_gettime_ns(time.CLOCK_BOOTTIME)
            control = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == 'control')
            # Freeze PID 1 too so its watchdog cannot replace the stopped
            # coordinator. The time worker and clock engine continue running.
            os.kill(router.pid, signal.SIGSTOP)
            frozen.append(router.pid)
            time.sleep(2)
            response = run(inside('ntp-router', ['curl', '--max-time', '3', '-sS', '--unix-socket', str(stage / 'run/control.sock'), '-o', '/dev/null', '-w', '%{http_code}', 'http://routerd/v1/services/service.time/ntp-sources?generation=' + str(worker['generation'])]))
            assert response == '503', 'unknown supervisor observation must not be reported as withdrawn sources'
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
            assert engine_pid() == chronyd_pid
            run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
            print('PASS stopped supervisor/control: original source deadline selected fallback in the same chronyd and independent LAN client still received time', flush=True)
            for pid in reversed(frozen):
                os.kill(pid, signal.SIGCONT)
            frozen.clear()

        if profile == 'import':
            # Apply the actual legacy client/server fragment structure through
            # the product importer and normal commit path, with public peers.
            for address in ('192.0.2.4/24', '2001:db8:123::3/64'):
                run(['ip', '-n', 'ntp-source', 'address', 'add', address, 'dev', 'eth0'])
            run(['ip', '-n', 'ntp-client', '-6', 'address', 'add', '2001:db8:456::2/64', 'dev', 'eth0'])
            for ns, network, gateway in [('ntp-source', '198.51.100.0/24', '192.0.2.1'), ('ntp-client', '192.0.2.0/24', '198.51.100.1'), ('ntp-source', '2001:db8:456::/64', '2001:db8:123::1'), ('ntp-client', '2001:db8:123::/64', '2001:db8:456::1')]:
                run(['ip', '-n', ns, 'route', 'replace', network, 'via', gateway])
            source_peer.terminate()
            source_peer.wait(timeout=5)
            source_config.write_text(source_config.read_text().replace('bindaddress 192.0.2.2', 'bindaddress 0.0.0.0').replace('bindaddress 2001:db8:123::2', 'bindaddress ::'))
            source_peer = start('ntp-source', command + ['-d', '-x', '-u', 'root', '-f', str(source_config)], 'source')
            phase_started = time.time()
            migrated = run([str(product), 'config', 'migrate', '--policy', str(stage / 'time-import-policy.yaml'), str(stage / 'time-import.yaml')])
            imported = yaml.safe_load(migrated)
            imported['resources'].append(console_resource)
            (stage / 'time-native.yaml').write_text(yaml.safe_dump(imported, sort_keys=False))
            revision = query('/v1/config')['revision']
            apply_log_position = len((stage / 'router.log').read_text())
            run(inside('ntp-router', [str(product), 'config', 'apply', '--file', str(stage / 'time-native.yaml'), '--runtime-dir', str(stage / 'run'), '--expected-revision', str(revision), '--request-id', 'import-time-fragments']), 30)
            configured = query('/v1/config')['config']['resources']
            clock = next(r['service'] for r in configured if r.get('service', {}).get('ntp'))
            gateway = next(r['service'] for r in configured if r.get('service', {}).get('vrrp'))
            time_name = 'service.' + clock['name']
            gateway_name = 'service.' + gateway['name']
            def imported_worker_ready():
                nonlocal worker
                worker = next(module for module in query('/v1/status', 'supervisor.sock')['modules'] if module['name'] == time_name)
                return worker['ready'] and worker.get('pid', 0) > 0
            wait(imported_worker_ready)
            chronyd_pid = engine_pid()
            def learned_present():
                path = Path(f'/proc/{chronyd_pid}/root/run/sources/learned.sources')
                return path.exists() and '2001:db8:123::2' in path.read_text()
            wait(learned_present)
            fixed = Path(f'/proc/{chronyd_pid}/root/chrony.conf').read_text()
            assert all('server ' + address + ' ' in fixed for address in ('192.0.2.2', '2001:db8:123::3', 'clock.ntp.test'))
            assert run(inside('ntp-client', ['dig', '@198.51.100.1', 'clock.ntp.test', 'A', '+short'])).strip() == '192.0.2.4'
            for address in ('192.0.2.2', '2001:db8:123::2'):
                start('ntp-source', ['python3', '-m', 'http.server', '18080', '--bind', address, '--directory', str(stage)], 'http')
                family = 'AF_INET6' if ':' in address else 'AF_INET'
                code = f'import socket\ns=socket.socket(socket.{family},socket.SOCK_DGRAM);s.bind(({address!r},18081))\nwhile True:\n b,a=s.recvfrom(100);s.sendto(b,a)'
                start('ntp-source', ['python3', '-u', '-c', code], 'udp')
            def imported_clients():
                run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
                for address in ('192.0.2.2', '2001:db8:123::2'):
                    url = '[' + address + ']' if ':' in address else address
                    run(inside('ntp-client', ['curl', '--noproxy', '*', '--fail', '--max-time', '3', '-sS', '-o', '/dev/null', 'http://' + url + ':18080/']))
                    family = 'AF_INET6' if ':' in address else 'AF_INET'
                    run(inside('ntp-client', ['python3', '-c', f'import socket;s=socket.socket(socket.{family},socket.SOCK_DGRAM);s.settimeout(3);s.connect(({address!r},18081));s.send(b"time-forwarding");assert s.recv(100)==b"time-forwarding"']))
                assert engine_pid() == chronyd_pid
            wait(lambda: query('/v1/services/' + time_name)['snapshot'].get('synchronized', False))
            imported_clients()
            console_time(time_name)
            pd_name = next(r['service']['name'] for r in configured if r.get('service', {}).get('dhcp6'))
            original_expiry = query('/v1/services/service.' + pd_name)['snapshot']['prefixes'][0]['valid_until']['nanos']
            provider.send_signal(signal.SIGSTOP)
            wait(lambda: not learned_present())
            assert time.clock_gettime_ns(time.CLOCK_BOOTTIME) >= original_expiry, 'conditional source disappeared before the observed DHCPv6 lease expired'

            imported_clients()
            console_time(time_name)
            provider.send_signal(signal.SIGCONT)
            wait(learned_present)
            imported_clients()
            console_time(time_name)
            peer_config.write_text(peer_config.read_text().replace('version 3', 'version 2'))
            election_peer = start('ntp-client', [str(stage / 'keepalived'), '--dont-fork', '--dont-respawn', '--log-console', '--no-syslog', '--vrrp', '--use-file=' + str(peer_config), '--pid=' + str(stage / 'keepalived.pid'), '--vrrp_pid=' + str(stage / 'vrrp.pid')], 'vrrp')
            wait(lambda: query('/v1/services/' + gateway_name)['snapshot']['state'] == 'BACKUP')
            wait(lambda: not learned_present())
            imported_clients()
            console_time(time_name)
            election_peer.terminate()
            election_peer.wait(timeout=5)
            wait(learned_present)
            imported_clients()
            console_time(time_name)
            starts = (stage / 'router.log').read_text()[apply_log_position:].count('chronyd version ')
            assert starts == 1, f'normal apply started chronyd {starts} times'
            print('Observed first imported time worker', {'generation': worker['generation'], 'pid': worker['pid'], 'chronyd_pid': chronyd_pid, 'engine_starts': starts}, flush=True)
            print('PASS product legacy import kept three always-static sources and two conditional fallbacks, learned source expiry/reacquisition/VRRP withdrawal/recovery, same chronyd and independent LAN NTP/IPv4/IPv6 TCP/UDP', flush=True)

        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=3)
        packets = run(['tcpdump', '-nn', '-r', str(stage / 'ntp.pcap')])
        assert '192.0.2.2.123' in packets and '198.51.100.1.123' in packets
        if profile == 'import':
            recent = run(['tcpdump', '-nn', '-tt', '-r', str(stage / 'ntp.pcap')])
            recent = '\n'.join(line for line in recent.splitlines() if line and line[0].isdigit() and float(line.split()[0]) >= phase_started)
            assert all(address + '.123' in recent for address in ('192.0.2.2', '192.0.2.4', '2001:db8:123::2', '2001:db8:123::3')), recent
        print('PASS packet capture contains both upstream synchronization and LAN NTP requests/replies', flush=True)
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
    parser.add_argument('--profile', choices=('static', 'learned', 'import'), default='static')
    parser.add_argument('--output', type=Path, default=Path('dist/ntp-peer'))
    args = parser.parse_args()
    if args.profile != 'static' and args.dhcp6_server is None:
        parser.error('learned/import profiles require --dhcp6-server')
    if args.inside:
        peer(args.inside, args.profile)
        return
    project = Path(__file__).resolve().parents[1]
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    stage = Path(run(root + ['mktemp', '-d', '/tmp/routerd-ntp.XXXXXX']).strip())
    try:
        for name, source in [('peer.py', Path(__file__)), ('chrony-engine.tar', project / 'dist/chrony-engine.tar'), ('time-import.yaml', project / 'internal/integration/testdata/ntp-import.yaml'), ('time-import-policy.yaml', project / 'internal/integration/testdata/ntp-import-policy.yaml')] + [(name, project / 'dist/bin' / name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
            run(root + ['install', '-m', '755' if name.startswith('routerd') else '644', str(source), str(stage / name)])
        extra = []
        if args.dhcp6_server:
            run(root + ['install', '-m', '500', str(args.dhcp6_server), str(stage / 'dhcpd')])
            run(root + ['install', '-m', '500', shutil.which('keepalived'), str(stage / 'keepalived')])
            extra = ['--dhcp6-server', str(stage / 'dhcpd')]
        result = subprocess.run(root + ['unshare', '--mount', '--net', '--fork', 'python3', str(stage / 'peer.py'), '--inside', str(stage), '--profile', args.profile] + extra, capture_output=True, text=True, timeout=210 if extra else 90)
        args.output.mkdir(parents=True, exist_ok=True)
        (args.output / 'peer.log').write_text(result.stdout + result.stderr)
        for name in ('router.log', 'source.log', 'dhcp6.log', 'vrrp.log', 'capture.log', 'ntp.pcap'):
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
