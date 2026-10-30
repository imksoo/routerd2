# OTLP/gRPC logs and protocol measurements

Configure a native service with `otlp_logs`. The independent worker uses the
official OpenTelemetry Go SDKs and an explicit gRPC connection. It owns its
providers; it does not install a global provider or use `OTEL_*` variables to
select its endpoint, headers, resource, export interval, or sampling.

```json
{"id":"collector","service":{"name":"collector","otlp_logs":{
  "endpoint":"https://collector.example:4317",
  "service_name":"routerd2","service_namespace":"edge","min_level":"info",
  "signals":["logs","metrics","traces"]
}}}
```

`endpoint` accepts host:port or an HTTP/HTTPS URL with an explicit port.
Host:port and HTTPS use system-trusted TLS unless `insecure` is set. HTTP selects
plaintext. `headers` supplies explicit gRPC metadata, and `attributes` supplies
string resource attributes. `min_level` is debug, info, warning, or error; it
filters exported logs without suppressing selected measurements.

Native `signals` defaults to logs only. The former `Telemetry` resource defaults
to all three signals, matching its previous selection behavior. Its importer
reports the remaining source gap: metrics and traces currently cover actual
Health probes and DHCPv6 client exchanges. Controller, PPPoE, firewall and other former
instrumentation is not migrated by selecting these signals.

The existing local log owner remains authoritative. This worker reads its Unix
HTTP API and retains the original timestamp, severity, message, producer, local
cursor and scalar attributes. Readiness means the local cursor has been acquired,
not that a collector has acknowledged delivery. Worker startup, destination
replacement and a logger epoch change begin at the current cursor, without
replaying retained history. Temporary local API failure resumes the same cursor.

Health records its actual probe start and finish through the existing nonblocking
log handler. Export failure cannot delay the probe or renew its observation
lifetime. The exporter consumes that measurement once per local cursor and emits:

- `routerd.healthcheck.probes`, a cumulative counter grouped by resource, result,
  target and protocol, exported every 30 seconds;
- `routerd.healthcheck.phase`, the existing Healthy=2, Passing=1, Failing=-1,
  Unhealthy=-2 values grouped by resource and phase;
- `healthcheck.probe`, a span with the original start/end times, resource,
  interface, target, protocol, result and source log epoch/sequence. Every
  observed probe is sampled. Optional route evidence lookup is not part of the
  measured communication interval.

DHCPv6 uses the same local log owner and exporter providers. Each transmitted
transaction records its first send and completion. Elapsed monotonic time is
anchored to that send's UTC timestamp, so a clock correction cannot make its
duration negative. Retransmissions and partial Replies remain in the original
transaction; its acquisition, renewal and expiry deadlines are unchanged.

- `routerd.dhcpv6.client.exchanges`, a cumulative counter grouped by resource,
  interface, operation and result;
- `routerd.dhcpv6.client.exchange.duration`, a histogram in seconds with the same
  attributes and the existing 30-second collection interval;
- `dhcpv6.exchange`, a span with the original interval, successful transmission
  count, operation/result and source log epoch/sequence. `timeout`, `expired`,
  `no_binding` and `error` results carry error status.

Operations distinguish Solicit, Request, Renew, Rebind, Information-request,
Confirm and Decline. A `reply` means the accepted response ended that transaction;
for Solicit it means an offer was selected, not that a lease was acquired. The
interval includes retransmissions and offer selection, not a per-packet round
trip. A protocol timeout, lease expiry, NoBinding response, superseding state,
cancellation or local error is recorded separately. No DUID, assigned address,
authentication data or server identity is exported as a measurement attribute.

Counters reset when this exporter restarts, and phase series can coexist because
phase is an attribute. They describe observed records, not a durable audit of
every exchange or probe: local queue loss or history retention can omit measurements.

The SDK owns each bounded export queue. Logs use at most 1024 records, batches
of up to 200 and a 250 ms flush interval. Traces use at most 1024 spans, batches
of up to 256 and the same interval. Each export has a one-second deadline with
retries disabled. Failed exports and queue overflow can lose remote records;
there is no durable retransmission or exactly-once guarantee. Diagnostics from
the worker's own PID are not fed back into its exporter. All providers share one
two-second shutdown budget before the owned gRPC connection closes. No external
collector daemon is bundled into the product.

`make check-otlp OTELCOL=/path/to/otelcol-contrib` builds the product and runs the
independent official collector in disposable mount, process and network
namespaces. It checks actual supervisor records, metadata and severity filtering,
then actual TCP probe success, endpoint failure and recovery against received
logs, metrics and spans. It also checks destination replacement, collector
outage/recovery, bounded replacement/removal while offline, continuing Health
observations and local disk records, and independent UDP forwarding. ISC DHCPv6
provides actual prefix acquisition, natural renewal and T2 timeout/rebind recovery.
The received logs, metric count/duration and spans are matched to their original
local records. Collector outage must leave the DHCPv6 generation unchanged and
allow its prefix lifetime to advance.
It requires Linux, root or passwordless sudo, iproute2, curl, ISC dhcpd and an
installed collector. `scripts/otlp-peer.py --dhcp6-server /path/to/dhcpd` selects
an explicit independent DHCPv6 server executable.
It never changes host interfaces or the host's collector service.

On 2026-10-30, build `health-telemetry-90f9d96` passed this acceptance with
Collector 0.151.0. The three probe phases retained their source timestamps and
log identities, and 948 concurrent UDP exchanges succeeded without loss. Evidence
is in `dist/telemetry-signals-peer/peer.log`, receiver JSONL files and the saved
binary hashes.

On 2026-10-30 21:44:07-21:45:47 JST, build
`wsl-dhcp6-telemetry-e3f3bd4` passed the extended acceptance in WSL Ubuntu in
99.29 seconds. It was built from `e3f3bd4` plus the then-uncommitted measurement
changes, now product commit `2baeefa`; all six product source hashes match that
commit, and no product source changed after the build. Official Collector
0.151.0 and ISC dhcpd 4.4.3-P1 were independent peers. The fixture used an
18-second valid lifetime, 5-second T1 and 9-second T2, not an ISP's production
lease period. It observed acquisition, natural renewal, a 4.011671380-second
Renew timeout with three transmissions, and successful Rebind with two
transmissions. Logs, spans, error status, cumulative counts and duration sums
matched the original records. While the Collector was stopped, the same DHCPv6
worker generation renewed its prefix lifetime. All 970 concurrent UDP exchanges
succeeded. Health recovery and the existing exporter lifecycle checks also passed.

The reproducible path is `make build RELEASE_ID=wsl-dhcp6-telemetry-e3f3bd4`,
then `python3 scripts/otlp-peer.py --output dist/dhcp6-telemetry-peer` as root
(or with existing passwordless sudo). Evidence: `peer.log`, `binaries.json`,
`product-sources.json`, Collector JSONL and ISC log in that output directory.
The three executed SHA-256 values were:

- `routerd`: `1da234264bc90a942342d3ca21fa52faa7550f65fe07a965c914ed00f75c7244`
- `routerd-modules`: `9d5b7af154ce60fc590428e620ab785c11dc78781855eca732fd39d44b03bce0`
- `routerd-logging`: `1167ce8e5a1decbb7ff98f94bf661148dad8e5557d5ae9277cb2c5c8ad5f4a91`

This acceptance covers OTLP/gRPC plaintext and the exercised lifecycle. It does
not qualify other signal producers, archive-on-delete forwarding, automatic
LAN client address acquisition, physical-device performance, mutual TLS or
deployment-specific TLS trust.
