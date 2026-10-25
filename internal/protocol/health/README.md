# Native health worker

`service.health` runs ICMP, TCP, DNS or HTTP against a literal unicast target.
An interface is mandatory. `source_address` and `mark` optionally constrain the
same socket path. TCP/DNS/HTTP reuse the pinned routerd healthcheck probe code;
ICMP uses a bound raw socket and validates the peer, echo ID, sequence and random
payload. DNS probes query example.com through the specified DNS server; HTTP
retains the pinned reachability contract (2xx–4xx).

The pinned `healthcheck.ApplyResult` supplies Passing/Healthy/Failing/Unhealthy
thresholds and history. Output history is bounded to 20 entries. The `.active`
fact follows the existing egress readiness contract: Healthy and pre-threshold
Failing are eligible. Passing and Unhealthy are not eligible. Missing/expired
facts remain indeterminate. The finite CLOCK_BOOTTIME lifetime is
`max(2 minutes, 3*interval + timeout)`; supervisor readiness/generation also gates
consumption. There is no generic replay renewal. Shutdown publishes withdrawal.

History storage is independent of probing. A bounded background recorder keeps
accepted events durable and reports dropped uncommitted events through
`history_dropped` and `history_error` in the current snapshot. The loss count
belongs to the worker's producer epoch and resets on replacement. Storage or
receiver failure does not pause probes or current observations. See
[history delivery](../../protocolevents/README.md) for queue and loss limits.

Configuration example: `examples/health-pbr.yaml`. It keeps a probe host route
independent of the application routing table. Health activates the primary
metric-100 route, with an unconditional metric-200 backup and a source policy
rule. Both routes, policy rule, and probe path retain explicit resource owners.
This is a minimal priority/PBR composition, not full EgressRoutePolicy parity.

The network worker accepts read-only `POST /v1/routes/lookup` with target,
interface, optional source and mark. It uses Netlink with a bounded socket wait.
The health worker requests this evidence after probing. Lookup failure never
changes the probe outcome, and no external `ip` command is invoked.

Local checks:

```
go test -race ./internal/protocol/health ./internal/runtimeplan ./internal/network
go test -tags integration ./internal/protocol/health -run '^TestScoped(TCPAndHTTP|DNS)Probes?$' -v
```

On the isolated privileged Linux test VM, include raw ICMP:

```
go test -race -tags integration ./internal/protocol/health -run '^TestScoped' -count=1 -v
```

Required real-daemon gate on a Linux VM with user/network namespaces and
nftables (the host management namespace is never changed):

```
umask 022
make check-health-pbr
```

The target freshly builds the daemon binaries and a race-instrumented test.
It requires the named test and PASS marker, refuses skips, and preserves each
run separately under `dist/health-pbr-test/TIMESTAMP/`: executable hashes,
commit/dirty-tree metadata, typed status at each transition, Ethernet PCAPs
for both WANs and the test log. Both independent providers serve the same
application address; WAN-B also serves a decoy for WAN-A's probe. Actual LAN
TCP/UDP must follow primary, backup, recovery and confirmed rollback. Scoped
probe SYN packets must carry mark 42 in independent nft OUTPUT counters;
marks themselves do not appear on the wire. Foreign route/address/rule/nft
objects must survive health/control/network worker restarts and stale reports.

Fact expiry is a deliberate fault injection: a real accepted snapshot is
shortened through the actual control socket to 1.5 seconds, then allowed to
expire in CLOCK_BOOTTIME while its producer is stopped but still
supervisor-ready. Identical and resequenced replay must not renew the fact.
This does not claim a natural two-minute silence test. The fixture keeps main
table probe host routes as well as the marked table, so Linux reverse-path
validation can accept replies without depending on the application default.
Captures start after interface activation.

The same required test also runs as `daemon-health-pbr` in
`make qemu-protocols`, using a no-NIC guest. QEMU records capture assertions and
mark counters in its serial log; the local target additionally retains PCAPs.

Not implemented/qualified: targetSource/dynamic target binding, full legacy
HealthCheck import, automatic mark/hash policy
selection, source-family acquisition policy, and whole NET-01/NET-02 acceptance.
The unit tests use coordinator code with fake network/manager boundaries;
the separate integration gate establishes the minimal composition's actual
PBR forwarding. Full NET-01/NET-02 parity is still unqualified.
