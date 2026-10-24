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

Not implemented/qualified: targetSource/dynamic target binding, full legacy
HealthCheck import, kernel packet capture of two WANs, automatic mark/hash policy
selection, source-family acquisition policy, and whole NET-01/NET-02 acceptance.
The current runtime tests use real coordinator code with fake network/manager
boundaries; they do not establish live PBR packet forwarding.
