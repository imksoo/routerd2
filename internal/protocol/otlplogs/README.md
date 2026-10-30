# OTLP/gRPC logs and Health measurements

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
filters exported logs without suppressing selected probe measurements.

Native `signals` defaults to logs only. The former `Telemetry` resource defaults
to all three signals, matching its previous selection behavior. Its importer
reports the remaining source gap: metrics and traces currently cover actual
Health probes only. Controller, DHCPv6, PPPoE, firewall and other former
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

Counters reset when this exporter restarts, and phase series can coexist because
phase is an attribute. They describe observed records, not a durable audit of
every probe: local queue loss or history retention can omit measurements.

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
observations and local disk records, and independent UDP forwarding.
It requires Linux, passwordless sudo, iproute2, curl and an installed collector.
It never changes host interfaces or the host's collector service.

On 2026-10-30, build `health-telemetry-90f9d96` passed this acceptance with
Collector 0.151.0. The three probe phases retained their source timestamps and
log identities, and 948 concurrent UDP exchanges succeeded without loss. Evidence
is in `dist/telemetry-signals-peer/peer.log`, receiver JSONL files and the saved
binary hashes.

This acceptance covers OTLP/gRPC plaintext and the exercised lifecycle. It does
not qualify other signal producers, archive-on-delete forwarding, automatic
client address acquisition, physical-device performance, mutual TLS or
deployment-specific TLS trust.
