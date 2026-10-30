# OTLP/gRPC local-log export

Configure a native service with `otlp_logs`. The exporter is an independent
worker, using the official OpenTelemetry Go log SDK and gRPC exporter. It has
its own provider and explicit connection; it does not install a global provider
or use `OTEL_*` variables to select an endpoint, headers, resource, or limits.

```json
{"id":"collector","service":{"name":"collector","otlp_logs":{
  "endpoint":"https://collector.example:4317",
  "service_name":"routerd2","service_namespace":"edge","min_level":"info"
}}}
```

`endpoint` accepts host:port or an HTTP/HTTPS URL with an explicit port.
Host:port and HTTPS use system-trusted TLS unless `insecure` is set. HTTP selects
plaintext. `headers` supplies explicit gRPC metadata, and `attributes` supplies
string resource attributes. `min_level` is debug, info, warning, or error.

The existing local log owner remains authoritative. This worker reads its Unix
HTTP API and retains the original timestamp, severity, message, producer, local
cursor and scalar attributes. Readiness means the local cursor has been acquired,
not that a collector has acknowledged delivery. Worker startup, destination
replacement and a logger epoch change begin at the current cursor, without
replaying retained history. Temporary local API failure resumes the same cursor.

The SDK owns the only export queue: at most 1024 records, batches of up to 200,
a 250 ms flush interval and a one-second export deadline. Failed exports and queue
overflow can lose remote records; there is no durable retransmission or exactly-once
guarantee. SDK diagnostics from the worker's own PID are not fed back into its
exporter. Shutdown drains the SDK within two seconds and closes the owned gRPC
connection. No external collector daemon is bundled into the product.

`make check-otlp OTELCOL=/path/to/otelcol-contrib` builds the product and runs the
independent official collector in disposable mount, process and network
namespaces. It checks actual supervisor records, metadata and severity filtering,
destination replacement, collector outage/recovery, bounded replacement/removal
while offline, local disk progress and uninterrupted independent UDP forwarding.
It requires Linux, passwordless sudo, iproute2, curl and an installed collector.
It never changes host interfaces or the host's collector service.

The executed `otlp-ba46e407` build with Collector 0.151.0 passed on 2026-10-30.
This proves OTLP/gRPC plaintext interoperability and the exercised lifecycle.
Metrics, traces, archive-on-delete forwarding, physical-device performance,
mutual TLS and deployment-specific TLS trust have not been qualified here.
