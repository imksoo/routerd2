# Native single-hop BFD

This package implements asynchronous IPv4 and IPv6 BFD control sessions directly
on Linux UDP sockets. It needs no FRR, `bfdd`, `vtysh`, shell, systemd or journald.
It reports observations and finite health grants; consumers own routing policy.

Protocol references are [RFC 5880](https://www.rfc-editor.org/rfc/rfc5880.html)
and [RFC 5881](https://www.rfc-editor.org/rfc/rfc5881.html). Multihop is a distinct
application ([RFC 5883](https://www.rfc-editor.org/rfc/rfc5883.html)) and is not
silently inferred from incomplete single-hop configuration.

```go
err := bfd.Run(ctx, config, effects, ready)
```

## Configuration and ownership

An instance owns one interface and IP family, with one explicitly installed
local source address and 1–64 distinct peer addresses. IPv4 and IPv6 use separate
instances. Config and Snapshot are JSON serializable:

```json
{
  "resource": "uplink-health4",
  "interface": "uplink0",
  "source_address": "192.0.2.1",
  "peers": [
    {
      "address": "192.0.2.2",
      "min_tx_milliseconds": 50,
      "min_rx_milliseconds": 50,
      "detect_multiplier": 3
    }
  ],
  "effect_timeout_milliseconds": 1000
}
```

`Normalize` copies the peer slice and supplies 300ms TX/RX intervals, multiplier
3 and a 1000ms effect timeout. Configured intervals are 50–60000ms, multipliers
1–255, and effect timeouts 10–5000ms. `Validate` requires canonical addresses,
an explicit interface, compatible families and unique peers. The declared local
source must be installed, usable and non-tentative. Startup requires a direct
route for each peer on the bound interface; ordinary multiaccess peers must
belong to the source subnet. There is no implicit address selection or route
creation.

`point_to_point: true` permits exactly one peer. It accepts another valid unicast
source from that same interface/family, including during initialization, while
preserving the configured outbound destination. The observed source is reported
separately. An actual Linux point-to-point interface requires this explicit mode.
IPv6 link-local addresses omit the zone in configuration; the bound interface
supplies their scope.

An abstract Unix socket reserves the interface/family in its network namespace.
Additional abstract claims reserve nonzero cryptographically random local
discriminators across routerd BFD workers in that namespace. Each session has a
stable UDP source port in 49152–65535 and transmits to UDP 3784. UDP reuse-port
distribution is not used. The transport reads link/address/route information
through netlink and never changes it. A separate verifier checks source-prefix,
link identity, master and administrative state every second without blocking
packet processing. Loss of these facts stops the worker.

## Wire behavior and timing

The implementation has independent directional interval negotiation, the
Down/Init/Up state machine, remote restart handling, receive-time detection,
per-packet randomized jitter, and Poll/Final negotiation. Local desired TX is at
least one second outside Up. Remote detect multipliers, including 255 with the
largest wire interval, use checked 64-bit calculations. Expiry clears the remote
discriminator even while already Down. Remote RequiredMinRx zero suppresses
periodic packets while still allowing an immediate Final response to Poll.

Both IP families bind to the configured device and source; transmission uses
`MSG_DONTROUTE`. Receive validation checks actual kernel interface/destination,
TTL or hop limit 255, source-port range, peer scope, discriminator and the base
24-byte version-1 control format. P+F, zero multiplier, zero MyDiscriminator,
zero DesiredMinTx and invalid zero YourDiscriminator are rejected. UDP padding
outside the declared control length is ignored. A remote nonzero Echo RX value
is a capability advertisement and can be observed; local Echo RX remains zero
and no echo traffic is generated. The outgoing C bit is zero because this
userspace engine does not claim forwarding-plane independence.

Authentication, Demand, Echo, Multihop and Passive configuration are explicitly
rejected. Incoming Authentication, Demand and Multipoint flags are rejected and
cannot refresh health. These modes have no fallback to another daemon. The
worker does not claim interoperability tests with FRR or hardware vendors; the
native test uses a separately encoded UDP peer fixture.

## Finite health and publisher contract

The wire/timer loop never waits for routine `Effects.Update` calls. A single
publisher consumes a one-slot latest-status mailbox. Slow consumers coalesce
intermediate snapshots without unbounded memory growth. State, negotiated timing,
remote identity and Poll changes request immediate publication. Ordinary receive
refreshes are published at most every 200ms, reduced to one third of the current
detection interval, with a 5ms lower bound.

An initial active Down snapshot must be acknowledged before `ready`. Updates are
serialized and their callbacks **must honor the supplied context deadline**.
Publisher failure stops the worker. Shutdown cancels and joins the active
publisher before the final inactive AdminDown snapshot, then releases socket
ownership. A best-effort AdminDown packet is sent; shutdown does not wait out a
potentially very long remote detection interval. There is no durable Up state.

An Up snapshot is a finite grant with the following interpretation:

1. Validate the entire snapshot using `ValidateSnapshot(config, snapshot)`.
2. Fence the enclosing worker generation, configuration digest, producer epoch
   and sequence. Snapshot alone does not prove those identities.
3. Read the current kernel clock with `ReadClock()`. Use
   `Healthy(config, snapshot, exactPeerAddress, clock)` for the grant decision.
4. Expire the grant at `DeadlineBootNanoseconds` in the matching `BootID`, even
   if no new status arrives. `ValidUntil` and `UpdatedAt` are display fields and
   are insufficient for this decision.
5. Compare `DownTransitions` within the same producer epoch before accepting a
   refreshed Up. It retains an Up→Down→Up loss that a slow publisher coalesced.
   A changed or unknown producer epoch requires new positive evidence.

`LastReceivedBootNanoseconds` is a conservative lower bound on packet arrival.
Linux [`SO_TIMESTAMPNS_NEW`](https://docs.kernel.org/networking/timestamping.html)
supplies a realtime arrival stamp, which alone cannot
safely establish a monotonic deadline across wall-clock steps. The receiver also
records CLOCK_BOOTTIME immediately before a nonblocking read proves the queue
empty. It takes the earlier of the converted kernel timestamp and that last
empty-queue sample. Polling the empty queue every 5ms ordinarily bounds the early
expiry allowance to about 5ms plus scheduling delay, versus the minimum supported
50ms receive interval. This can cause an early failure under CPU overload; it
cannot extend a stale grant after suspension or a backward realtime adjustment.

If a flooded queue never becomes empty, this lower bound stops advancing and
eventually cannot renew health. A worker resuming with old queued packets must
drain them before fresh input can establish health. Snapshot polling, replay,
redisplay and slow Effects do not change the receive-anchored boot deadline.
The userspace scheduler is not a hard realtime service; choose intervals and
capacity appropriate to the deployment.

## Original routerd boundary

The source behavior audit is pinned to original routerd
[`1329ef54f296507eda79bbbc3b853783dd27251e`](https://github.com/imksoo/routerd/tree/1329ef54f296507eda79bbbc3b853783dd27251e):

- [`BFDSpec`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/api/specs.go)
  declares peer, interface, profile, minRx/minTx, multiplier and `when`.
- The [BFD controller](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/controller/bfd/controller.go)
  renders FRR configuration, applies it through `vtysh`, and reads peer status
  through `show bfd peers json`.
- Empty/fast profiles use 300ms, normal uses 500ms, and slow uses 1000ms with
  multiplier 5 instead of 3. An explicit old interval overrides the profile.
- An omitted old interface selects **multihop**, potentially deriving local
  identity from a referenced BGP router. That declaration cannot be accepted as
  this native single-hop module. A converter must reject it or request an
  explicit supported redesign.

This package does not resolve legacy references, evaluate resource `when`, reset
BGP sessions or gate static routes itself. The native
[BGP consumer](../bgp/README.md#native-bfd-session-gating) uses explicit service
references, finite source grants, actual TCP/admin acknowledgment and a separate
route withdrawal fence. Static-route gating and legacy graph conversion remain
separate work; configured semantics outside the implemented scope must be
rejected rather than dropped. This is not a claim of complete old BFD parity.

## Verification

Pure tests cover independent wire vectors, malformed/unsupported flags, exhaustive
state pairs, Poll/Final, negotiated interval extremes, deterministic jitter,
queued expiry, wall-clock steps, continuous queue load, snapshot tampering,
coalesced Down transitions, initial acknowledgment and cancellation fencing.
No ordinary unit test changes host networking.

```sh
go test -race ./internal/protocol/bfd
go test ./internal/protocol/bfd -run '^$' -fuzz '^FuzzControl$' -fuzztime=20s
ROUTERD2_INTEGRATION_REQUIRED=1 go test -tags integration ./internal/protocol/bfd \
  -run '^TestIsolatedNativeBFD$' -v
```

The integration parent only forks. Its child verifies a freshly created user/
network namespace before constructing a private veth link and peer namespace.
An independently encoded UDP peer checks actual IPv4/IPv6 packets, TTL, stable
source port, discriminator, handshake, Poll/Final, malformed/foreign traffic,
finite timeout, restart, point-to-point source changes, duplicate ownership,
source removal and final withdrawal. Namespace unavailability is a skip unless
the required-test environment variable is set; a skip is not packet evidence.

The integration binary also supports PID 1 in a disposable QEMU initramfs with
matching `veth` kernel modules. That branch bootstraps only the guest, requires
the namespace tests, prints `BFD_QEMU_TEST_PASS` or `BFD_QEMU_TEST_FAIL`, and
powers off. The runtime needs Linux
[CLOCK_BOOTTIME](https://docs.kernel.org/core-api/timekeeping.html) and
SO_TIMESTAMPNS_NEW support, device-bound UDP sockets and access to
read-only link/address/route netlink. No additional Go dependency is introduced.
