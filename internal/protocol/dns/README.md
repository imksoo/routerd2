# Native DNS module

This package adapts the Linux engine and tests from
`imksoo/routerd@1329ef54f296507eda79bbbc3b853783dd27251e`,
`cmd/routerd-dns-resolver`. The source notice and BSD license are in `NOTICE`.
It starts no external process and writes no addresses, routes, firewall rules,
or sysctls. The owning control module must prepare the network first.

## Callable contract

```go
type Config struct {
    Runtime dnsresolver.RuntimeConfig
    SocketPath string
    StatePath string
    EventPath string
    RuntimeConfigPath string
}

func Validate(Config) error
func Run(context.Context, Config, func()) error
```

`Runtime` uses the pinned original `pkg/dnsresolver` and `pkg/api` types. The
compiler must resolve address, upstream, bootstrap, zone-record, and conditional
resource references before invoking the module. Unresolved address/upstream
references are rejected. Initial configuration always comes from `Runtime`.
`RuntimeConfigPath` is optional and only supports an explicit file reload.

Socket and state paths must be clean, absolute, distinct paths. The optional
event journal and reload input must also be absolute and distinct. Query logging
uses `Runtime.Spec.QueryLog.Path`; if enabled with an empty path, it defaults to
`dns-queries.db` beside `StatePath`. Database and module paths may not collide.
Each process holds an exclusive advisory lock on `StatePath + ".lock"`.

`ready` fires once, after both UDP and TCP listeners have started and the private
Unix HTTP API is listening. Upstream health is reported separately and is not a
prerequisite for readiness. A configured query database that cannot open, a
listener failure, a missing explicitly configured lease file, or failed state
persistence prevents success. A listener that unexpectedly stops terminates the
worker. Context cancellation closes listeners, cancels upstream exchanges and
health probes, closes logging, persists stopped state, and normally returns nil.

The private API socket is mode 0600. The shared `internal/localapi` server refuses
to replace a live socket or a non-socket file, bounds request sizes and timeouts,
and closes the server on cancellation.

| API | Request / response |
| --- | --- |
| `GET /v1/healthz` | Ready health; HTTP 503 while unhealthy |
| `GET /v1/status` | Original `daemonapi.DaemonStatus` plus cache/query/log counters |
| `GET /v1/events?since=N` | Original cursor and event envelope, most recent 1,000 events |
| `GET /v1/config` | Current normalized `dnsresolver.RuntimeConfig` |
| `PUT /v1/config` | Strict JSON `dnsresolver.RuntimeConfig`; prepare and atomically replace live intent |
| `POST /v1/reload` | Read only the explicitly configured `RuntimeConfigPath`; otherwise error |
| `POST /v1/leases` | `{ "action": "added", "ip": "192.0.2.10", "hostname": "host", "mac": "..." }` |

Lease action names are the original `daemonapi` added/updated/removed actions.
The compatibility `Run` entry point preserves the original lease endpoint.
The bundled runtime uses `RunWithLeaseUpdates` when DHCP sources are configured;
in that mode this unscoped endpoint is rejected. Typed complete projections carry
source, epoch, sequence and the original publisher BOOTTIME deadline, and are validated
against each zone's configured source allowlist. Static records retain priority.
Both A and PTR answers expire at the earlier of the address lease and producer
ownership. Each answer checks the original boot identity/deadline, including
suspend elapsed time, and clamps its TTL to the remaining budget. There is no
receiver-clock lease; polling, replay and configuration reload cannot renew it.
Client lease expiry remains UTC and is checked separately.
Listener sockets and existing TCP connections stay open during lease updates.
Heartbeat-only updates do not invalidate the upstream cache.

Updates cannot change resource identity. They validate and prepare new listeners,
zone inputs, upstreams, and query databases before changing the active runtime.
An invalid update leaves the previous listeners and answers active. Unchanged
listener sockets stay open. Lease events serialize with reload so acknowledged
events cannot disappear between copying and installing zone tables. Runtime
updates do not rewrite a caller-owned configuration file; the owning configuration
transaction is responsible for restart persistence. A state-file failure after
the swap is explicitly reported as an applied update with failed persistence and
stops the worker.

## Functionality retained from the original engine

- UDP and TCP DNS listeners, IPv4/IPv6 freebind, source selection per listener.
- Ordered local-zone, conditional-forward, and upstream sources. Failure remains
  within the selected source's configured upstreams; a failed private forwarder
  never silently falls through to a public resolver.
- UDP, TCP, TLS/DoT, HTTPS/DoH and QUIC/DoQ upstreams, bootstrap DNS, interface
  binding, independent query/probe deadlines, health ranking and failover.
