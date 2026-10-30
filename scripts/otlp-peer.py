#!/usr/bin/env python3
"""Receive product OTLP/gRPC logs and real Health/DHCPv6 measurements in private namespaces.

Worker lifecycle records prove the product-to-receiver path. One explicitly
labelled synthetic DEBUG input checks only severity filtering, not log origin.
An independent TCP endpoint proves Health success, failure, and recovery in logs,
metrics, and traces. ISC dhcpd proves prefix acquisition, natural renewal, and
recovery after the server stops. An independent client exercises IPv4 UDP
forwarding during Collector outages. This does not qualify physical hardware,
LAN prefix distribution, TCP forwarding continuity, TLS, other signal producers,
or reliable delivery during outages.
"""
import argparse
import datetime
import hashlib
import json
import os
import re
from pathlib import Path
import socket
import subprocess
import time

import yaml


def run(command, timeout=10, input=None):
    result = subprocess.run(command, input=input, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{command[0]}: {result.stdout[-2000:]}{result.stderr[-2000:]}')
    return result.stdout


def endpoint(mode, output, port):
    if mode == 'tcp':
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            sock.bind(('192.0.2.2', port))
            sock.listen()
            with output.open('a', buffering=1) as log:
                while True:
                    connection, peer = sock.accept()
                    with connection:
                        log.write(json.dumps({'peer': peer, 'accepted_at_ns': time.time_ns()})+'\n')
        return
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        if mode == 'client':
            sock.settimeout(1)
            sock.connect(('192.0.2.2', 18081))
            with output.open('a', buffering=1) as log:
                sequence = 0
                while True:
                    sequence += 1
                    message = f'otlp-forwarding-{sequence}'.encode()
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
                    sock.sendto(message, peer)


def peer(stage):
    children = []
    namespaces = ('otlp-router', 'otlp-peer', 'otlp-client')
    assert os.readlink('/proc/self/ns/net') != os.environ['ROUTERD_OTLP_PARENT_NETNS'], 'requires private network namespace'
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
        return json.loads(run(inside('otlp-router', command), 8))

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

    def value(v):
        if 'kvlistValue' in v:
            return {item['key']: value(item['value']) for item in v['kvlistValue'].get('values', [])}
        for key in ('stringValue', 'boolValue', 'doubleValue'):
            if key in v:
                return v[key]
        if 'intValue' in v:
            return int(v['intValue'])
        return None

    def records(name):
        for batch in rows(name):
            for resource in batch.get('resourceLogs', []):
                resource_attributes = {item['key']: value(item['value']) for item in resource.get('resource', {}).get('attributes', [])}
                for scope in resource.get('scopeLogs', []):
                    for record in scope.get('logRecords', []):
                        yield resource_attributes, record

    def received(name, original):
        for resource_attributes, record in records(name):
            attributes = {item['key']: value(item['value']) for item in record.get('attributes', [])}
            if attributes.get('routerd.log.epoch') != original['epoch'] or attributes.get('routerd.log.sequence') != str(original['sequence']):
                continue
            assert record['body']['stringValue'] == original['message'], record
            assert record['severityNumber'] == 9 and record['severityText'] == 'INFO', record
            assert attributes['routerd.module'] == original['module'], attributes
            assert attributes['process.pid'] == original['pid'], attributes
            assert attributes['routerd.generation'] == str(original['generation']), attributes
            assert attributes['routerd.attributes'] == original['attributes'], attributes
            expected_time = int(datetime.datetime.fromisoformat(original['timestamp'].replace('Z', '+00:00')).timestamp()*1e9)
            assert abs(int(record['timeUnixNano'])-expected_time) < 2000, record
            assert resource_attributes['service.name'] == 'routerd2' and resource_attributes['service.namespace'] == 'acceptance', resource_attributes
            return True
        return False

    def attributes(item):
        return {entry['key']: value(entry['value']) for entry in item.get('attributes', [])}

    def nanos(stamp):
        parsed = datetime.datetime.fromisoformat(stamp.replace('Z', '+00:00'))
        fraction = re.search(r'\.(\d+)', stamp)
        return int(parsed.replace(microsecond=0).timestamp())*1_000_000_000+int(fraction.group(1).ljust(9, '0') if fraction else '0')

    def health_record(result, phase, after):
        found = []
        def completed():
            entries = api('logging.sock', '/v1/logs?module=service.probe&tail=true&limit=100')['records']
            found[:] = [entry for entry in entries if entry['message'] == 'health probe completed'
                        and entry.get('attributes', {}).get('result') == result
                        and entry['attributes'].get('phase') == phase
                        and nanos(entry['attributes']['probe_started_at']) >= after]
            return bool(found)
        wait(completed, 'actual TCP probe '+result+'/'+phase, 15)
        original = found[-1]
        details = original['attributes']
        assert details['resource'] == 'probe' and details['interface'] == 'wan0', details
        assert details['target'] == '192.0.2.2' and details['protocol'] == 'tcp', details
        assert nanos(details['probe_finished_at']) >= nanos(details['probe_started_at']), details
        print('Actual Health measurement', json.dumps(original), flush=True)
        return original

    def span_received(name, original):
        for batch in rows(name):
            for resource in batch.get('resourceSpans', []):
                for scope in resource.get('scopeSpans', []):
                    for span in scope.get('spans', []):
                        tags = attributes(span)
                        if tags.get('routerd.log.epoch') != original['epoch'] or tags.get('routerd.log.sequence') != original['sequence']:
                            continue
                        details = original['attributes']
                        fields = [('routerd.resource.name', 'resource'), ('network.interface.name', 'interface')]
                        if original['message'] == 'dhcpv6 exchange completed':
                            assert span['name'] == 'dhcpv6.exchange', span
                            fields += [('routerd.dhcpv6.operation', 'operation'), ('routerd.dhcpv6.result', 'result'),
                                       ('routerd.dhcpv6.transmissions', 'transmissions')]
                            assert tags['network.protocol.name'] == 'dhcpv6', tags
                            if details['result'] in ('timeout', 'expired', 'no_binding', 'error'):
                                assert span.get('status', {}).get('code') == 2, span
                            started, finished = details['exchange_started_at'], details['exchange_finished_at']
                        else:
                            assert span['name'] == 'healthcheck.probe', span
                            fields += [('server.address', 'target'), ('network.protocol.name', 'protocol'),
                                       ('routerd.healthcheck.result', 'result')]
                            started, finished = details['probe_started_at'], details['probe_finished_at']
                        for key, source in fields:
                            assert tags[key] == details[source], tags
                        assert int(span['startTimeUnixNano']) == nanos(started), span
                        assert int(span['endTimeUnixNano']) == nanos(finished), span
                        resource_tags = attributes(resource.get('resource', {}))
                        assert resource_tags['service.name'] == 'routerd2' and resource_tags['service.namespace'] == 'acceptance', resource_tags
                        return True
        return False

    def metric_points(name, metric_name, kind, unit=None):
        for batch in rows(name):
            for resource in batch.get('resourceMetrics', []):
                for scope in resource.get('scopeMetrics', []):
                    for metric in scope.get('metrics', []):
                        if metric.get('name') == metric_name:
                            if unit is not None:
                                assert metric.get('unit') == unit, metric
                            resource_tags = attributes(resource.get('resource', {}))
                            assert resource_tags['service.name'] == 'routerd2' and resource_tags['service.namespace'] == 'acceptance', resource_tags
                            yield from metric[kind].get('dataPoints', [])

    def measured(original, previous_count=0):
        details = original['attributes']
        finished = nanos(details['probe_finished_at'])
        counts = []
        def delivered():
            if not received('receiver-a.jsonl', original) or not span_received('receiver-a.jsonl', original):
                return False
            counts[:] = [int(point['asInt']) for point in metric_points('receiver-a.jsonl', 'routerd.healthcheck.probes', 'sum')
                         if attributes(point) == {'routerd.resource.name': 'probe', 'routerd.healthcheck.result': details['result'],
                                                  'server.address': '192.0.2.2', 'network.protocol.name': 'tcp'}
                         and int(point['timeUnixNano']) >= finished and int(point['asInt']) > previous_count]
            # Phase is part of the metric identity; an older Healthy series may
            # coexist with Unhealthy. Require a new collection after this probe.
            phases = [point for point in metric_points('receiver-a.jsonl', 'routerd.healthcheck.phase', 'gauge')
                      if attributes(point) == {'routerd.resource.name': 'probe', 'routerd.healthcheck.phase': details['phase']}
                      and int(point['timeUnixNano']) >= finished]
            expected = {'Healthy': 2, 'Unhealthy': -2}[details['phase']]
            assert all(int(point['asInt']) == expected for point in phases), phases
            return bool(counts and phases)
        # The product uses the SDK PeriodicReader at 30 seconds; this permits
        # one export interval without changing the actual probe timing.
        wait(delivered, 'log/span/metrics for '+details['result']+'/'+details['phase'], 45)
        count = max(counts)
        print('PASS real TCP probe log/span/metrics', json.dumps({'epoch': original['epoch'], 'sequence': original['sequence'],
              'result': details['result'], 'phase': details['phase'], 'counter': count,
              'started_at': details['probe_started_at'], 'finished_at': details['probe_finished_at']}), flush=True)
        return count

    def dhcp6_record(operation, result, after):
        found = []
        def completed():
            entries = api('logging.sock', '/v1/logs?module=service.pd&tail=true&limit=100')['records']
            found[:] = [entry for entry in entries if entry['message'] == 'dhcpv6 exchange completed'
                        and entry.get('attributes', {}).get('operation') == operation
                        and entry['attributes'].get('result') == result
                        and nanos(entry['attributes']['exchange_finished_at']) >= after]
            return bool(found)
        wait(completed, 'actual DHCPv6 '+operation+'/'+result, 25)
        original = found[-1]
        details = original['attributes']
        assert details['resource'] == 'pd' and details['interface'] == 'wan0', details
        assert details['transmissions'] >= 1, details
        assert nanos(details['exchange_finished_at']) >= nanos(details['exchange_started_at']), details
        print('Actual DHCPv6 measurement', json.dumps(original), flush=True)
        return original

    def dhcp6_measured(original):
        details = original['attributes']
        finished = nanos(details['exchange_finished_at'])
        duration = (finished-nanos(details['exchange_started_at']))/1_000_000_000
        tags = {'routerd.resource.name': 'pd', 'network.interface.name': 'wan0',
                'routerd.dhcpv6.operation': details['operation'], 'routerd.dhcpv6.result': details['result']}
        evidence = []
        def delivered():
            if not received('receiver-a.jsonl', original) or not span_received('receiver-a.jsonl', original):
                return False
            counts = [int(point['asInt']) for point in metric_points('receiver-a.jsonl', 'routerd.dhcpv6.client.exchanges', 'sum')
                      if attributes(point) == tags and int(point['timeUnixNano']) >= finished]
            histograms = [point for point in metric_points('receiver-a.jsonl', 'routerd.dhcpv6.client.exchange.duration', 'histogram', 's')
                          if attributes(point) == tags and int(point['timeUnixNano']) >= finished]
            for point in histograms:
                count = int(point['count'])
                if count not in counts:
                    continue
                assert float(point['sum']) >= duration-1e-9, point
                # Acquisition and the single induced timeout have one observation;
                # their exported sum must preserve the measured exchange interval.
                if count == 1:
                    assert abs(float(point['sum'])-duration) < 1e-9, point
                evidence[:] = [count, point['sum']]
                return True
            return False
        wait(delivered, 'DHCPv6 log/span/counter/duration '+details['operation']+'/'+details['result'], 45)
        print('PASS real DHCPv6 log/span/counter/duration', json.dumps({'operation': details['operation'],
              'result': details['result'], 'epoch': original['epoch'], 'sequence': original['sequence'],
              'started_at': details['exchange_started_at'], 'finished_at': details['exchange_finished_at'],
              'counter': evidence[0], 'duration_sum_seconds': evidence[1]}), flush=True)

    def dhcp6_peer():
        process = start('otlp-peer', [str(stage/'dhcpd'), '-6', '-f', '-d', '-cf', str(stage/'dhcp6.conf'),
                                    '-lf', str(stage/'dhcp6.leases'), '-pf', str(stage/'dhcp6.pid'), 'eth0'], 'dhcp6')
        def listening():
            if process.poll() is not None:
                raise RuntimeError('independent DHCPv6 server exited: '+(stage/'dhcp6.log').read_text()[-2000:])
            return bool(run(inside('otlp-peer', ['ss', '-H', '-lun', 'sport = :547'])))
        wait(listening, 'independent ISC DHCPv6 server', 10)
        return process

    def probe_peer():
        process = start('otlp-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'tcp', '--port', '18082',
                                    '--output', str(stage/'tcp-probe.jsonl')], 'tcp-probe')
        def listening():
            if process.poll() is not None:
                raise RuntimeError('independent TCP probe endpoint exited')
            return bool(run(inside('otlp-peer', ['ss', '-H', '-ltn', 'sport = :18082'])))
        wait(listening, 'independent TCP probe endpoint', 10)
        return process

    def commit(resources, label):
        current = api('control.sock', '/v1/config')
        api('control.sock', '/v1/config/commit', {'expected_revision': current['revision'], 'request_id': label, 'config': {'schema_version': 2, 'resources': resources}})
        wait(lambda: api('control.sock', '/v1/config')['healthy'], label+' ready')

    def receiver(port, name):
        config = {'receivers': {'otlp': {'protocols': {'grpc': {'endpoint': '192.0.2.2:'+str(port)}}}},
                  'exporters': {'file': {'path': str(stage/name), 'append': True, 'flush_interval': '100ms'}},
                  'service': {'telemetry': {'logs': {'level': 'error'}, 'metrics': {'level': 'none'}},
                              'pipelines': {signal: {'receivers': ['otlp'], 'exporters': ['file']} for signal in ('logs', 'metrics', 'traces')}}}
        path = stage/(name+'.collector.json')
        path.write_text(json.dumps(config))
        process = start('otlp-peer', [os.environ['ROUTERD_OTLP_COLLECTOR'], '--config', str(path)], name)
        def listening():
            if process.poll() is not None:
                raise RuntimeError('collector exited: '+(stage/(name+'.log')).read_text())
            return bool(run(inside('otlp-peer', ['ss', '-H', '-ltn', 'sport = :'+str(port)])))
        wait(listening, 'collector listening on gRPC '+str(port), 10)
        return process

    try:
        for name in namespaces:
            run(['ip', 'netns', 'add', name])
            run(['ip', '-n', name, 'link', 'set', 'lo', 'up'])
        for device, ns, address, gateway in [('wan0', 'otlp-peer', '192.0.2.2/24', '192.0.2.1'), ('lan0', 'otlp-client', '198.51.100.2/24', '198.51.100.1')]:
            run(['ip', 'link', 'add', device, 'netns', 'otlp-router', 'type', 'veth', 'peer', 'name', 'eth0', 'netns', ns])
            run(['ip', '-n', ns, 'link', 'set', 'eth0', 'up'])
            run(['ip', '-n', ns, 'address', 'add', address, 'dev', 'eth0'])
            run(['ip', '-n', ns, 'route', 'add', 'default', 'via', gateway])
        run(['ip', '-n', 'otlp-peer', '-6', 'address', 'add', '2001:db8:123::2/64', 'dev', 'eth0'])
        (stage/'dhcp6.leases').touch()
        (stage/'dhcp6.conf').write_text('default-lease-time 18; preferred-lifetime 12; option dhcp-renewal-time 5; option dhcp-rebinding-time 9; subnet6 2001:db8:123::/64 { prefix6 2001:db8:400:: 2001:db8:400:f0:: /60; }\n')
        resources = [{'id': device, 'interface': {'name': device}} for device in ('wan0', 'lan0')]
        resources += [{'id': 'wan6-address', 'depends_on': ['wan0'], 'address': {'interface': 'wan0', 'cidr': '2001:db8:123::1/64'}}]
        resources += [{'id': device+'-address', 'depends_on': [device], 'address': {'interface': device, 'cidr': address}} for device, address in [('wan0', '192.0.2.1/24'), ('lan0', '198.51.100.1/24')]]
        resources += [{'id': 'forward', 'forwarding': {'ipv4': True, 'ipv4_rp_filter': 0}}]
        resources += [{'id': 'probe', 'depends_on': ['wan0-address'], 'service': {'name': 'probe', 'health': {
            'interface': 'wan0', 'source_address': '192.0.2.1', 'target': '192.0.2.2', 'protocol': 'tcp', 'port': 18082,
            'interval_ms': 500, 'timeout_ms': 500, 'healthy_threshold': 2, 'unhealthy_threshold': 2}}}]
        legacy = {'apiVersion': 'routerd.net/v1alpha1', 'kind': 'Router', 'metadata': {'name': 'otlp-acceptance'}, 'spec': {'resources': [
            {'apiVersion': 'observability.routerd.net/v1alpha1', 'kind': 'Telemetry', 'metadata': {'name': 'collector'}, 'spec': {
                'otlp': {'endpoint': 'http://192.0.2.2:4317', 'insecure': True}, 'serviceNamespace': 'acceptance', 'signals': ['logs', 'metrics', 'traces']}},
            {'apiVersion': 'system.routerd.net/v1alpha1', 'kind': 'LogSink', 'metadata': {'name': 'otlp'}, 'spec': {
                'type': 'otlp', 'minLevel': 'info', 'otlp': {'telemetryRef': 'Telemetry/collector'}}}]}}
        (stage/'legacy.json').write_text(json.dumps(legacy))
        migration = subprocess.run([str(stage/'routerd'), 'config', 'migrate', str(stage/'legacy.json')], capture_output=True, text=True, timeout=10)
        assert migration.returncode == 0, migration.stderr
        print('Migration review', migration.stderr, flush=True)
        imported = yaml.safe_load(migration.stdout)
        assert len(imported['resources']) == 1, 'legacy OTLP LogSink created an additional sender'
        sink = imported['resources'][0]
        sender = 'service.'+sink['service']['name']
        (stage/'config.json').write_text(json.dumps({'schema_version': 2, 'resources': resources+[sink]}))
        boot = {'platform': 'host', 'config': str(stage/'config.json'), 'runtime_dir': str(stage/'run'), 'state_dir': str(stage/'state'), 'workers': {role: str(stage/('routerd-logging' if role == 'logging' else 'routerd-modules')) for role in ('logging', 'network', 'control', 'management', 'health', 'dhcp6', 'otlp_logs')}}
        (stage/'boot.json').write_text(json.dumps(boot))
        receiver(4317, 'receiver-a.jsonl')
        second = receiver(4318, 'receiver-b.jsonl')
        start('otlp-peer', ['python3', '-I', str(stage/'peer.py'), '--mode', 'echo', '--port', '18081', '--output', str(stage/'echo.jsonl')], 'echo')
        wait(lambda: rows('echo.jsonl'), 'independent UDP echo receiver')
        tcp_peer = probe_peer()
        start('otlp-router', [str(stage/'routerd'), 'boot', '--file', str(stage/'boot.json')], 'router')
        wait(lambda: (stage/'run/control.sock').exists(), 'control socket')
        wait(lambda: api('control.sock', '/v1/config')['healthy'] and modules().get(sender, {}).get('ready'), 'configured otlp worker')
        print('Product status', json.dumps(api('supervisor.sock', '/v1/status')), flush=True)
        client = start('otlp-client', ['python3', '-I', str(stage/'peer.py'), '--mode', 'client', '--output', str(stage/'forwarding.jsonl')], 'client')
        wait(lambda: len(rows('forwarding.jsonl')) >= 3, 'independent routed UDP replies')
        record = restart_management()
        wait(lambda: received('receiver-a.jsonl', record), 'actual product INFO at first destination')
        print('PASS actual supervisor INFO reached independent OpenTelemetry Collector over gRPC', flush=True)

        # This input proves filtering only. Lifecycle records above/below are
        # emitted by the product and independently prove end-to-end delivery.
        marker = 'fixture-debug-filter-only'
        api('logging.sock', '/v1/logs', {'entries': [{'timestamp': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'module': 'fixture', 'pid': os.getpid(), 'generation': 1, 'level': 'DEBUG', 'message': marker, 'producer_epoch': 'a'*32, 'producer_sequence': 1}]})
        assert any(r['message'] == marker for r in api('logging.sock', '/v1/logs?module=fixture&level=DEBUG')['records'])
        record = restart_management()
        wait(lambda: received('receiver-a.jsonl', record), 'post-DEBUG product record')
        assert all(r.get('body', {}).get('stringValue') != marker for _, r in records('receiver-a.jsonl'))
        print('PASS auxiliary DEBUG input retained locally and excluded remotely at info threshold', flush=True)

        wait(lambda: any(a.get('local') == '2001:db8:123::2' and not a.get('tentative')
                         for link in json.loads(run(['ip', '-n', 'otlp-peer', '-j', '-6', 'address', 'show', 'dev', 'eth0']))
                         for a in link['addr_info']), 'DHCPv6 peer address DAD')
        dhcp_peer = dhcp6_peer()
        acquisition_started = time.time_ns()
        resources += [{'id': 'pd', 'depends_on': ['wan6-address'], 'service': {'name': 'pd', 'dhcp6': {
            'interface': 'wan0', 'iaid': 1, 'request_prefix': True, 'prefix_length': 60}}}]
        commit(resources+[sink], 'start-dhcp6-with-collector-ready')
        solicitation = dhcp6_record('solicit', 'reply', acquisition_started)
        acquisition = dhcp6_record('request', 'reply', acquisition_started)
        wait(lambda: api('control.sock', '/v1/services/service.pd')['snapshot'].get('prefixes'), 'delegated prefix acquired')
        renewal = dhcp6_record('renew', 'reply', nanos(acquisition['attributes']['exchange_finished_at']))
        healthy = health_record('passed', 'Healthy', time.time_ns())
        first_count = measured(healthy)
        for observation in (solicitation, acquisition, renewal):
            dhcp6_measured(observation)
        assert any(row['peer'][0] == '192.0.2.1' for row in rows('tcp-probe.jsonl')), 'no independent TCP accept'
        tcp_peer.terminate()
        tcp_peer.wait(timeout=5)
        stopped_at = time.time_ns()
        dhcp_peer.terminate()
        dhcp_peer.wait(timeout=5)
        unhealthy = health_record('failed', 'Unhealthy', stopped_at)
        timed_out = dhcp6_record('renew', 'timeout', stopped_at)
        dhcp_peer = dhcp6_peer()
        rebound = dhcp6_record('rebind', 'reply', stopped_at)
        wait(lambda: api('control.sock', '/v1/services/service.pd')['snapshot'].get('state') == 'bound', 'PD recovered after server outage')
        measured(unhealthy)
        for observation in (timed_out, rebound):
            dhcp6_measured(observation)
        print('PASS ISC DHCPv6 acquisition, natural renewal, T2 timeout and rebind recovery', flush=True)
        accepted_before = len(rows('tcp-probe.jsonl'))
        tcp_peer = probe_peer()
        healthy = health_record('passed', 'Healthy', time.time_ns())
        measured(healthy, first_count)
        assert len(rows('tcp-probe.jsonl')) > accepted_before, 'no independent TCP accept after recovery'
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        print('PASS independent TCP stop/recovery changed actual Health measurements; routed UDP continued', flush=True)

        sink['service']['otlp_logs']['endpoint'] = 'http://192.0.2.2:4318'
        commit(resources+[sink], 'change-otlp-destination')
        record = restart_management()
        wait(lambda: received('receiver-b.jsonl', record), 'product record at changed destination')
        assert not received('receiver-a.jsonl', record)
        print('PASS configured destination changed; new lifecycle record reached only receiver B', flush=True)

        second.terminate()
        second.wait(timeout=5)
        before_disk = api('logging.sock', '/v1/status')['disk_written']
        before_probe = api('control.sock', '/v1/services/service.probe')
        before_pd = api('control.sock', '/v1/services/service.pd')
        collector_stopped_at = time.time_ns()
        before_forwarding = len(rows('forwarding.jsonl'))
        for _ in range(3):
            record = restart_management()
        wait(lambda: api('logging.sock', '/v1/status')['disk_written'] > before_disk, 'local disk records during receiver outage')
        wait(lambda: len(rows('forwarding.jsonl')) >= before_forwarding+10, 'forwarding during receiver outage')
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        def probing():
            current = api('control.sock', '/v1/services/service.probe')
            assert current['generation'] == before_probe['generation'], 'collector outage restarted Health producer'
            return current['snapshot']['observed_nanos'] > before_probe['snapshot']['observed_nanos']
        wait(probing, 'actual Health observation during collector outage')
        print('PASS Health observations advanced without restarting during collector outage', flush=True)
        offline_renewal = dhcp6_record('renew', 'reply', collector_stopped_at)
        def renewing():
            current = api('control.sock', '/v1/services/service.pd')
            assert current['generation'] == before_pd['generation'], 'collector outage restarted DHCPv6 producer'
            return (current['snapshot']['observed_nanos'] > before_pd['snapshot']['observed_nanos']
                    and current['snapshot']['prefixes'][0]['valid_until']['nanos']
                    > before_pd['snapshot']['prefixes'][0]['valid_until']['nanos'])
        wait(renewing, 'actual PD renewal during collector outage', 15)
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        print('PASS DHCPv6 lease deadline advanced in the same generation while Collector was stopped',
              json.dumps({'sequence': offline_renewal['sequence'], 'generation': before_pd['generation']}), flush=True)
        old = modules()[sender]
        sink['service']['otlp_logs']['attributes'] = {'phase': 'replacement-with-collector-offline'}
        replacement_started = time.monotonic()
        commit(resources+[sink], 'replace-otlp-while-collector-down')
        wait(lambda: modules()[sender]['generation'] > old['generation'], 'replacement without collector')
        assert time.monotonic()-replacement_started < 6, 'collector outage blocked replacement'
        assert not Path('/proc/'+str(old['pid'])).exists(), 'old exporter survived replacement'
        print('PASS configuration replacement while collector stopped; old worker exited', flush=True)
        second = receiver(4318, 'receiver-b.jsonl')
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

        second.terminate()
        second.wait(timeout=5)
        record = restart_management()
        old = modules()[sender]
        removed_at = time.monotonic()
        commit(resources, 'remove-otlp-destination')
        wait(lambda: sender not in modules(), 'otlp worker removed')
        assert time.monotonic()-removed_at < 6, 'collector outage blocked shutdown'
        assert not Path('/proc/'+str(old['pid'])).exists(), 'removed exporter survived shutdown'
        receiver(4318, 'receiver-b.jsonl')
        record = restart_management()
        deadline = time.monotonic()+2
        while time.monotonic() < deadline:
            assert not received('receiver-a.jsonl', record) and not received('receiver-b.jsonl', record)
            time.sleep(.1)
        assert all(row['ok'] for row in rows('forwarding.jsonl'))
        client.terminate()
        client.wait(timeout=5)
        print('PASS removal: later product records stayed local and UDP forwarding continued', flush=True)
        identities = set()
        for name in ('receiver-a.jsonl', 'receiver-b.jsonl'):
            for _, exported in records(name):
                attributes = {item['key']: value(item['value']) for item in exported.get('attributes', [])}
                identity = (attributes['routerd.log.epoch'], attributes['routerd.log.sequence'])
                assert identity not in identities, 'duplicate delivery from legacy LogSink and Telemetry'
                identities.add(identity)
        print('PASS imported Telemetry and legacy OTLP LogSink produced one delivery per received event', flush=True)
        print('Forwarding result', json.dumps({'replies': len(rows('forwarding.jsonl')), 'failures': sum(not row['ok'] for row in rows('forwarding.jsonl'))}), flush=True)
    except Exception:
        for name in ('router.log', 'dhcp6.log', 'receiver-a.jsonl', 'receiver-b.jsonl'):
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
    parser.add_argument('--mode', choices=('echo', 'client', 'tcp'))
    parser.add_argument('--port', type=int, default=18081)
    parser.add_argument('--collector', type=Path, default=Path('/usr/local/bin/otelcol-contrib'))
    parser.add_argument('--binary-dir', type=Path, default=Path('dist/bin'))
    parser.add_argument('--dhcp6-server', type=Path, default=Path('/usr/sbin/dhcpd'))
    parser.add_argument('--output', type=Path, default=Path('dist/otlp-peer'))
    args = parser.parse_args()
    if args.mode:
        endpoint(args.mode, args.output, args.port)
        return
    if args.inside:
        peer(args.inside)
        return
    root = [] if os.geteuid() == 0 else ['sudo', '-n']
    assert args.collector.is_file(), 'install the official otelcol-contrib binary or pass --collector'
    assert args.dhcp6_server.is_file(), 'install ISC dhcpd or pass --dhcp6-server'
    print(run([str(args.collector), '--version']).strip(), flush=True)
    version = subprocess.run([str(args.dhcp6_server), '--version'], capture_output=True, text=True, timeout=10, check=True)
    print((version.stdout+version.stderr).strip(), flush=True)
    args.output.mkdir(parents=True, exist_ok=True)
    stage = Path(run(root+['mktemp', '-d', '/tmp/routerd-otlp.XXXXXX']).strip())
    for name, source in [('peer.py', Path(__file__)), ('dhcpd', args.dhcp6_server.resolve())]+[(name, args.binary_dir.resolve()/name) for name in ('routerd', 'routerd-modules', 'routerd-logging')]:
        run(root+['install', '-m', '755', str(source), str(stage/name)])
    hashes = {name: hashlib.sha256((args.binary_dir/name).read_bytes()).hexdigest() for name in ('routerd', 'routerd-modules', 'routerd-logging')}
    (args.output/'binaries.json').write_text(json.dumps(hashes, indent=2)+'\n')
    print('Private stage', stage, flush=True)
    parent_netns = os.readlink('/proc/self/ns/net')
    # Three distinct Health phases each await the 30-second metric reader,
    # in addition to the existing lifecycle and collector-outage acceptance.
    # DHCPv6 acquisition/renewal/server recovery share these collection periods.
    with (args.output/'peer.log').open('w') as output:
        result = subprocess.run(root+['env', 'ROUTERD_OTLP_PARENT_NETNS='+parent_netns, 'ROUTERD_OTLP_COLLECTOR='+str(args.collector.resolve()), 'unshare', '--mount', '--net', '--pid', '--fork', '--mount-proc', '--kill-child=SIGKILL', '--propagation', 'private', 'python3', '-I', str(stage/'peer.py'), '--inside', str(stage)], stdout=output, stderr=subprocess.STDOUT, timeout=240)
    for name in ('router.log', 'receiver-a.jsonl', 'receiver-b.jsonl', 'forwarding.jsonl', 'tcp-probe.jsonl', 'dhcp6.log'):
        artifact = subprocess.run(root+['cat', str(stage/name)], capture_output=True)
        if artifact.returncode == 0:
            (args.output/name).write_bytes(artifact.stdout)
    print((args.output/'peer.log').read_text(), end='')
    if result.returncode:
        # Keep the small state/log evidence, not duplicate release binaries.
        prune = 'from pathlib import Path; import sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-otlp.") and p.resolve()==p; [(p/n).unlink() for n in ("routerd","routerd-modules","routerd-logging","dhcpd")]'
        run(root+['python3', '-I', '-c', prune, str(stage)])
        raise RuntimeError('otlp acceptance failed; retained small evidence '+str(stage))
    # Only remove the exact disposable directory created above; artifacts were copied.
    cleanup = 'from pathlib import Path; import shutil,sys; p=Path(sys.argv[1]); assert p.parent==Path("/tmp") and p.name.startswith("routerd-otlp.") and p.resolve()==p; shutil.rmtree(p)'
    run(root+['python3', '-I', '-c', cleanup, str(stage)])
    print('PASS otlp acceptance; evidence '+str(args.output))


if __name__ == '__main__':
    main()
