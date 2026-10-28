# Native VRRP module

This package runs one independently supervised VRRP instance per address family,
election interface and VRID. It has no keepalived, systemd, shell, `ip`, `arping`
or `nft` dependency. VRRPv3 targets
[RFC 9568](https://www.rfc-editor.org/rfc/rfc9568.html); explicit IPv4 VRRPv2 targets
[RFC 3768](https://www.rfc-editor.org/rfc/rfc3768.html), with optional
[RFC 2338 PASS](https://www.rfc-editor.org/rfc/rfc2338.html#section-5.3.6.2).

```go
err := vrrp.Run(ctx, config, effects, ready)
```

`Config` is JSON serializable. `Validate` checks semantic boundaries and
`Normalize` supplies priority 100, preemption enabled, a one-second
advertisement interval, a two-second effect timeout, and a deterministic virtual
interface name when omitted. Example IPv4 instance:

```json
{
  "resource": "lan-gateway4",
  "version": 3,
  "interface": "lan0",
  "vip_interface": "vrrp-lan4",
  "family": "ipv4",
  "source_address": "192.0.2.2",
  "virtual_router_id": 42,
  "priority": 150,
  "advert_interval_centiseconds": 100,
  "addresses": ["192.0.2.1/24"],
  "accept_mode": true
}
```

Multicast IPv6 is a separate instance with `family: ipv6`, a real link-local source and
a virtual link-local address first in `addresses`, followed by optional global
VIPs. Virtual addresses are host CIDRs, including their prefix lengths. An
explicit `address_owner: true` uses reserved priority 255 and activates without
the backup timer. Ordinary backup priorities are 1–254. IPv4 and IPv6 can reuse
the same VRID independently. An optional `vrf` requires both the election
interface's VRF association and the active virtual interface's VRF membership.

Startup waits for a tentative IPv6 election source to finish normal kernel DAD.
Automatic multicast source selection also waits for kernel link-local creation.
The worker context and supervisor startup bound own this wait; it neither writes
addresses nor disables DAD. Active ownership checks still fail immediately when
the selected source becomes unusable.

For unicast, add a nonempty `unicast_peers` list and an explicit `source_address`.
For example, the IPv4 configuration above can use `"unicast_peers": ["192.0.2.3"]`.
Peers must be distinct same-family unicast addresses other than the local source;
at most 128 are accepted. An explicit empty list is an error, not a multicast
fallback. Omitting the field selects multicast. Unicast IPv6 also permits a
global source and peers; the virtual address list still starts with a link-local
VIP. Both modes require TTL/Hop Limit 255 and therefore adjacent peers.

Unicast advertises to every configured peer, accepts only those source addresses
and the actual local destination, and never joins or advertises to the multicast
group. Linux handles unicast routing and ARP/ND on a source/interface-bound raw
IP socket. Multicast advertisements and GARP/NA use Ethernet frames with the
virtual MAC. Unicast advertisements use the election interface's normal MAC;
VIP ownership, its VMAC and neighbor announcements retain the same writer ACK.

## IPv4 VRRPv3 checksum

`ipv4_checksum` selects the transmitted IPv4 VRRPv3 checksum. Omit it or use
`rfc9568` for the current message-only checksum. Use `rfc5798` for the IPv4
pseudo-header form required by default keepalived 2.2.8 peers. The pseudo-header
uses the actual source and destination, including for unicast. Configure the
transmit form understood by the other routers on that virtual router.

Reception accepts both forms regardless of the transmit choice. IPv6 retains
its required pseudo-header, and IPv4 VRRPv2 retains its message-only checksum.
This setting does not apply to IPv6 or v2.

## Wire version and PASS

`version` defaults to 3. Select `version: 2` explicitly for IPv4 peers using
VRRPv2. The advertisement interval remains expressed in centiseconds, but v2
accepts only whole seconds from 1 to 255 (`100..25500`). IPv6 requires v3.
A v2 receiver rejects advertisements whose interval differs from its configured
interval. V2 skew is `(256-priority)/256` seconds, independently of the advert
interval; v3 scales skew by the received advert interval. The original ownership
deadline uses the configured base priority and the selected version's skew.
Inferior nonzero advertisements are discarded by a v2 master; v3 immediately
asserts ownership. A normal shutdown sends priority zero in either format.
There is no version negotiation or automatic fallback.

For v2 PASS, set `auth_password` to 1–8 printable ASCII bytes. Omit it for no
authentication. Long passwords are rejected, not silently truncated. The sender
includes both authentication words, zero-pads short passwords and checksums the
entire v2 message. The receiver checks the authentication type and all eight
bytes when PASS is selected. Type zero ignores the two authentication words.
Passwords are configuration data and are never copied into election snapshots.

PASS is a cleartext legacy mechanism from RFC 2338, removed by RFC 3768. It is
provided for existing peers, not cryptographic authentication or replay
protection. IPv6/v3 PASS, AH and Keepalived's HMAC extension are not implemented;
unsupported declarations are rejected rather than downgraded. Secret-provider
resolution remains separate work. The original-resource importer supports
basic IPv4 v2/PASS and IPv6 v3 elections with explicit local/VMAC identities,
retained non-preempt defaults and zone ACLs. It also converts additional VMACs
and master/backup guards on HealthCheck/static addresses. Conditional readiness,
tracking and the complete homert graph remain open; see the
[import contract](../../legacyconfig/README.md#vrrp-election-and-vip-identity).

## Single-writer effects and liveness contract

`Effects.Apply(ctx, Snapshot) (Decision, error)` returns completed kernel application,
actual VIP ownership and finite resource tracking decisions. It does not acknowledge a queue submission. `Snapshot` includes resource identity, process epoch,
transition number, active state, virtual MAC, virtual interface, host CIDRs,
accept policy, peer observation and counters.

For an active request, the single kernel writer must create or reconcile its
owned macvlan on the election interface, set the requested virtual MAC, bring
it up, assign every requested VIP, and enforce the accept policy. Set IPv6
address generation mode to `IN6_ADDR_GEN_MODE_NONE` before link-up and install
the explicit virtual link-local address. VIPs must have completed DAD, or the
writer must explicitly suppress DAD for these owned HA addresses. For ordinary publication, the protocol
worker checks link type, parent, MAC, UP state, optional VRF, and non-tentative
CIDRs through netlink before advertising ownership. It never changes them.

For backup or initialize requests, the writer must withdraw owned VIPs and
stop answering ARP/ND or forwarding frames for the virtual MAC. It must also
gate any RA producer tied to this virtual router. `accept_mode: false` requires
an input policy preventing local delivery to non-owner VIPs while retaining
necessary IPv6 ND; this package does not install that policy. Returning success
without these effects violates the module contract.

Every advertisement of published ownership follows an acknowledged renewal.
Initial neighbor announcements retain the existing election timer: a slow but
still-valid ownership ACK must not postpone renewal beyond original authority.
A graceful election may advertise while withholding VIP publication.
`BootID`, `ObservedNanos` and `ExpiresNanos` bind its original CLOCK_BOOTTIME
observation and deadline. The bounded lifetime is three local advertisement
intervals plus skew calculated from the configured base priority. Tracking changes
the wire priority and election timers, but never extends this ownership deadline. `ValidUntil` is UTC display metadata only.
The writer **must withdraw ownership at that original boot deadline**, using
the current supervisor registry, authenticated worker generation, config
digest, producer epoch and sequence to fence stale publishers. The coordinator
passes the same absolute boot deadline to the address writer; UTC corrections,
transport replay and reapplying a projection cannot extend it. Both the first
publication after control restart and subsequent messages must match the live
supervisor's generation. A failed or expired acknowledgment, or
a missing/tentative VIP, prevents a positive advertisement and triggers
withdrawal. A normal shutdown sends priority zero before withdrawing.
Parent cancellation during effects, active verification, advertisement or
neighbor announcement is also a normal stop; independent action errors and
withdrawal failures remain errors. Cleanup uses a fresh bounded context.
Kernel
ownership cannot rely only on a process's graceful exit: SIGKILL cannot execute
cleanup, and expiry belongs to the surviving single writer.

Backup publishes a finite negative observation every second even when valid
peer advertisements keep resetting its election timer. It grants no ownership.
Initialize withdraws authority with a zero deadline and is terminal for that
producer generation. Rewound observations, changed epochs or a later Active
after Initialize are refused. A paused Active process whose lease already
expired must not renew or advertise under its old election transition.

The coordinator uses the original boot clock directly. It does not derive a
second receipt lease or rewrite an expired election snapshot. Cluster
coordination remains separate HA-01 work.

`make qemu-vrrp-daemon` exercises actual supervisor/control/network/VRRP workers,
VIP TCP/UDP traffic and capture, original deadline expiry/reapply, worker/control
replacement and old-generation refusal. `make qemu-vrrp-clock` repeats it with
fixed actual guest UTC corrections through the dedicated test PID 1 broker.
The native two-peer IPv4/IPv6 election/VMAC tests remain separate.

Callbacks must honor their context deadlines. Extremely short intervals need
enough CPU and IPC headroom for the complete renewal/verification/send cycle;
failure to meet a lease stops ownership rather than advertising unverified
readiness. There is no claim of hitless config or worker restart.

## Resource tracking

An instance can track up to 16 declared HealthCheck, BFD or BGPRouter resources.
The reference is the source resource ID, including any slash in that ID:

```json
"track": [{
  "resource": "service/wan-probe",
  "penalty": 100,
  "healthy_threshold": 2,
  "unhealthy_threshold": 3,
  "max_age_millis": 1500
}]
```

References are resolved against the whole declaration at compilation. Tracking
is a soft observation dependency: a conditionally excluded provider can leave
the projected worker set while its election continues. Missing declarations and
unsupported status providers are rejected before any worker or kernel changes.
The discarded `service` reference and `known` decision field are not accepted.

Health tracking counts original passed/failed probes independently of the
Health service's own phase thresholds; raw consecutive counters retain progress
through coalesced delivery within the current streak. BFD is healthy when its
current phase is Up; BGP requires Established with matching applied FIB proof.
Their confirmations count distinct original status observation times. Repeated
queries and replayed observations do not add confirmations. Producer replacement,
BFD transition continuity changes and expiry require fresh confirmation.

Defaults are penalty 50, two healthy and three unhealthy observations.
`max_age_millis` is required and bounds original observation age. Each decision
also retains its source's earlier expiry and current supervisor authority.
Priority is the configured base minus current penalties, with a floor of 1;
priority zero is reserved for withdrawal. Address owners cannot configure tracking.

An explicitly false `when` on the tracked resource produces `neutral`, preserving
its previous penalty and confirmation counts. Neutral carries the condition's
original finite deadline. An indeterminate/expired condition, missing observation,
stopped or unregistered producer produces `unknown` and imposes the penalty.
A new configuration resets tracking history. Other resource kinds and their
Disabled/NotApplicable/Standby phase contracts remain unimplemented.

Decisions return through the existing ownership application response. `track`
reports resource, state (`healthy`, `unhealthy`, `neutral`, `unknown`), counts,
penalty and expiry. The worker checks expiry with its own BOOTTIME clock before
advertising, including for neutral decisions, and adjusts Backup's down timer
when priority changes. Backup polls at 250ms when tracking is configured.
Tracking does not rewrite operator intent or restart the election worker.

## Additional WAN/LAN identities

Additional identities use ordinary conditional `link`, `interface` and `address`
resources, with no second VRRP instance or failover manager. See
[`examples/vrrp-additional-identity.yaml`](../../../examples/vrrp-additional-identity.yaml).
Gate the macvlan on `service.ha.elected` to prepare a fixed MAC and link-local
address before publishing the gateway VIP. Inferred link dependencies carry this
condition to its interface, addresses and consumers. Gate RA on
`service.ha.active` so advertisements start only after VIP application.
A graceful VRRP instance can wait for a dependent WAN protocol's readiness before
publishing its VIP. Ordinary dependent stop ordering withdraws RA before deleting
the identity.

The network writer receives the original condition/dependency boot deadline and
uses it to fence the link DOWN and revoke its managed addresses independently
of control. Main VRRP VMACs carry the same finite link authority. Address budgets
are intersected with the link budget; an old projection cannot restore UP.
Current finite MACs reassert their L2 location with RARP once per second. This
avoids relying on unrelated traffic to repair switch learning after handover.
No IP lease or election lifetime is extended by that announcement.

`TestDaemonVRRPAdditionalIdentity` uses an independent WAN client and Keepalived,
a shared fixed WAN MAC/link-local address, real WAN TCP/UDP, RA withdrawal and
packet capture. It pauses both control and VRRP while the registered network
worker remains alive, verifies DOWN and managed-address removal, then requires
a fresh election and traffic recovery. The peer uses `garp_extra_if all 1` for
its excluded WAN VIPs. The fixture permits up to two seconds for bridge learning
before testing steady-state traffic; it does not establish lossless handover.
Its peer WAN macvlan is precreated, with addresses owned by Keepalived. Symmetric
two-native-node DHCP/PD/NAT session continuity and physical NIC qualification
remain open.

## Native wire and timer behavior

Linux IPv4/IPv6 raw receive sockets provide the actual source, destination,
interface and hop-limit metadata directly. Multicast instances join the VRRP
group. There is no synthetic Ethernet header or reconstructed IPv6 packet in
the receive path. Linux owns IP validation, extension processing and reassembly;
this package validates the complete VRRP message and received IP metadata.
AF_PACKET transmission supplies the correct Ethernet
source MAC, including for gratuitous ARP and unsolicited neighbor advertisements.
Receiving never substitutes a configured TTL for the value received from the
kernel. An abstract Unix socket reserves each interface/family/VRID tuple within
its network namespace and disappears when its process dies.

The implementation checks version, advertisement type, VRID, complete address
list, checksum, receiving interface and TTL/hop limit 255. It also checks virtual
address-set agreement. IPv4 VRRPv3 accepts both established checksum forms; IPv6
includes its pseudo-header. Incoming VRRPv3 reserved bits are ignored. A different configured
interval is observable and accepted. State transitions implement priority skew,
master-down detection, optional nonpreemption, address-owner priority, priority
zero, primary-address tie breaking, and immediate active assertions.

The receive queue is bounded. Malformed-packet logging is rate limited, and
rejected/dropped/interval-mismatch counts are included in ownership snapshots.
Counters represent the latest published transition or renewal. Advertisements
are not sent by backups. Activation notifications contain the virtual MAC;
IPv6 unsolicited NAs set Router and Override while clearing Solicited.

## Scope and compatibility limits

`service.NAME.backup` is a finite fact for an observed BACKUP state. It is
indeterminate when the producer is absent, initialized or expired, and is not
the negation of `elected`. It shares the original observation deadline, boot,
generation and producer epoch. Configuration contract native-45 includes this
fact. The legacy role importer uses `elected` for master so preparation does not
wait for the VIP it is preparing.

This is a native election engine, **not full original `VirtualAddress` or
`RouterdCluster` parity**. Integration must preserve the original resource
dependency/`when` behavior, health tracking penalties, readiness gates,
additional failover MAC interfaces, associated DHCP/RA state and cloud actions.
`RouterdCluster` lease/peer policy is a different mechanism and is not silently
mapped onto this local VRRP election.

Preempt-delay extensions are not implemented. Use an Ethernet logical interface for a VLAN;
the worker does not interpret encapsulated VLAN tags itself. Importers must
reject unsupported declarations rather than drop them. VRRP provides local-LAN
election, not authenticated quorum or protection against an on-link attacker.

`TestDaemonVRRPUnicastIPv4` and `TestDaemonVRRPUnicastIPv6` use an independent
keepalived process with real supervisor/control/network/VRRP workers in isolated
Linux namespaces. They check two-way unicast capture, TTL/Hop Limit, VIP TCP/UDP,
higher-priority takeover, worker SIGKILL/rebind, peer stop and lower-priority
recovery. The ordinary IPv4 case uses default keepalived checksum settings and native
`ipv4_checksum: rfc5798`. `TestDaemonVRRPRFC9568IPv4` separately uses native
`rfc9568` and keepalived `v3_checksum_as_v2`; see the
[keepalived checksum settings](https://keepalived.org/documentation/keepalived-conf/).
Both pass actual VIP TCP/UDP, takeover and process replacement/recovery. VRF unicast and target hardware
have not been qualified by these plain Ethernet fixtures.
`TestDaemonVRRPHealthTracking` also exercises real Health failure, original sample
expiry during SIGSTOP, Health worker replacement and recovery against keepalived
priority 120, with native priorities 150/50 and TCP/UDP on both owners.
`TestDaemonVRRPv2` exercises IPv4 multicast without authentication and
`TestDaemonVRRPv2PASS` exercises unicast PASS against Keepalived. The PASS fixture
also verifies that a wrong-password peer cannot make the native owner withdraw.
The v2 peer explicitly uses `lower_prio_no_advert`, matching RFC 3768 discard
behavior. Keepalived 2.2.8 otherwise resets its send timer on rejected packets
([receive dispatcher](https://github.com/acassen/keepalived/blob/v2.2.8/keepalived/vrrp/vrrp_scheduler.c#L1075)),
which can postpone the wrong-password peer's advertisements.
Both check takeover, process replacement, peer stop, lower-priority recovery,
VIP TCP/UDP and actual version/interval/authentication fields in capture.
The election-source addresses in these fixtures use normal DAD; first startup
must remain in generation 1. The owned virtual HA addresses separately use the
writer's deliberate NoDAD policy. A traffic probe does not query control, so it
also works during the test's intentional control SIGSTOP.

Run `make check-vrrp-unicast` on the isolated Linux VM with keepalived installed
and root or noninteractive sudo available. The IPv6 unicast and v2/PASS cases
boot the actual CLI-migrated resource graph, including its owned firewall.
They verify a real listener on an undeclared VIP port is denied while declared
TCP/UDP ports work. This qualifies basic import, not conditional HA/cluster input.
The runner builds as the caller and stages execution in a root-owned 0700 temporary directory; it never changes home
permissions or host interface configuration. Current logs go to
`dist/vrrp-unicast/ipv4.log`, `ipv6.log`, `health-track.log`, `v2.log` and
`v2-pass.log`, `additional-identity.log` and `ipv4-rfc9568.log`. The runner also
saves actual interface captures beside each log. Use its `--test` argument
for an individual case while developing.

## Verification

Unit tests use independently calculated packet fixtures, malformed/truncated
packets, exact deterministic clock transitions, priority-zero and tie behavior,
acknowledgment ordering, expired activation and lease renewal. No unit test
configures the host network.

The native integration fixture re-executes into a fresh user and network
namespace, proves that it differs from the parent, and then creates two peer
namespaces and a private bridge. It checks duplicate instance rejection,
priority election and stable backup reception over real raw sockets, forced
advertisement loss including loss of priority zero, takeover, actual VMAC/VIP
presence, and final withdrawal for IPv4 and IPv6.

```sh
CGO_ENABLED=0 go test -tags integration -c -o /tmp/routerd2-vrrp.test ./internal/protocol/vrrp
sudo env ROUTERD2_INTEGRATION_REQUIRED=1 /tmp/routerd2-vrrp.test -test.v -test.timeout=45s
```

Without `ROUTERD2_INTEGRATION_REQUIRED=1`, unavailable namespace infrastructure
is reported as a skip, never a passing packet test. The integration binary can
also be installed as `/init` in a disposable QEMU initramfs containing the
matching kernel modules. Its PID 1 `TestMain` mounts the basic guest filesystems,
loads `veth`, `bridge` and `macvlan`, requires the native tests, reports
`VRRP_QEMU_TEST_PASS` or `VRRP_QEMU_TEST_FAIL`, and powers off the guest. That
branch is not used by ordinary test execution.

## Graceful publication admission

`graceful_activation` is opt-in. Its `ready_when` uses the same bounded finite
fact expression grammar as resource conditions; `timeout` defaults to `45s`.
The resource's own `ready_when` still gates dependents rather than its own start.

An elected MASTER can report `State: ACTIVE` and `Active: false`. It keeps its
VRRP election advertisements but owns no VIP/VMAC and sends no VIP GARP/NA.
`ActivationState` reports `Preparing` or `ReadinessTimeout`; timeout never forces
publication and the same live election can retry when readiness recovers.

Each election update uses the same application request. The coordinator checks
finite `ready_when` evidence immediately before starting a new kernel activation.
It installs the VIP with the election's original BOOTTIME deadline and replies
with the ownership actually applied. There is no grant exchange, provisional VIP
or lifetime promotion. Neighbor announcements follow the application reply and
native netlink verification. Linux address lifetimes retain their integer-second
rounding resolution.

Readiness is consumed when that kernel operation begins. If the operation
succeeds, later readiness loss—including loss during that I/O—does not withdraw
the election. A failed initial activation creates no admission; retry needs current
readiness. The coordinator retains only the successfully applied election key.
Role loss, conditional exclusion, worker replacement and original finite expiry
still fence ownership. A stopped or expired election cannot renew under its
former transition. `service.NAME.elected` is a finite role fact;
`service.NAME.active` reflects the coordinator's applied ownership, including
when the worker's last submitted observation was still pending. Missing or
expired producer evidence is unknown.

This is not the complete legacy VirtualAddress/cluster/track/VMAC contract.
General legacy status references, cluster fencing and all protocol/hardware
qualification remain separate acceptance requirements.