- Truncated UDP responses retry over TCP to the same server. Invalid messages,
  mismatched questions/IDs, SERVFAIL and REFUSED trigger failure handling;
  NXDOMAIN, NODATA and filtered successful answers remain final responses.
- Forwarded reply caching with entry/TTL limits, negative answers and generation
  fencing. Reload clears the cache and late old replies cannot repopulate it.
- Local A/AAAA/PTR records, static aliases, wildcard names, DHCP-derived names,
  explicit dnsmasq-format lease-file import, and recovered sticky-host snapshots.
  Static, current lease-file, retained live-event and sticky-record precedence is
  retained, including PTR alias behavior across reload.
- SQLite DNS query records and statistics, query-log recovery after a failed
  handle, resource status, and typed lifecycle/query/upstream/lease events.

## Corrections made during extraction

- Cache keys distinguish class, CD/RD/DO flags and EDNS options, including ECS.
  Transaction ID and name case alone do not split equivalent cache entries.
- Cached RR TTLs decrease with time and are capped by cache expiry; OPT flag bits
  are never interpreted as TTLs. The minimum record TTL, including the negative
  SOA minimum, determines message lifetime before configured bounds are applied.
- Cache storage has a 32 MiB accounting budget in addition to the entry limit.
  At most 256 DNS handlers perform active resolution; overload returns SERVFAIL
  and increments the `queryOverloads` status counter.
- Local zone matching respects label boundaries and chooses the most specific
  selected zone. Local answers set AA. They never assert AD without verification.
- DoQ queries carry ID zero and send request FIN before reading the response;
  response ID and FIN are checked and the original client ID is restored.
  DoT/DoH/DoQ verify certificates. IP-literal DoQ upstreams retain an IP certificate
  verification name. DoH validates its response media type. Bootstrap DNS permits
  the standard resolver's TCP fallback.
- UDP/TCP listener start is acknowledged rather than inferred from a sleep.
  All query transports obey the worker context. Outgoing HTTP idle connections
  close when their upstream pool stops.
- State writes are atomic, fsynced and mode 0600. Optional event history uses
  mode 0600 with 4 MiB rotation and one retained file. Persistence errors are
  observable; they are not discarded silently.
- Query database changes take effect on reload. Record writes have a 250 ms
  context budget and contention drops are counted rather than blocking DNS
  indefinitely behind disk writes. Record errors are counted and logged.

## Deliberate limits / integration still required

This is the original forwarding/local-zone engine, not a newly implemented
iterative recursive resolver or a complete authoritative DNS server. Local zone
records remain A/AAAA/PTR; DNS UPDATE, AXFR/IXFR, arbitrary authoritative record
types, zone signing, root trust-anchor management and cryptographic DNSSEC
validation are not implemented here. Local-zone `dnssec` settings are rejected:
the old engine asserted AD without signing or validating and that behavior is
not preserved. Forward-source `dnssecValidate` retains the original trust model:
it requests DNSSEC data and requires AD from the selected upstream. It is not
independent DNSSEC validation.

System TLS trust roots must exist in the appliance image. With hostname upstreams
and no explicit bootstrap addresses, Go's system resolver is used, as in the
original engine. Appliances should supply bootstrap addresses explicitly to avoid
an accidental dependency on a host resolver or a resolver loop. DoT and DoQ still
create a connection per exchange; connection pooling, 0-RTT and throughput tuning
are not qualified by these correctness tests.

The owning compiler/controller supplies resolved zones and source-scoped live DHCP
events, durable configuration transactions, module supervision, query database
retention, and integration of DNS events into the unified event stream. The
legacy DNS event cursor restarts at zero on worker restart; consumers must reset
their cursor when the supervised process generation changes. This package does
not claim parity for other original routerd services, compiler dependencies, or
hardware/cloud deployments.

## Verification

`go test -race ./internal/protocol/dns` runs the transplanted engine/failover/zone
tests and new module-level tests. Real private loopback UDP/TCP, TLS, HTTPS, QUIC
and Unix sockets verify responses, query matching, fallback, DoQ FIN/ID behavior,
hot updates, invalid-update rollback, database replacement, cache aging, and
cancellation with a pending upstream request. Socket tests need permission to
open local sockets; they never change host network configuration. The freebind
tests only bind sockets and do not assign the non-local addresses.

Protocol references: [RFC 9250 section 4.2](https://www.rfc-editor.org/rfc/rfc9250.html#section-4.2),
[RFC 2308 section 5](https://www.rfc-editor.org/rfc/rfc2308.html#section-5),
[RFC 8484](https://www.rfc-editor.org/rfc/rfc8484.html).
