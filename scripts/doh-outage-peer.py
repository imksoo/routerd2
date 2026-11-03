#!/usr/bin/env python3
# SPDX-License-Identifier: BSD-3-Clause
"""Use actual product daemons, public DoH peers and an independent dig client.

Run in the existing uplink fixture's private mount namespace, with a further
private mount namespace for this trial. The parent client0 LAN must provide the
independent gateway 198.18.0.253. Addresses .200 and .201 are outside its pools.
Only the two new namespaces receive configuration and packet-drop rules.
Cache is disabled to require real upstream queries, not previously cached answers.
"""

import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import tempfile
import time

parser = argparse.ArgumentParser(description='Check real public DoH failure and recovery on an isolated node attached to an existing uplink fixture LAN.')
parser.add_argument('--parent', required=True, help='Existing client namespace on the 198.18.0.0/24 fixture LAN')
parser.add_argument('--bin-dir', type=Path, required=True, help='Normal Makefile release binaries')
args = parser.parse_args()
stage = Path(tempfile.mkdtemp(prefix='routerd-doh-live.'))
names = ['r2doh-' + str(os.getpid()) + '-' + n for n in ('router', 'client')]
children = []

def run(cmd, timeout=10):
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
    if p.returncode:
        raise RuntimeError(str(cmd[:3]) + ': ' + p.stdout[-1200:] + p.stderr[-1200:])
    return p.stdout

def inside(i, cmd):
    return ['ip', 'netns', 'exec', names[i]] + cmd

def api(sock, path):
    return json.loads(run(['curl', '-fsS', '--max-time', '3', '--unix-socket', str(stage / 'run' / sock), 'http://localhost' + path]))

def wait(f, seconds=45):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        try:
            if f():
                return
        except (RuntimeError, ValueError, FileNotFoundError):
            pass
        time.sleep(0.2)
    raise RuntimeError('readiness did not converge')

def query(label):
    for transport in ('+notcp', '+tcp'):
        start = time.monotonic()
        out = run(inside(1, ['dig', '@198.18.0.200', 'www.example.net', 'A', '+time=4', '+tries=1', transport]), 7)
        print(label, transport, 'seconds', round(time.monotonic() - start, 3), 'rcode', next((x for x in out.splitlines() if 'status:' in x), ''), flush=True)
        if 'status: NOERROR' not in out or not re.search(r'\sIN\s+A\s+', out):
            raise RuntimeError('DNS response did not contain a usable address: ' + out)

def status():
    return api('services/dns.dns.sock', '/v1/status')['resources'][0]['observed']['upstreams']
def stop(signum, frame):
    if signum == signal.SIGALRM:
        raise TimeoutError('bounded DoH trial expired')
    raise KeyboardInterrupt()


for sig in (signal.SIGALRM, signal.SIGTERM, signal.SIGINT):
    signal.signal(sig, stop)
