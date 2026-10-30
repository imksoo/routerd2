#!/usr/bin/env python3
"""Receive actual product syslog over UDP in disposable Linux namespaces.

Worker lifecycle records prove the product-to-receiver path. One explicitly
labelled synthetic DEBUG input checks only severity filtering, not log origin.
The independent client exercises IPv4 UDP forwarding, not physical hardware,
TCP continuity, automatic address acquisition, or reliable UDP log delivery.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import time


def run(command, timeout=10, input=None):
    result = subprocess.run(command, input=input, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{command[0]}: {result.stdout[-2000:]}{result.stderr[-2000:]}')
    return result.stdout


def endpoint(mode, output, port):
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        if mode == 'client':
            sock.settimeout(1)
            sock.connect(('192.0.2.2', 18081))
            with output.open('a', buffering=1) as log:
                sequence = 0
                while True:
                    sequence += 1
                    message = f'syslog-forwarding-{sequence}'.encode()
                    start = time.monotonic()
                    try:
                        sock.send(message)
                        ok = sock.recv(1024) == message
                    except OSError:
                        ok = False
                    log.write(json.dumps({'sequence': sequence, 'ok': ok, 'seconds': time.monotonic()-start})+'\n')
                    time.sleep(.1)
        else:
            sock.bind(('192.0.2.2', port))
            with output.open('a', buffering=1) as log:
                log.write(json.dumps({'ready': True})+'\n')
                while True:
                    message, peer = sock.recvfrom(65535)
                    if mode == 'echo':
                        sock.sendto(message, peer)
                    else:
                        log.write(json.dumps({'time': time.time(), 'peer': peer, 'message': message.decode('utf-8')})+'\n')


def peer(stage):
    children = []
    namespaces = ('syslog-router', 'syslog-peer', 'syslog-client')
    assert os.readlink('/proc/self/ns/net') != os.environ['ROUTERD_SYSLOG_PARENT_NETNS'], 'requires private network namespace'
    run(['mount', '--make-rprivate', '/'])
    Path('/run/netns').mkdir(exist_ok=True)
    run(['mount', '-t', 'tmpfs', '-o', 'mode=700', 'tmpfs', '/run/netns'])

    def inside(namespace, command):
        return ['ip', 'netns', 'exec', namespace]+command

    def start(namespace, command, name):
        with (stage/(name+'.log')).open('a') as log:
            process = subprocess.Popen(inside(namespace, command), stdout=log, stderr=subprocess.STDOUT)
        children.append(process)
        return process

    def wait(condition, description, seconds=30):
        deadline = time.monotonic()+seconds
        while time.monotonic() < deadline:
            if condition():
                return
            time.sleep(.1)
        raise RuntimeError('timed out: '+description)

    def rows(name):
        path = stage/name
        if not path.exists():
            return []
        # A running receiver may be appending its last line.
        return [json.loads(line) for line in path.read_text().splitlines(keepends=True) if line.endswith('\n')]

    def api(socket_name, path, body=None):
        command = ['curl', '--fail-with-body', '--max-time', '5', '-sS', '--unix-socket', str(stage/'run'/socket_name), 'http://routerd'+path]
        if body is not None:
            command += ['-H', 'Content-Type: application/json', '--data-binary', json.dumps(body)]
        return json.loads(run(inside('syslog-router', command), 8))

    def modules():
        return {item['name']: item for item in api('supervisor.sock', '/v1/status')['modules']}

    def local_logs():
        return api('logging.sock', '/v1/logs?module=supervisor&limit=200')['records']

    def restart_management():
        previous = modules()['management']['generation']
        api('supervisor.sock', '/v1/modules/management/restart', {})
        wait(lambda: modules()['management']['generation'] > previous and modules()['management']['ready'], 'management restart')
        current = modules()['management']
        found = []
        def recorded():
            found[:] = [record for record in local_logs() if record['message'] == 'worker started' and record.get('attributes', {}).get('module') == 'management' and record['attributes'].get('pid') == current['pid']]
            return bool(found)
        wait(recorded, 'actual supervisor lifecycle record')
        print('Actual product record', json.dumps(found[-1]), flush=True)
        return found[-1]

    def received(name, record):
        for row in rows(name):
            message = row.get('message', '')
            if not message:
                continue
            end = message.index('>')
            assert message.startswith('<'), message
            assert int(message[1:end]) // 8 == 16 and int(message[1:end]) % 8 <= 6, message
            body, separator, attributes = message.partition(' attributes=')
            identity = f" epoch={record['epoch']} sequence={record['sequence']}"
            if body.endswith(identity):
                assert ': info '+record['message']+' timestamp=' in body, message
                assert f" module={record['module']} pid={record['pid']} generation={record['generation']}" in body, message
                assert separator and json.loads(attributes) == record['attributes'], message
                return True
        return False

    def commit(resources, label):
        current = api('control.sock', '/v1/config')
        api('control.sock', '/v1/config/commit', {'expected_revision': current['revision'], 'request_id': label, 'config': {'schema_version': 2, 'resources': resources}})
        wait(lambda: api('control.sock', '/v1/config')['healthy'], label+' ready')

    def receiver(port, name):
        ready_count = sum(bool(row.get('ready')) for row in rows(name))
        process = start('syslog-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'receive', '--port', str(port), '--output', str(stage/name)], name)
        wait(lambda: sum(bool(row.get('ready')) for row in rows(name)) > ready_count, 'receiver bound to UDP '+str(port), 5)
        return process

    try:
        for name in namespaces:
            run(['ip', 'netns', 'add', name])
            run(['ip', '-n', name, 'link', 'set', 'lo', 'up'])
        for device, ns, address, gateway in [('wan0', 'syslog-peer', '192.0.2.2/24', '192.0.2.1'), ('lan0', 'syslog-client', '198.51.100.2/24', '198.51.100.1')]:
            run(['ip', 'link', 'add', device, 'netns', 'syslog-router', 'type', 'veth', 'peer', 'name', 'eth0', 'netns', ns])
            run(['ip', '-n', ns, 'link', 'set', 'eth0', 'up'])
            run(['ip', '-n', ns, 'address', 'add', address, 'dev', 'eth0'])
            run(['ip', '-n', ns, 'route', 'add', 'default', 'via', gateway])
        resources = [{'id': device, 'interface': {'name': device}} for device in ('wan0', 'lan0')]
        resources += [{'id': device+'-address', 'depends_on': [device], 'address': {'interface': device, 'cidr': address}} for device, address in [('wan0', '192.0.2.1/24'), ('lan0', '198.51.100.1/24')]]
        resources += [{'id': 'forward', 'forwarding': {'ipv4': True, 'ipv4_rp_filter': 0}}]
        sink = {'id': 'log-forward', 'service': {'name': 'remote-log', 'syslog': {'address': '192.0.2.2:5514', 'facility': 'local0', 'tag': 'routerd', 'min_level': 'info'}}}
        (stage/'config.json').write_text(json.dumps({'schema_version': 2, 'resources': resources+[sink]}))
        boot = {'platform': 'host', 'config': str(stage/'config.json'), 'runtime_dir': str(stage/'run'), 'state_dir': str(stage/'state'), 'workers': {role: str(stage/('routerd-logging' if role == 'logging' else 'routerd-modules')) for role in ('logging', 'network', 'control', 'management', 'syslog')}}
        (stage/'boot.json').write_text(json.dumps(boot))
        receiver(5514, 'receiver-a.jsonl')
        second = receiver(5515, 'receiver-b.jsonl')
        start('syslog-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'echo', '--port', '18081', '--output', str(stage/'echo.jsonl')], 'echo')
        wait(lambda: all(rows(name) for name in ('receiver-a.jsonl', 'receiver-b.jsonl', 'echo.jsonl')), 'independent receivers')
        start('syslog-router', [str(stage/'routerd'), 'boot', '--file', str(stage/'boot.json')], 'router')
        wait(lambda: (stage/'run/control.sock').exists(), 'control socket')
        wait(lambda: api('control.sock', '/v1/config')['healthy'] and modules().get('service.remote-log', {}).get('ready'), 'configured syslog worker')
        print('Product status', json.dumps(api('supervisor.sock', '/v1/status')), flush=True)
        client = start('syslog-client', ['python3', '-I', str(stage/'peer.py'), '--mode', 'client', '--output', str(stage/'forwarding.jsonl')], 'client')
        wait(lambda: len(rows('forwarding.jsonl')) >= 3, 'independent routed UDP replies')
        record = restart_management()
        wait(lambda: received('receiver-a.jsonl', record), 'actual product INFO at first destination')
        print('PASS actual supervisor INFO reached independent UDP receiver', flush=True)

        # This input proves filtering only. Lifecycle records above/below are
        # emitted by the product and independently prove end-to-end delivery.
        marker = 'fixture-debug-filter-only'
        api('logging.sock', '/v1/logs', {'entries': [{'timestamp': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'module': 'fixture', 'pid': os.getpid(), 'generation': 1, 'level': 'DEBUG', 'message': marker, 'producer_epoch': 'a'*32, 'producer_sequence': 1}]})
        assert any(r['message'] == marker for r in api('logging.sock', '/v1/logs?module=fixture&level=DEBUG')['records'])
        record = restart_management()
        wait(lambda: received('receiver-a.jsonl', record), 'post-DEBUG product record')
        assert all(marker not in r.get('message', '') for r in rows('receiver-a.jsonl'))
        print('PASS auxiliary DEBUG input retained locally and excluded remotely at info threshold', flush=True)

        sink['service']['syslog']['address'] = '192.0.2.2:5515'
        commit(resources+[sink], 'change-syslog-destination')
        record = restart_management()
        wait(lambda: received('receiver-b.jsonl', record), 'product record at changed destination')
        assert not received('receiver-a.jsonl', record)
        print('PASS configured destination changed; new lifecycle record reached only receiver B', flush=True)

        second.terminate()
        second.wait(timeout=5)
        before_disk = api('logging.sock', '/v1/status')['disk_written']
        before_forwarding = len(rows('forwarding.jsonl'))
        for _ in range(3):
            record = restart_management()
        wait(lambda: api('logging.sock', '/v1/status')['disk_written'] > before_disk, 'local disk records during receiver outage')
        wait(lambda: len(rows('forwarding.jsonl')) >= before_forwarding+10, 'forwarding during receiver outage')
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        receiver(5515, 'receiver-b.jsonl')
        recovered = False
        for attempt in range(1, 4):
            record = restart_management()
            deadline = time.monotonic()+3
            while time.monotonic() < deadline and not received('receiver-b.jsonl', record):
                time.sleep(.1)
            recovered = received('receiver-b.jsonl', record)
            print('Recovery observation', json.dumps({'attempt': attempt, 'epoch': record['epoch'], 'sequence': record['sequence'], 'received': recovered}), flush=True)
            if recovered:
                break
        assert recovered, 'no fresh product lifecycle record after receiver recovery'
        print('PASS receiver outage: local records/disk and independent routed UDP continued; fresh delivery recovered', flush=True)

        commit(resources, 'remove-syslog-destination')
        wait(lambda: 'service.remote-log' not in modules(), 'syslog worker removed')
        record = restart_management()
        deadline = time.monotonic()+2
        while time.monotonic() < deadline:
            assert not received('receiver-a.jsonl', record) and not received('receiver-b.jsonl', record)
            time.sleep(.1)
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        client.terminate()
        client.wait(timeout=5)
        print('PASS removal: later product records stayed local and UDP forwarding continued', flush=True)
        print('Forwarding result', json.dumps({'replies': len(rows('forwarding.jsonl')), 'failures': sum(not row['ok'] for row in rows('forwarding.jsonl'))}), flush=True)
    except Exception:
        for name in ('router.log', 'receiver-a.jsonl', 'receiver-b.jsonl'):
            path = stage/name
            if path.exists():
                print(name+' tail\n'+'\n'.join(path.read_text(errors='replace').splitlines()[-12:]), flush=True)
        raise
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
    parser.add_argument('--mode', choices=('receive', 'echo', 'client'))
    parser.add_argument('--port', type=int, default=5514)
    parser.add_argument('--binary-dir', type=Path, default=Path('dist/bin'))
    parser.add_argument('--output', type=Path, default=Path('dist/syslog-peer'))
    args = parser.parse_args()
    if args.mode:
        endpoint(args.mode, args.output, args.port)
        return
    if args.inside:
        peer(args.inside)
        return
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    args.output.mkdir(parents=True, exist_ok=True)
    stage = Path(run(root+['mktemp', '-d', '/tmp/routerd-syslog.XXXXXX']).strip())
    for name, source in [('peer.py', Path(__file__))]+[(name, args.binary_dir.resolve()/name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
        run(root+['install', '-m', '755', str(source), str(stage/name)])
    hashes = {name: hashlib.sha256((args.binary_dir/name).read_bytes()).hexdigest() for name in ('routerd', 'routerd-modules', 'routerd-logging')}
    (args.output/'binaries.json').write_text(json.dumps(hashes, indent=2)+'\n')
    print('Private stage', stage, flush=True)
    parent_netns = os.readlink('/proc/self/ns/net')
    with (args.output/'peer.log').open('w') as output:
        result = subprocess.run(root+['env', 'ROUTERD_SYSLOG_PARENT_NETNS='+parent_netns, 'unshare', '--mount', '--net', '--pid', '--fork', '--mount-proc', '--kill-child=SIGKILL', '--propagation', 'private', 'python3', '-I', str(stage/'peer.py'), '--inside', str(stage)], stdout=output, stderr=subprocess.STDOUT, timeout=150)
    for name in ('router.log', 'receiver-a.jsonl', 'receiver-b.jsonl', 'forwarding.jsonl'):
        artifact = subprocess.run(root+['cat', str(stage/name)], capture_output=True)
        if artifact.returncode == 0:
            (args.output/name).write_bytes(artifact.stdout)
    print((args.output/'peer.log').read_text(), end='')
    if result.returncode:
        # Keep the small state/log evidence, not duplicate release binaries.
        prune = 'from pathlib import Path; import sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-syslog.") and p.resolve()==p; [(p/n).unlink() for n in ("routerd","routerd-modules","routerd-logging")]'
        run(root+['python3', '-I', '-c', prune, str(stage)])
        raise RuntimeError('syslog acceptance failed; retained small evidence '+str(stage))
    # Only remove the exact disposable directory created above; artifacts were copied.
    cleanup = 'from pathlib import Path; import shutil,sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-syslog.") and p.resolve()==p; shutil.rmtree(p)'
    run(root+['python3', '-I', '-c', cleanup, str(stage)])
    print('PASS syslog acceptance; evidence '+str(args.output))


if __name__ == '__main__':
    main()
