#!/usr/bin/env python3
"""Exercise the actual router's time client/server against independent chronyd.

Private network/mount namespaces and -x/-Q keep the host clock unchanged.
This host-mode check does not qualify appliance clock adjustment.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tarfile
import time


def run(args, timeout=10):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{args[0]}: {result.stderr[-2000:]}')
    return result.stdout


def peer(stage):
    children = []
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
        start('ntp-source', command + ['-d', '-x', '-u', 'root', '-f', str(source_config)], 'source')
        product = stage / 'routerd'
        modules = stage / 'routerd-modules'
        resources = []
        for device, address in [('wan0', '192.0.2.1/24'), ('lan0', '198.51.100.1/24')]:
            resources += [{'id': device, 'interface': {'name': device}}, {'id': device + '-address', 'depends_on': [device], 'address': {'interface': device, 'cidr': address}}]
        resources.append({'id': 'time', 'service': {'name': 'time', 'ntp': {'servers': [{'address': '192.0.2.2'}], 'listen': ['198.51.100.1'], 'allow': ['198.51.100.0/24']}}})
        configuration = stage / 'config.json'
        configuration.write_text(json.dumps({'schema_version': 2, 'resources': resources}))
        boot = {'platform': 'host', 'config': str(configuration), 'runtime_dir': str(stage / 'run'), 'state_dir': str(stage / 'state'),
                'workers': {role: str(modules) for role in ('network', 'control', 'management', 'ntp')}, 'engines': {'ntp': str(stage / 'chrony-engine.tar')}}
        boot['workers']['logging'] = str(stage / 'routerd-logging')
        (stage / 'boot.json').write_text(json.dumps(boot))
        capture = start('ntp-router', ['tcpdump', '-n', '-i', 'any', '-U', '-w', str(stage / 'ntp.pcap'), 'udp port 123'], 'capture')
        start('ntp-router', [str(product), 'boot', '--file', str(stage / 'boot.json')], 'router')
        def query(path):
            return json.loads(run(inside('ntp-router', ['curl', '--fail-with-body', '--max-time', '3', '-sS', '--unix-socket', str(stage / 'run/control.sock'), 'http://routerd' + path])))
        observed = None
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
            return snapshot.get('synchronized', False)
        wait(synchronized)
        snapshot = query('/v1/services/service.time')['snapshot']
        assert snapshot['source'] == '192.0.2.2' and not snapshot['adjusting_clock']
        assert snapshot['stratum'] == 9 and snapshot['expires_nanos'] > snapshot['observed_nanos']
        print('PASS actual supervisor/control/network/time worker: synchronized to independent stratum-8 chronyd, typed tracking and finite boot observation, host clock adjustment disabled', flush=True)
        client_config = stage / 'client.conf'
        client_config.write_text('server 198.51.100.1 iburst\ncmdport 0\n')
        output = run(inside('ntp-client', command + ['-Q', '-t', '8', '-u', 'root', '-f', str(client_config)]), 12)
        print('PASS independent chronyd -Q client received LAN NTP time without adjusting the host clock', flush=True)
        capture.send_signal(signal.SIGINT)
        capture.wait(timeout=3)
        packets = run(['tcpdump', '-nn', '-r', str(stage / 'ntp.pcap')])
        assert '192.0.2.2.123' in packets and '198.51.100.1.123' in packets
        print('PASS packet capture contains both upstream synchronization and LAN NTP requests/replies', flush=True)
    finally:
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
    parser.add_argument('--output', type=Path, default=Path('dist/ntp-peer'))
    args = parser.parse_args()
    if args.inside:
        peer(args.inside)
        return
    project = Path(__file__).resolve().parents[1]
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    stage = Path(run(root + ['mktemp', '-d', '/tmp/routerd-ntp.XXXXXX']).strip())
    try:
        for name, source in [('peer.py', Path(__file__)), ('chrony-engine.tar', project / 'dist/chrony-engine.tar')] + [(name, project / 'dist/bin' / name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
            run(root + ['install', '-m', '755' if name.startswith('routerd') else '644', str(source), str(stage / name)])
        result = subprocess.run(root + ['unshare', '--mount', '--net', '--fork', 'python3', str(stage / 'peer.py'), '--inside', str(stage)], capture_output=True, text=True, timeout=90)
        args.output.mkdir(parents=True, exist_ok=True)
        (args.output / 'peer.log').write_text(result.stdout + result.stderr)
        for name in ('router.log', 'source.log', 'capture.log', 'ntp.pcap'):
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