signal.alarm(300)
try:
    for i, n in enumerate(names):
        run(['ip', 'netns', 'add', n])
        run(inside(i, ['ip', 'link', 'set', 'lo', 'up']))
        run(['ip', '-n', args.parent, 'link', 'add', 'link', 'client0', 'name', 'eth0', 'netns', n, 'type', 'macvlan', 'mode', 'bridge'])
        run(inside(i, ['ip', 'link', 'set', 'eth0', 'address', '02:dd:00:00:00:0' + str(i + 1), 'up']))
    run(inside(1, ['ip', 'address', 'add', '198.18.0.201/24', 'dev', 'eth0']))
    for binary in ('routerd', 'routerd-modules', 'routerd-logging'):
        shutil.copy2(args.bin_dir / binary, stage / binary)
    document = {
        'schema_version': 2,
        'resources': [
            {'id': 'link', 'interface': {'name': 'eth0'}},
            {'id': 'address', 'depends_on': ['link'],
             'address': {'interface': 'eth0', 'cidr': '198.18.0.200/24'}},
            {'id': 'default', 'depends_on': ['address'],
             'route': {'destination': '0.0.0.0/0', 'gateway': '198.18.0.253',
                       'interface': 'eth0'}},
            {'id': 'service/dns', 'depends_on': ['address', 'default'],
             'service': {'name': 'dns', 'dns': {
                 'listen_interface': 'eth0',
                 'spec': {
                     'listen': [{'name': 'lan', 'addresses': ['198.18.0.200'], 'port': 53}],
                     'cache': {'enabled': False},
                     'queryLog': {'enabled': True},
                     'sources': [{
                         'name': 'encrypted-public', 'kind': 'forward', 'match': ['.'],
                         'upstreams': ['https://cloudflare-dns.com/dns-query',
                                       'https://dns.google/dns-query'],
                         'queryTimeout': '1s',
                         'healthcheck': {'interval': '15s', 'timeout': '3s',
                                         'failThreshold': 3, 'passThreshold': 2},
                         'bootstrapResolver': ['1.1.1.1', '8.8.8.8'],
                     }],
                 },
             }}},
        ],
    }
    (stage / 'config.json').write_text(json.dumps(document))
    boot = {'platform': 'host', 'config': str(stage / 'config.json'), 'runtime_dir': str(stage / 'run'), 'state_dir': str(stage / 'state'), 'workers': {r: str(stage / ('routerd-logging' if r == 'logging' else 'routerd-modules')) for r in ('logging', 'network', 'control', 'management', 'dns')}}
    (stage / 'boot.json').write_text(json.dumps(boot))
    with (stage / 'product.log').open('w') as out:
        p = subprocess.Popen(inside(0, [str(stage / 'routerd'), 'boot', '--file', str(stage / 'boot.json')]), stdout=out, stderr=subprocess.STDOUT)
    children.append(p)
    print('Private DoH trial', stage, 'pid', p.pid, flush=True)
    wait(lambda: api('control.sock', '/v1/config')['healthy'])
    query('baseline')
    wait(lambda: all((x + ':Healthy' in status() for x in ('https://cloudflare-dns.com/dns-query', 'https://dns.google/dns-query'))))
    print('both peers healthy', status(), flush=True)
    peers = {}
    for host in ('cloudflare-dns.com', 'dns.google'):
        answer = run(inside(0, ['dig', '@1.1.1.1', host, 'A', '+short', '+time=2', '+tries=1']))
        peers[host] = [x for x in answer.split() if x.replace('.', '').isdigit() and ipaddress.ip_address(x).version == 4]
        if not peers[host]:
            raise RuntimeError('bootstrap returned no IPv4 addresses')
    run(inside(0, ['nft', 'add', 'table', 'inet', 'doh_probe']))
    run(inside(0, ['nft', 'add', 'chain', 'inet', 'doh_probe', 'out', '{ type filter hook output priority -10; policy accept; }']))
    for host in ('cloudflare-dns.com', 'dns.google'):
        run(inside(0, ['nft', 'flush', 'chain', 'inet', 'doh_probe', 'out']))
        expression = 'ip daddr { ' + ', '.join(peers[host]) + ' } tcp dport 443 counter drop'
        run(inside(0, ['nft', 'add', 'rule', 'inet', 'doh_probe', 'out', expression]))
        for i in range(3):
            query('blocked ' + host + ' query' + str(i))
        wait(lambda: 'https://' + host + '/dns-query:Down' in status(), seconds=65)
        query('confirmed down ' + host)
        print('blocked state', status(), flush=True)
        counter = run(inside(0, ['nft', 'list', 'chain', 'inet', 'doh_probe', 'out']))
        print('drop counter', counter, flush=True)
        if not re.search(r'counter packets [1-9][0-9]* ', counter):
            raise RuntimeError('No actual packets were blocked')
        run(inside(0, ['nft', 'flush', 'chain', 'inet', 'doh_probe', 'out']))
        wait(lambda: all((x + ':Healthy' in status() for x in ('https://cloudflare-dns.com/dns-query', 'https://dns.google/dns-query'))))
        query('restored ' + host)
    print('PASS both real public DoH endpoints reached Down and recovered; uncached UDP/TCP client resolution continued', flush=True)
except BaseException:
    f = stage / 'product.log'
    if f.exists():
        print('product diagnostics', f.read_text(errors='replace')[-3000:], flush=True)
    raise
finally:
    signal.alarm(0)
    for p in reversed(children):
        if p.poll() is None:
            p.terminate()
        try:
            p.wait(timeout=20)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait(timeout=3)
    for n in reversed(names):
        q = subprocess.run(['ip', 'netns', 'pids', n], capture_output=True, text=True)
        for pid in q.stdout.split():
            try:
                os.kill(int(pid), signal.SIGKILL)
            except ProcessLookupError:
                pass
        subprocess.run(['ip', 'netns', 'del', n], capture_output=True)
    shutil.rmtree(stage)
    print('Private DoH node and clients removed', flush=True)
