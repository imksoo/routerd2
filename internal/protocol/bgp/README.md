# Native BGP module

This worker embeds GoBGP v4.7.0 as a Go library. It does not execute `gobgp`,
`gobgpd`, a shell, `ip`, systemd or journald. Linux remains the packet forwarding
stack. BGP owns sessions, route selection, import/export policy and the local
RIB; the routerd control plane owns all kernel FIB changes.

The policy and SAM mobility path implementation was adapted from
[`imksoo/routerd` at `1329ef54f296507eda79bbbc3b853783dd27251e`](https://github.com/imksoo/routerd/tree/1329ef54f296507eda79bbbc3b853783dd27251e/cmd/routerd-bgp).
The upstream BSD 3-Clause notice is retained in `NOTICE`. GoBGP is a pinned
Apache-2.0 dependency; the distribution's dependency notice collector includes
its original license. The library API reference is the pinned upstream source
and [library documentation](https://github.com/osrg/gobgp/blob/v4.7.0/docs/sources/lib.md).

## Integration

```go
err := bgp.Run(ctx, cfg, bgp.EffectsFunc(func(ctx context.Context, s bgp.Snapshot) error {
    return writer.ReplaceBGP(ctx, s)
}), ready)
```

`internal/protocol/bgpconfig` contains Config, validation and the pure
Snapshot/Route/Peer/Event wire types. The worker aliases these types. PID 1 and
the control plane must import `bgpconfig`, not `bgp`: the former does not link
GoBGP or gRPC. The configured resource is resolved intent, not an interpreter
for the original resource graph. The compiler resolves dependencies, dynamic
admission, addresses, secrets and convergence defaults before constructing it.

An instance uses one worker process in the intended network namespace. GoBGP
has process-global route selection options; a second simultaneous `Run` in the
same process is rejected. Global `BindInterface` scopes BGP listeners and is
the default peer socket binding; it may be a Linux VRF master. Per-peer
`BindInterface` overrides it. Namespace entry and VRF creation belong to the
supervisor and single kernel writer.

Example resolved config:

```json
{
  "resource": "BGPRouter/core",
  "runtime": {
    "version": 1,
    "global": {
      "asn": 64512,
      "routerID": "192.0.2.1",
      "listenPort": 179,
      "listenAddresses": ["192.0.2.1"],
      "families": ["ipv4-unicast", "ipv6-unicast"],
      "useMultiplePaths": true
    },
    "peers": {
      "192.0.2.2": {
        "address": "192.0.2.2",
        "asn": 64513,
        "timersProfile": "fast",
        "importPolicy": {
          "allowedPrefixes": ["198.51.100.0/24", "2001:db8:42::/48"],
          "nextHopRewrite": "unchanged"
        }
      }
    }
  },
  "peer_transports": {
    "192.0.2.2": {"local_address": "192.0.2.1", "fib_interface": "wan0"}
  },
  "socket_path": "/run/routerd/services/bgp-core/control.sock",
  "grpc_socket_path": "/run/routerd/services/bgp-core/gobgp.sock",
  "state_path": "/var/lib/routerd/bgp-core/applied.json",
  "status_path": "/run/routerd/services/bgp-core/status.json",
  "event_path": "/var/lib/routerd/bgp-core/events.jsonl",
  "table": 254,
  "route_metric": 200,
  "max_routes": 4096
}
```

`listenPort: -1` disables inbound BGP listening; `0` defaults to 179.
`RemotePort` permits explicitly configured peer ports. An omitted family list
enables IPv4 and IPv6 unicast. Initial originated routes may use `Paths` or the
legacy `Advertisements` view. IPv6 origination uses MP_REACH_NLRI. Supply an
appropriate explicit next hop when the transport cannot supply an address of
the originated route's family, for example IPv6 NLRI on an IPv4 transport.

## FIB ownership and readiness

The worker publishes a complete learned-route snapshot after startup, on
coalesced native best-path/peer events, and every second. Locally originated
paths, non-best paths, withdrawals, filtered paths and invalid next hops do not
become learned FIB routes. `RejectedPaths` counts unusable selected learned
paths. Each accepted publication has a random process epoch, increasing
sequence, observation time and a five-second `ValidUntil` lease.

An Effects success means the writer accepted the snapshot under its ownership
and generation rules. The writer must fence retired worker epochs and withdraw
owned routes when an active lease expires, even after SIGKILL. The root service
envelope adds committed configuration digest and supervisor generation. A
shutdown snapshot is inactive, has no routes, and clears its lease. The module
does not claim a hitless worker/control restart or implement kernel rollback.

`Route` contains prefix, family, next hop, interface, peer address, table, metric
and stale flag. Interface is the per-peer `FIBInterface`, then the global
`FIBInterface`; socket `BindInterface` is deliberately distinct. Missing egress
interfaces and recursive next hops must be resolved by the kernel writer.
Unresolved paths can remain in the input snapshot with an explicit route issue
in control status; accepting an input snapshot must not imply that every
unresolved route reached the kernel. Link-local next hops need an interface.

Multiple selected entries for one prefix form an ECMP set. The writer must
compose a Linux multipath route or report the unsupported set; successive
single-path RouteReplace operations would silently lose members. Stale paths
are retained only while GoBGP considers them eligible under negotiated graceful
restart. Readiness requires successful config restore, private APIs and the
first Effects acknowledgment; it does not require all peers to be established.

The standalone worker defaults `MaxRoutes` to 100,000 and allows an explicit
limit up to 1,000,000. This bounds exported selected paths, not GoBGP's entire
Adj-RIB-In. Exceeding it terminates the worker and withdraws ownership instead
of publishing a partial FIB. The bundled service transport currently has a
2 MiB IPC bound and therefore caps its public limit at 4,096; larger deployments
need a suitable snapshot transport or a future streaming protocol. Full route
walks are currently performed once per second and on coalesced events; Internet
full-table scale and convergence throughput have not been benchmarked.

## Private APIs and state

HTTP and optional GoBGP gRPC APIs use Unix sockets, mode 0600, in a worker-owned
0700 parent directory. Existing shared directories are not chmodded. A live
socket or non-socket entry is never removed to claim a path. The private
directory protects the interval before GoBGP's own listener is chmodded. A
gRPC socket becomes ready only after a successful local GetBgp call; there is
no TCP management listener.

HTTP endpoints retain the original schemas:

| Endpoint | Behavior |
| --- | --- |
| `GET /v1/applied` | Applied journal, including privileged peer configuration |
| `PUT /v1/applied` | Validate and durably replace the restart journal; live policy changes remain the controller's gRPC transaction |
| `GET /v1/paths?source=...` | Source-scoped locally originated paths |
| `POST /v1/paths` | Upsert a validated MobilityPool IPv4 /32 path and refresh applicable policy |
| `DELETE /v1/paths?source=...&prefix=...` | Durably fence withdrawal before deleting the live path |
| `GET /v1/observed-paths` | Original read-only IPv4 live global RIB view used by SAM; distinct from the originated-path journal |
| `GET /v1/status` | Latest acknowledged typed IPv4/IPv6 snapshot, without passwords |
| `GET /v1/events?epoch=...&after=...` | Last 128 typed summaries with an explicit cursor gap flag |

The gRPC endpoint retains GoBGP's policy/peer/path APIs for existing privileged
controllers. These are live operations: they do not automatically change the
restart journal. Applied state and path HTTP transactions share one mutex.
Bodies accept one strict JSON document, bounded to 2 MiB; HTTP timeouts and
headers are bounded. The applied API intentionally contains secrets for the
privileged controller; never expose it through an unredacted user status view.

The applied journal is an atomic, fsynced 0600 file, protected by a process
flock. Its same-file `_routerd2` metadata records resource identity and the
normalized committed intent digest. Dynamic paths are restored only for that
same intent; a changed committed intent replaces the old restart set. Restored
native paths always receive fresh GoBGP UUIDs. Direct-peer addition/removal
fences, pending import reset, narrow direct-peer prefix boundaries and original
static-path withdrawal metadata are preserved.

Dynamic replacement/deletion removes the obsolete path from the durable
restart set before calling live DeletePath. A failure after a live mutation
requires worker restart; returning only HTTP 500 could leave an unjournaled
advertisement alive. Import policies are re-evaluated with inbound soft resets
for regular peers, including when the last dynamic prefix is removed. Direct
peers retain the separately fenced policy. Export soft resets remain in place.
Mixed IPv4/IPv6 allowlists use separate family-specific prefix sets, and IPv6
neighbor scope names include a hash to prevent compressed-address collisions.

Optional status JSON is replaced atomically at mode 0600. Optional event NDJSON
rotates at 1 MiB, retaining one archive; events are observation summaries, not a
durable protocol command log. Secrets are absent from status and event records.

## Lifecycle and supported scope

Cancellation stops control mutations, publishes withdrawal, stops GoBGP and
closes sockets. HTTP draining, Effects calls and GoBGP shutdown have bounds.
The outer cancellation guard returns after 16 seconds even if an upstream
management call hangs. On that exceptional error the supervisor must terminate
the worker process; do not reuse a GoBGP instance with surviving goroutines.
Effects implementations must honor their context. No process-global signals
or environment defaults are installed by this package.

The worker supports resolved static peers, passive mode, TCP MD5, eBGP
multihop, iBGP route-reflector clients, the original timer profiles, resolved
graceful restart options, prefix/community/next-hop/local-preference policy,
local static origination and SAM mobility path APIs. It is not by itself full
BGPRouter/BGPPeer/BGPDynamicPeer resource-controller parity. Dynamic admission,
redistribution discovery, topology/when/health integration and migration from
the original resource graph belong to the compiler/controllers. BFD profile
references are rejected pending an owned native BFD integration; EVPN/VPN/FlowSpec
and other family intent is rejected rather than ignored. Convergence-profile
defaults must arrive as resolved timer and graceful-restart settings.

## Verification

```sh
go test -race ./internal/protocol/bgpconfig ./internal/protocol/bgp
```

Tests preserve the original policy/SAM regression suite and exercise private
HTTP/gRPC APIs, readiness acknowledgment, shutdown withdrawal, native restart
with fresh UUIDs, committed-intent fencing, durable-before-live withdrawal,
configuration validation and collision cases. An independent BGP wire peer
uses real loopback TCP high ports to negotiate IPv4/IPv6, establish a session,
advertise both families, verify import rejection and withdraw both routes.
Both mixed-family import and export policies are applied to real GoBGP. These
tests do not modify host interfaces, routes or firewall state. They do not
substitute for the parent writer's namespace/QEMU FIB and ECMP tests, appliance
boot tests or performance measurements.
