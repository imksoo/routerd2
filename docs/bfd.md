# Native BFD and BGP session ownership

`routerd-modules` has a native `bfd` role. It runs one worker per interface and
address family, with an explicitly configured local address and up to 64 peers.
It opens Linux UDP sockets directly; FRR, `vtysh`, systemd and external helpers
are not part of the runtime path. [The example](../examples/bfd-bgp.yaml) binds
an adjacent BGP peer to a BFD service.

## Protocol and configuration scope

The engine implements active, single-hop, asynchronous BFD control for IPv4
and IPv6, following [RFC 5880](https://www.rfc-editor.org/rfc/rfc5880) and
[RFC 5881](https://www.rfc-editor.org/rfc/rfc5881). Each session has a stable
local discriminator and UDP source port in 49152..65535. Reception uses port
3784, interface/destination checks and TTL/Hop Limit 255. It validates the
version, length, discriminator, flags and timing fields before changing a
session. Poll/Final negotiation, randomized transmission intervals, silent
expiry and remote restart are covered by the protocol tests.

`min_tx_milliseconds` and `min_rx_milliseconds` accept 50..60000, with defaults
of 300. `detect_multiplier` accepts 1..255 and defaults to 3. The negotiated
detection interval depends on the remote transmission interval and multiplier;
the local values alone do not guarantee a fixed failure time. Down/Init
transmission is no faster than once per second before jitter. Linux scheduling
and control-plane load still matter; this is not a real-time convergence bound.

`source_address` must be a canonical unicast address explicitly committed on
the configured interface in `network.interfaces`. A worker does not choose a
DHCP/RA source implicitly. It checks continuing ownership of the interface and
source address. Separate IPv4/IPv6 workers may use one interface; two workers
of the same family on that interface are rejected. The runtime also claims
socket/discriminator ownership within its network namespace.

`point_to_point: true` permits the RFC's point-to-point source-address behavior
and requires exactly one configured peer. It is an explicit topology choice,
not a general wildcard for accepting arbitrary neighbors. Authentication,
demand mode, echo execution, passive mode and multihop BFD are not implemented
and are rejected. A remote echo capability advertisement is not a request to
silently create an echo engine. See the [protocol implementation contract](../internal/protocol/bfd/README.md).

## Receive time, publication and restarts

Health is anchored to the packet receive observation and `CLOCK_BOOTTIME`,
scoped by Linux's boot ID. Wall-clock timestamps are for display. The monotonic
clock includes suspend time; copying an old snapshot or correcting UTC cannot
renew the original detection deadline. Timestamp conversion is conservative
when packets were queued before scheduling delays or a wall-clock change.

The socket loop does not wait for a slow controller request. A bounded,
coalescing publisher retains transition counters, including `down_transitions`,
so a short Up→Down→Up sequence is not erased by a later Up snapshot. Initial
status must be acknowledged before the worker reports ready. Shutdown joins
the active publisher and sends an inactive final fence. A finalized producer
cannot become active again in the same generation.

The controller accepts only the current immutable service digest, process
generation, producer epoch and increasing IPC sequence. It also rejects
rewound peer identities/counters, future receive observations and a receive
anchor advanced without another packet. BFD observation acknowledgments do
not run or fsync the complete kernel plan. BGP is an explicit consumer; BFD
does not install routes itself.

`show services edge-bfd` includes both the last protocol snapshot and a
calculated `bfd_health` section. A last observed `UP` state may remain visible
after a suspended worker has missed its deadline; `healthy_peers` reflects
current validity rather than the old display state.

## Explicit BGP dependency

The public BGP setting is `peer_bfd`, keyed by the exact BGP neighbor address.
Its `service` names a configured BFD service. `address` defaults to that BGP
neighbor and must identify the same neighbor. The BGP transport must explicitly
use the same local address and bound interface as BFD. A dependency on an
unrelated monitored router, a different family, or an implicit transport path
is rejected before applying a configuration.

BFD-bound BGP neighbors start administratively disabled. They can establish a
session only after the consumer has a current, finite BFD health grant. A Down,
expired receive deadline, source identity change or new Down counter disables
the actual GoBGP session. The consumer waits for administrative Down and Idle
before it considers enabling that session again. This closes the asynchronous
Disable/Enable race in GoBGP's management API.

The worker consumes a typed projection scoped to its configuration digest and
process generation. A separate loop observes the original BOOTTIME deadlines
while a control request is slow. Polling is currently every 20 milliseconds;
loss of successful control observations for 500 milliseconds also removes the
grant. This is bounded local IPC polling, not an Internet probe. A BGP worker
can bind at most 64 peers to BFD in this version.

The BGP route snapshot records which BFD source identity and Down counter the
session gate actually acknowledged. The controller requires a matching proof
and live BFD receive deadline before projecting a learned route. A coalesced
Up→Down→Up observation cannot reintroduce a pre-failure route while the BGP
session reset is still pending. Kernel withdrawal remains subject to the
network writer and reconciliation schedule; the implementation does not claim
sub-millisecond FIB replacement.

This behavior is a configured strict dependency, including startup, rather
than opportunistic detection after an already-established BGP session. The
original `AppliedPeer.BFD` profile string is still rejected: it is a different
legacy model and is not silently reinterpreted as a native service reference.
Multihop BGP and enabled global/peer graceful restart cannot be combined with
this single-hop gate in the current implementation. Full original BFD resource
and BGP profile compatibility remains a separate migration item.

## Acceptance evidence

The native BFD suite uses an independently encoded UDP peer inside a private
network namespace. It covers both families, negotiation, malformed/off-scope
packet rejection, silent expiry, remote restart, point-to-point source behavior,
source/interface loss and the final inactive fence. The `bfd` QEMU suite runs
that test on the packaged Alpine kernel with no external network interface.

The `daemon-bfd` suite uses the real `routerd` and `routerd-modules` ELF pair,
an independent BFD peer and a TCP BGP speaker. It checks startup suppression,
healthy establishment and route export/import, UDP-only loss while BGP
keepalives continue, recovery, and expiry while the BFD worker is suspended.
The suite must pass explicitly; unavailable isolation and skipped tests are
not successful gates. Exact results belong to the corresponding CI run and
its serial log/manifest, not merely the presence of this test definition.

The application boundary follows the considerations in
[RFC 5882](https://www.rfc-editor.org/rfc/rfc5882). Authentication, multihop,
hardware offload, very large peer populations and behavior on homert02/03's
actual links require additional implementation or acceptance work.
