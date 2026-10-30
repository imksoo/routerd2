#!/usr/bin/env python3
"""Exercise product event retention with independent UDP receipt and forwarding.

Actual configuration commits supply the retained records. A separate receiver
observes before-delete archives. A receiver outage plus an unreachable log-only
route makes UDP submission fail; a silent UDP loss alone has no acknowledgement.
The independent client exercises IPv4 UDP forwarding, not physical hardware,
TCP continuity, automatic address acquisition, or reliable UDP log delivery.
"""
import argparse
import datetime
import hashlib
import html
import json
import os
from pathlib import Path
import re
import signal
import socket
import sqlite3
import subprocess
import time
from urllib.parse import urlencode


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
            sock.bind(('192.0.2.2' if mode == 'echo' else '192.0.2.3', port))
            with output.open('a', buffering=1) as log:
                log.write(json.dumps({'ready': True})+'\n')
                while True:
                    message, peer = sock.recvfrom(65535)
                    if mode == 'echo':
                        sock.sendto(message, peer)
                    else:
                        log.write(json.dumps({'time': time.time(), 'peer': peer, 'message': message.decode('utf-8')})+'\n')


def peer(stage, dns_queries=False):
    children = []
    namespaces = ('retention-router', 'retention-peer', 'retention-client')
    assert os.readlink('/proc/self/ns/net') != os.environ['ROUTERD_RETENTION_PARENT_NETNS'], 'requires private network namespace'
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
        return json.loads(run(inside('retention-router', command), 8))

    def modules():
        return {item['name']: item for item in api('supervisor.sock', '/v1/status')['modules']}

    def observations():
        result, after = [], 0
        while True:
            page = api('control.sock', '/v1/observations?after_cursor='+str(after)+'&limit=16')
            result.extend(page['records'] or [])
            if not page['more']:
                return result
            after = page['next_cursor']

    def capacity():
        return api('control.sock', '/v1/event-store')

    def commit(policy, label, healthy=True):
        current = api('control.sock', '/v1/config')
        current_cursor = max((r['cursor'] for r in observations()), default=0)
        api('control.sock', '/v1/config/commit', {'expected_revision': current['revision'], 'request_id': label, 'config': {'schema_version': 2, 'resources': resources, 'retention': policy}})
        if healthy:
            wait(lambda: api('control.sock', '/v1/config')['healthy'], label+' ready')
        wait(lambda: any(r['event']['topic'] == 'routerd.retention.observed' and r['cursor'] > current_cursor for r in observations()), label+' rule consumed commit')
        for record in observations():
            original[record['event']['id']] = record['event']
        print('Product commit', label, 'revision', api('control.sock', '/v1/config')['revision'], flush=True)

    def retained_ids():
        return {record['event']['id'] for record in observations()}

    def reclaimed():
        # Read only this fixture's owned database. Current state/work and the
        # latest registration anchor may legitimately exceed a history target.
        with sqlite3.connect('file:'+str(stage/'state/events/journal.sqlite3')+'?mode=ro', uri=True) as database:
            referenced = {row[0] for row in database.execute('SELECT state_cursor FROM consumers WHERE stateful=1 UNION SELECT event_cursor FROM outbox UNION SELECT max(cursor) FROM events')}
            remaining = {row[0] for row in database.execute('SELECT cursor FROM events')}
            return remaining <= referenced

    def delivered():
        found = {}
        for row in rows('receiver-a.jsonl'):
            wire = row.get('message', '')
            if ': info retained event ' not in wire:
                continue
            payload = wire.split(': info retained event ', 1)[1]
            event, _ = json.JSONDecoder().raw_decode(payload)
            if event['id'] in original:
                assert event == original[event['id']], 'archive changed original event'
                found[event['id']] = event
        return found

    def receiver(port, name):
        ready_count = sum(bool(row.get('ready')) for row in rows(name))
        process = start('retention-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'receive', '--port', str(port), '--output', str(stage/name)], name)
        wait(lambda: sum(bool(row.get('ready')) for row in rows(name)) > ready_count, 'receiver bound to UDP '+str(port), 5)
        return process

    try:
        for name in namespaces:
            run(['ip', 'netns', 'add', name])
            run(['ip', '-n', name, 'link', 'set', 'lo', 'up'])
        for device, ns, address, gateway in [('wan0', 'retention-peer', '192.0.2.2/24', '192.0.2.1'), ('lan0', 'retention-client', '198.51.100.2/24', '198.51.100.1')]:
            run(['ip', 'link', 'add', device, 'netns', 'retention-router', 'type', 'veth', 'peer', 'name', 'eth0', 'netns', ns])
            run(['ip', '-n', ns, 'link', 'set', 'eth0', 'up'])
            run(['ip', '-n', ns, 'address', 'add', address, 'dev', 'eth0'])
            run(['ip', '-n', ns, 'route', 'add', 'default', 'via', gateway])
        resources = [{'id': device, 'interface': {'name': device}} for device in ('wan0', 'lan0')]
        resources += [{'id': device+'-address', 'depends_on': [device], 'address': {'interface': device, 'cidr': address}} for device, address in [('wan0', '192.0.2.1/24'), ('lan0', '198.51.100.1/24')]]
        resources += [{'id': 'forward', 'forwarding': {'ipv4': True, 'ipv4_rp_filter': 0}}]
        resources += [{'id': 'observe-commits', 'event_rule': {'pattern': {'operator': 'count', 'topic': 'configuration.committed', 'threshold': 1}, 'emit': {'topic': 'routerd.retention.observed'}}}]
        run(['ip', '-n', 'retention-peer', 'address', 'add', '192.0.2.3/24', 'dev', 'eth0'])
        destination = {'address': '192.0.2.3:5514', 'facility': 'local0', 'tag': 'routerd', 'min_level': 'info'}
        policy = {'max_age_seconds': 3600, 'max_records': 3, 'interval_seconds': 1, 'vacuum': True, 'syslog': [destination]}
        if dns_queries:
            import yaml
            source = {"apiVersion": "routerd.net/v1alpha1", "kind": "Router", "metadata": {"name": "dns-retention"}, "spec": {"resources": []}}
            def add(version, kind, name, spec):
                source["spec"]["resources"].append({"apiVersion": version+".routerd.net/v1alpha1", "kind": kind, "metadata": {"name": name}, "spec": spec})
            add("net", "DNSResolver", "resolver", {"listen": [{"name": "lan", "addresses": ["198.51.100.1"], "port": 53, "sources": ["local"]}], "queryLog": {"enabled": True, "path": "/legacy/dns-queries.db"}})
            add("net", "DNSZone", "home", {"zone": "home.arpa", "records": [{"hostname": "*.lab", "ipv4": "203.0.113.25"}]})
            add("net", "DNSForwarder", "local", {"resolver": "DNSResolver/resolver", "match": ["home.arpa"], "zoneRefs": ["DNSZone/home"]})
            add("system", "LogSink", "archive", {"type": "syslog", "syslog": {"network": "udp", "address": destination['address'], "facility": "local0", "tag": "routerd"}})
            add("system", "LogRetention", "queries", {"retention": "2s", "schedule": "daily", "vacuum": True, "signals": ["dnsQueries"], "sinks": ["LogSink/archive"]})
            (stage/'legacy.json').write_text(json.dumps(source))
            imported = yaml.safe_load(run([str(stage/'routerd'), 'config', 'migrate', str(stage/'legacy.json')]))
            dns_service = next(r['service'] for r in imported['resources'] if r.get('service', {}).get('dns'))
            dns_name = dns_service['name']
            dns_policy = dns_service['dns']['query_log_retention']
            assert dns_policy['max_age_seconds'] == 2 and dns_policy['interval_seconds'] == 86400 and dns_policy['vacuum']
            dns_policy['interval_seconds'] = 1  # Short natural-expiry acceptance window.
            resources.extend(imported['resources'])
            resources.append({'id': 'console', 'service': {'name': 'console', 'web_console': {'listen': '198.51.100.1:8088', 'base_path': '/ops/', 'title': 'routerd2'}}})
            resources.append({'id': 'console-input', 'firewall': {'disable_forward': True, 'input': {'policy': 'drop', 'rules': [
                {'match': {'in_interface': 'lo'}, 'action': 'accept'},
                *[{'match': {'in_interface': 'lan0', 'protocol': protocol, 'destination_ports': [{'start': port}]}, 'action': 'accept'} for protocol, port in (('udp', 53), ('tcp', 53), ('tcp', 8088))],
            ]}}})
        document = {'schema_version': 2, 'resources': resources}
        if not dns_queries:
            document['retention'] = policy
        (stage/'config.json').write_text(json.dumps(document))
        boot = {'platform': 'host', 'config': str(stage/'config.json'), 'runtime_dir': str(stage/'run'), 'state_dir': str(stage/'state'), 'workers': {role: str(stage/('routerd-logging' if role == 'logging' else 'routerd-modules')) for role in ('logging', 'network', 'control', 'management', 'dns', 'syslog')}}
        if dns_queries:
            boot['workers']['web_console'] = str(stage/'routerd-modules')
        (stage/'boot.json').write_text(json.dumps(boot))
        receive = receiver(5514, 'receiver-a.jsonl')
        start('retention-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'echo', '--port', '18081', '--output', str(stage/'echo.jsonl')], 'echo')
        wait(lambda: bool(rows('echo.jsonl')), 'independent echo receiver')
        product = start('retention-router', [str(stage/'routerd'), 'boot', '--file', str(stage/'boot.json')], 'router')
        wait(lambda: (stage/'run/control.sock').exists(), 'control socket')
        wait(lambda: api('control.sock', '/v1/config')['healthy'], 'product ready')
        start('retention-client', ['python3', '-I', str(stage/'peer.py'), '--mode', 'client', '--output', str(stage/'forwarding.jsonl')], 'client')
        wait(lambda: len(rows('forwarding.jsonl')) >= 3, 'independent routed UDP replies')
        if dns_queries:
            query_path = stage/'state/services'/dns_name/'dns-queries.db'
            def query_records():
                with sqlite3.connect('file:'+str(query_path)+'?mode=ro', uri=True) as db:
                    db.row_factory = sqlite3.Row
                    return {(r['ts'], r['question_name']): dict(r) for r in db.execute('SELECT * FROM dns_queries')}
            def query(label):
                for transport in ('+notcp', '+tcp'):
                    answer = run(inside('retention-client', ['dig', '@198.51.100.1', label+'.lab.home.arpa', 'A', '+short', '+tries=1', '+time=1', transport]))
                    assert answer.strip() == '203.0.113.25', answer
                wait(lambda: len([key for key in query_records() if key[1] == label+'.lab.home.arpa']) >= 2, 'real UDP/TCP queries recorded', 2)
            def console(expected, save=False, **filters):
                params = {'service': 'service.'+dns_name, 'limit': 100, **filters}
                page = run(inside('retention-client', ['curl', '--fail-with-body', '--max-time', '3', '-sS', 'http://198.51.100.1:8088/ops/dns-queries?'+urlencode(params)]))
                found = set(re.findall(r'data-query="([0-9]+)"', page))
                assert found == {str(row['id']) for row in expected.values()}, ('DNS console returned different records', filters, found, expected)
                content = html.unescape(page)
                for row in expected.values():
                    for value in (row['client_address'], row['question_name'], row['question_type'], row['response_code'], *(json.loads(row['answers_json'] or '[]') or [])):
                        assert value in content, ('DNS console omitted recorded value', value)
                if save:
                    (stage/'dns-console.html').write_text(page)
            def query_status():
                return api('services/'+dns_name+'.dns.sock', '/v1/status')['resources'][0]['observed']
            def archived_queries():
                result = {}
                for row in rows('receiver-a.jsonl'):
                    wire = row.get('message', '')
                    if ': info dnsQueries ' in wire:
                        record, _ = json.JSONDecoder().raw_decode(wire.split(': info dnsQueries ', 1)[1])
                        result[(record['ts'], record['question_name'])] = record
                return result
            since = datetime.datetime.now(datetime.timezone.utc).isoformat()
            query('before-expiry')
            negative = run(inside('retention-client', ['dig', '@198.51.100.1', 'missing.home.arpa', 'A', '+comments', '+answer', '+tries=1', '+time=1']))
            assert 'status: NXDOMAIN' in negative, negative
            wait(lambda: any(row['response_code'] == 'NXDOMAIN' for row in query_records().values()), 'negative DNS query recorded', 2)
            original_queries = query_records()
            until = datetime.datetime.now(datetime.timezone.utc).isoformat()
            successful = {key: row for key, row in original_queries.items() if row['response_code'] == 'NOERROR'}
            failed = {key: row for key, row in original_queries.items() if row['response_code'] == 'NXDOMAIN'}
            console(original_queries, save=True)
            console(successful, client='198.51.100.2', name='%.lab.home.arpa', response='NOERROR', since=since, until=until)
            console(failed, response='NXDOMAIN')
            console({}, client='198.51.100.99')
            console({}, since=until)
            wait(lambda: not query_records(), 'original DNS query expiry and vacuum')
            console({})
            print('PASS independent HTTP client filtered actual DNS client, name, response and time; natural expiry removed displayed rows', flush=True)
            assert all(archived_queries().get(key) == value for key, value in original_queries.items()), 'archive changed or lost original query columns'
            print('PASS imported query logging, natural expiry, original-column UDP archive and vacuum', flush=True)

            receive.terminate()
            receive.wait(timeout=5)
            run(['ip', '-n', 'retention-router', 'route', 'add', 'unreachable', '192.0.2.3/32'])
            query('archive-unreachable')
            protected = query_records()
            wait(lambda: bool(query_status().get('queryRetentionError')), 'visible query archive failure')
            for number in range(3):
                query('while-unreachable-'+str(number))
            assert protected.items() <= query_records().items(), 'archive failure deleted expired queries'
            console(protected, name='archive-unreachable.lab.home.arpa')
            before = modules()['service.'+dns_name]['generation']
            api('control.sock', '/v1/services/service.'+dns_name+'/restart', {})
            wait(lambda: modules()['service.'+dns_name]['generation'] > before and modules()['service.'+dns_name]['ready'], 'DNS restart during archive outage')
            console(protected, name='archive-unreachable.lab.home.arpa')
            assert all(r['ok'] for r in rows('forwarding.jsonl'))
            print('PASS independent HTTP client read protected DNS rows before and after resolver restart', flush=True)
            print('PASS unreachable archive retained expired rows while real UDP/TCP DNS and routed UDP continued', flush=True)

            receive = receiver(5514, 'receiver-a.jsonl')
            run(['ip', '-n', 'retention-router', 'route', 'delete', 'unreachable', '192.0.2.3/32'])
            wait(lambda: not query_records() and not query_status().get('queryRetentionError'), 'query archive recovery')
            console({})
            assert all(archived_queries().get(key) == value for key, value in protected.items()), 'recovery changed or lost protected rows'
            before = modules()['service.'+dns_name]['generation']
            api('control.sock', '/v1/services/service.'+dns_name+'/restart', {})
            wait(lambda: modules()['service.'+dns_name]['generation'] > before and modules()['service.'+dns_name]['ready'], 'DNS worker restart')
            query('after-restart')
            wait(lambda: not query_records(), 'retention after resolver restart')
            print('PASS receiver recovery and resolver restart retained the policy and restored expiry', flush=True)

            dns_service['dns'].pop('query_log_retention')
            current = api('control.sock', '/v1/config')
            api('control.sock', '/v1/config/commit', {'expected_revision': current['revision'], 'request_id': 'remove-dns-retention', 'config': document})
            wait(lambda: api('control.sock', '/v1/config')['healthy'], 'retention removal')
            query('after-policy-removal')
            retained = query_records()
            time.sleep(3)
            query('after-removal-deadline')
            assert retained.items() <= query_records().items(), 'removed policy still deleted query history'
            print('PASS policy removal stopped expiry without stopping DNS or query recording', flush=True)
            return
        original = {}
        for number in range(8):
            policy['max_age_seconds'] = 3600+number
            commit(policy, 'record-limit-'+str(number))
        wait(lambda: capacity()['capacity']['history']['rows'] <= 3, 'record-count limit')
        assert len(delivered()) >= 5
        assert all(identity in delivered() for identity in original if identity not in retained_ids())
        print('PASS count limit and original event receipt before deletion', json.dumps(capacity()), flush=True)

        policy.pop('max_records')
        policy['max_bytes'] = 1
        commit(policy, 'byte-limit')
        wait(lambda: reclaimed(), 'event-body byte limit')
        print('PASS body-byte limit', flush=True)

        policy.pop('max_bytes')
        policy['max_age_seconds'] = 2
        commit(policy, 'age-limit')
        wait(lambda: reclaimed(), 'age limit')
        print('PASS observed-time expiry and incremental vacuum', json.dumps(capacity()), flush=True)

        receive.terminate()
        receive.wait(timeout=5)
        # This log-only route affects neither the echo peer nor client traffic.
        # UDP silence itself cannot prove remote failure, so exercise a real
        # sender error separately from stopping the receiver.
        run(['ip', '-n', 'retention-router', 'route', 'add', 'unreachable', '192.0.2.3/32'])
        commit(policy, 'archive-unreachable')
        protected = retained_ids()
        wait(lambda: bool(capacity().get('retention_error')), 'visible UDP submission error')
        for _ in range(2):
            status = capacity()
            assert status.get('last_error') and status.get('retention_error'), status
        before = len(rows('forwarding.jsonl'))
        cursor = max(r['cursor'] for r in observations())
        for number in range(3):
            policy['max_age_seconds'] = 2+number
            commit(policy, 'accepted-during-outage-'+str(number), healthy=False)
        assert max(r['cursor'] for r in observations()) > cursor
        assert protected <= retained_ids(), 'failed archive deleted retained evidence'
        wait(lambda: len(rows('forwarding.jsonl')) >= before+10, 'forwarding while archive fails')
        assert all(r['ok'] for r in rows('forwarding.jsonl'))
        print('PASS submission error remained visible; old candidates retained; new product records and routed UDP progressed', flush=True)

        receive = receiver(5514, 'receiver-a.jsonl')
        run(['ip', '-n', 'retention-router', 'route', 'delete', 'unreachable', '192.0.2.3/32'])
        wait(lambda: reclaimed() and not capacity().get('retention_error'), 'receiver-path recovery and retained backlog deletion')
        assert protected <= delivered().keys()
        wait(lambda: api('control.sock', '/v1/config')['healthy'], 'storage alarm recovery')
        print('PASS archive recovery delivered retained originals and cleared the alarm', flush=True)

        policy['max_age_seconds'] = 2
        commit(policy, 'before-restart')
        old_generation = modules()['control']['generation']
        api('supervisor.sock', '/v1/modules/control/restart', {})
        wait(lambda: modules()['control']['generation'] > old_generation and modules()['control']['ready'], 'control restart')
        wait(lambda: api('control.sock', '/v1/config')['healthy'], 'restarted persisted configuration')
        assert api('control.sock', '/v1/config')['config']['retention'] == policy
        commit(policy, 'after-restart')
        wait(lambda: reclaimed(), 'restarted retention')
        assert all(r['ok'] for r in rows('forwarding.jsonl'))
        print('PASS restart retained policy, accepted fresh product events and expired their history', flush=True)
        destination['min_level'] = 'warning'
        commit(policy, 'archive-info-filtered-by-warning')
        filtered = retained_ids()
        assert filtered
        wait(lambda: reclaimed(), 'filtered archive expiry')
        assert not filtered.intersection(delivered()), 'INFO archive bypassed warning threshold'
        print('PASS archive INFO respects warning threshold while original history expires', flush=True)
        print('Forwarding result', json.dumps({'replies': len(rows('forwarding.jsonl')), 'failures': sum(not row['ok'] for row in rows('forwarding.jsonl'))}), flush=True)
    except Exception:
        for name in ('router.log', 'receiver-a.jsonl'):
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
    parser.add_argument('--dns-queries', action='store_true', help='Exercise imported DNS query retention instead of event history')
    parser.add_argument('--mode', choices=('receive', 'echo', 'client'))
    parser.add_argument('--port', type=int, default=5514)
    parser.add_argument('--binary-dir', type=Path, default=Path('dist/bin'))
    parser.add_argument('--output', type=Path, default=Path('dist/retention-peer'))
    args = parser.parse_args()
    if args.mode:
        endpoint(args.mode, args.output, args.port)
        return
    if args.inside:
        peer(args.inside, args.dns_queries)
        return
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    args.output.mkdir(parents=True, exist_ok=True)
    stage = Path(run(root+['mktemp', '-d', '/tmp/routerd-retention.XXXXXX']).strip())
    for name, source in [('peer.py', Path(__file__))]+[(name, args.binary_dir.resolve()/name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
        run(root+['install', '-m', '755', str(source), str(stage/name)])
    hashes = {name: hashlib.sha256((args.binary_dir/name).read_bytes()).hexdigest() for name in ('routerd', 'routerd-modules', 'routerd-logging')}
    (args.output/'binaries.json').write_text(json.dumps(hashes, indent=2)+'\n')
    print('Private stage', stage, flush=True)
    parent_netns = os.readlink('/proc/self/ns/net')
    with (args.output/'peer.log').open('w') as output:
        result = subprocess.run(root+['env', 'ROUTERD_RETENTION_PARENT_NETNS='+parent_netns, 'unshare', '--mount', '--net', '--pid', '--fork', '--mount-proc', '--kill-child=SIGKILL', '--propagation', 'private', 'python3', '-I', str(stage/'peer.py'), '--inside', str(stage)]+(['--dns-queries'] if args.dns_queries else []), stdout=output, stderr=subprocess.STDOUT, timeout=150)
    for name in ('router.log', 'receiver-a.jsonl', 'forwarding.jsonl', 'dns-console.html'):
        artifact = subprocess.run(root+['cat', str(stage/name)], capture_output=True)
        if artifact.returncode == 0:
            (args.output/name).write_bytes(artifact.stdout)
    print((args.output/'peer.log').read_text(), end='')
    if result.returncode:
        # Keep the small state/log evidence, not duplicate release binaries.
        prune = 'from pathlib import Path; import sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-retention.") and p.resolve()==p; [(p/n).unlink() for n in ("routerd","routerd-modules","routerd-logging")]'
        run(root+['python3', '-I', '-c', prune, str(stage)])
        raise RuntimeError('retention acceptance failed; retained small evidence '+str(stage))
    # Only remove the exact disposable directory created above; artifacts were copied.
    cleanup = 'from pathlib import Path; import shutil,sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-retention.") and p.resolve()==p; shutil.rmtree(p)'
    run(root+['python3', '-I', '-c', cleanup, str(stage)])
    print('PASS retention acceptance; evidence '+str(args.output))


if __name__ == '__main__':
    main()
