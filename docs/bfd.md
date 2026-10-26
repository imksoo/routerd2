# Native BFD and BGP session ownership

`routerd-modules` has a native `bfd` role. It runs one worker per single-hop interface/family or multihop local address/routing domain,
with an explicitly configured local address and up to 64 peers.
It opens Linux UDP sockets directly; FRR, `vtysh`, systemd and external helpers
are not part of the runtime path. [The example](../examples/bfd-bgp.yaml) binds
an adjacent BGP peer to a BFD service.

## Protocol and configuration scope

The engine implements active asynchronous BFD control for IPv4
and IPv6. Single-hop follows [RFC 5880](https://www.rfc-editor.org/rfc/rfc5880) and
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

In single-hop mode, `source_address` must be a canonical unicast address explicitly committed on
the configured interface by an `address` resource. A worker does not choose a
DHCP/RA source implicitly. It checks continuing ownership of the interface and
source address. Separate IPv4/IPv6 workers may use one interface; two workers
of the same family on that interface are rejected. The runtime also claims
socket/discriminator ownership within its network namespace.

`point_to_point: true` permits the RFC's point-to-point source-address behavior
and requires exactly one configured peer. It is an explicit topology choice,
not a general wildcard for accepting arbitrary neighbors. Authentication,
demand mode, echo execution and passive mode are not implemented
and are rejected. A remote echo capability advertisement is not a request to
silently create an echo engine. See the [protocol implementation contract](../internal/protocol/bfd/README.md).

## Routed multihop

Set `multihop: true`, omit `interface` and `point_to_point`, and declare the
explicit local source address and peer routes. The source can live on `lo` while
packets leave a physical interface. See [the routed BFD/BGP example](../examples/bfd-multihop.yaml).
The source address must be committed and usable; its owning link is monitored.
There is no implicit source selection, route creation or fallback to single-hop.

[RFC 5883](https://www.rfc-editor.org/rfc/rfc5883.html) sessions use UDP 4784 and
normal source-bound routing. The kernel can change the egress path without
retargeting the session. Packets originate with TTL/Hop Limit 255 and can arrive
with a decremented value. The receiver checks the exact configured endpoint
pair and discriminator; multihop does not allow point-to-point source aliases or
link-local addresses. Two workers cannot own the same local multihop receive
socket within a routing domain. Single-hop UDP 3784 remains independently available.

Multihop receivers use `SO_REUSEADDR` so a default-domain socket and VRF-bound
sockets can bind the same address/control port. The existing abstract endpoint
claim still gives one routerd receiver per domain/address. Transmit source
ports remain exclusive; `SO_REUSEPORT` distribution is not used.

Authentication is not implemented in either mode. Other pending protocol and
resource work is listed in the implementation plan; this routed mode is not a
claim of full BFD resource parity.

## Explicit VRF scope

Set `vrf` to a declared `link` of kind `vrf`. Its committed table and interface
membership determine the source domain. Omitting `vrf` selects the default
domain; an address installed only in another VRF cannot supply that source.
An owned `dummy` link provides a per-VRF loopback source, separate from the
egress. See [the VRF example](../examples/bfd-vrf.yaml).

Multihop sockets bind to the VRF master using Linux `SO_BINDTODEVICE`; they
remain free to route through its members. Single-hop retains its explicit
member interface binding, direct route and TTL/Hop Limit 255. The worker checks
the source owner's master chain and the VRF's identity, table and administrative
state. Identical local/remote addresses in different VRFs have independent
receive sockets and session ownership.

A BFD-bound BGP multihop transport must bind the same VRF master and use its
table for learned routes. Single-hop BGP binds the same member interface as
BFD. Address routes used for recursive FIB resolution inherit the declared
VRF through the master chain, including addresses on the VRF device itself.
An excluded VRF declaration also excludes its dependent BFD service.

`make check-bfd` exercises IPv4 single-hop in a VRF and two independent routed
BFD/BGP peers with identical addresses in separate VRFs and the default domain,
for both IPv4 and IPv6. Distinct actual
TCP/UDP responses identify each path. UDP loss and worker replacement in one
VRF leave the other worker/session running. A reachable default-domain copy
of the destination cannot carry the withdrawn VRF's bound client traffic.
Freezing the default-domain BFD worker also expires its retained UP observation,
closes its BGP session and withdraws its learned route while the VRFs continue.
Captures inspect UDP 4784 and both routed TTLs on all three paths. This establishes the exercised
bound sockets and FIBs, not arbitrary forwarded VRF policy or physical NIC
qualification. Tests use the kernel's default l3mdev accept
settings; changing shared socket-accept policy requires separate qualification.

The IPv6 namespace topology explicitly coordinates usable link-local addresses
on each forwarding link with NoDAD. A loopback source is not an address of the
egress link; Linux needs a usable on-link source for its neighbor solicitations.
The first attempt with only tentative automatic link-local addresses produced
no SYN or neighbor solicitation and left the neighbor cache incomplete. This
matches [Linux 6.8's ND source selection](https://github.com/torvalds/linux/blob/v6.8/net/ipv6/ndisc.c#L612-L715).
The test does not qualify startup with automatic link-local DAD. Client
timeouts and protocol detection budgets were not increased.

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

## Current status in DerivedEvent

A `derived_event` can read the native BFD provider using its ResourceProgram
ID, including an ID containing `/`. For the service in the example above,
append this resource to observe the aggregate phase:

```yaml
  - id: edge-bfd-up
    derived_event:
      topic: routerd.operator.bfd
      inputs:
        - field: "${BFD/service/edge-bfd.status.phase}"
          equals: Up
      emitInitial: true
      hysteresis: 100ms
```

The supported top-level fields are `phase`, `peerStates`, `peers` and
`observedAt`, retaining the original BFD status field representations. Phase
is `Up`, `Down`, `Degraded` or `Pending`; nested selectors are unsupported.
The [DerivedEvent contract](resource-program.md#derivedevent) describes the
map/array text representations and the bounded current observations.

Current values require the active declaration, current ready worker identity
and a fresh supervisor observation. A local status observation expires after
five seconds. Up/Init is further capped by the original packet detection
deadline; redisplay, periodic publication and identical notification retries
cannot renew it. Expired raw Up becomes unknown without fabricating a Down
transition. Durable asserted/retracted outputs can feed an EventRule, but
stored output cannot restore current Up after worker/control recovery.

Peer transition counters prevent a coalesced Up→Down→Up from inheriting a
pending hysteresis hold. A peer change can conservatively restart a hold
even if the aggregate phase stayed unchanged. Saturated transition counters
cannot certify continuity and produce unknown. The BFD callback retains its
independent short cache lock; this consumer does not make it wait for network
apply. This is sampled current-state evaluation, not a lossless durable raw
BFD packet/transition producer or full legacy BFD resource parity.

For `observedAt` literal comparisons, a separate timestamp-change counter is
advanced before publication coalescing. This prevents a skipped mismatch and
return to the same timestamp from completing an old hold. The evaluator uses
this proof for `equals`, including `any_of`; presence-only `notEmpty` can span
ordinary periodic updates. Mixed presence/equality use of the same reference
selects the stricter proof conservatively. A saturated timestamp counter makes
that field unknown without invalidating the independent phase proof. Counter
rewinds and changed timestamps without counter advancement are rejected in
the same worker identity. The internal snapshot requires `timestamp_changes`;
unreleased worker/control formats are deployed as a matching runtime pair.

## Explicit BGP dependency

The public BGP setting is `peer_bfd`, keyed by the exact BGP neighbor address.
Its `service` names a configured BFD service. `address` defaults to that BGP
neighbor and must identify the same neighbor. The BGP transport must explicitly
use the same local address as BFD. Single-hop requires the same bound interface;
multihop uses routing on both transports with empty `bind_interface`. A dependency on an
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
each projected deadline intersects the original packet deadline and the
producer's original snapshot expiry. A failed poll does not replace valid wire
evidence with an IPC failure. If control remains unavailable, the unchanged
source deadline still closes the session. There is no receiver-created lease.
This is bounded local IPC polling, not an Internet probe. A BGP worker
can bind at most 64 peers to BFD in this version.

Forwarding proof is revoked before requesting the asynchronous GoBGP admin
operation. Its acknowledgment has a separate 2-second watchdog; this does not
renew the original packet or producer observation deadline.
Recovery waits for Down/Idle, accepted withdrawal, and a new packet after the
close. A missing or failed admin acknowledgment still terminates the worker.

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
Multihop BGP can use a multihop BFD source for that same endpoint pair. Enabled
global/peer graceful restart is rejected because it conflicts with immediate
withdrawal. Full original BFD resource
and BGP profile compatibility remains a separate migration item.

## Running the current communication checks

`make check-bfd` builds the native product and runs real workers in fresh user/network
namespaces. It needs Linux namespace capabilities and root or passwordless sudo.
The harness and product ELFs are staged in a private root-owned directory; home
permissions are unchanged. Logs and routed captures are written to `dist/bfd-test`.

The IPv4 routed test uses independent UDP BFD and TCP BGP peers behind a Linux
forwarder. It verifies initial Down suppresses BGP, loss closes TCP and withdraws
routes, recovery, actual user TCP/UDP over the learned route, original receive expiry during SIGSTOP, and surviving writer
expiry while control/BGP/BFD are stopped. The IPv6 test verifies loopback endpoint
sessions across a forwarder, loss/recovery and a fresh producer after SIGKILL.
Both capture tests check UDP 4784, the source-port range, outgoing TTL/Hop Limit
255 and incoming 254. A single-hop case checks the existing UDP 3784 path.

## Additional environments

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
[RFC 5882](https://www.rfc-editor.org/rfc/rfc5882). Authentication,
hardware offload, very large peer populations and behavior on homert02/03's
actual links require additional implementation or acceptance work.
