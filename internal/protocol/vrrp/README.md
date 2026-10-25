# Native VRRPv3 module

This package runs one independently supervised VRRP instance per address family,
election interface and VRID. It has no keepalived, systemd, shell, `ip`, `arping`
or `nft` dependency. Protocol behavior targets
[RFC 9568](https://www.rfc-editor.org/rfc/rfc9568.html).

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

## Single-writer effects and liveness contract

`Effects.Apply(ctx, Snapshot) (bool, error)` returns completed kernel application
and whether the VIP is actually owned. It does not acknowledge a queue submission. `Snapshot` includes resource identity, process epoch,
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
A graceful election may advertise while withholding VIP publication.
`BootID`, `ObservedNanos` and `ExpiresNanos` bind its original CLOCK_BOOTTIME
observation and deadline. The bounded lifetime is three local advertisement
intervals plus priority skew. `ValidUntil` is UTC display metadata only.
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
second receipt lease or rewrite an expired election snapshot. Track policy,
legacy version/authentication and cluster coordination remain
separate HA-01 work.

`make qemu-vrrp-daemon` exercises actual supervisor/control/network/VRRP workers,
VIP TCP/UDP traffic and capture, original deadline expiry/reapply, worker/control
replacement and old-generation refusal. `make qemu-vrrp-clock` repeats it with
fixed actual guest UTC corrections through the dedicated test PID 1 broker.
The native two-peer IPv4/IPv6 election/VMAC tests remain separate.

Callbacks must honor their context deadlines. Extremely short intervals need
enough CPU and IPC headroom for the complete renewal/verification/send cycle;
failure to meet a lease stops ownership rather than advertising unverified
readiness. There is no claim of hitless config or worker restart.

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
address-set agreement. IPv4 checksums cover the VRRP message alone; IPv6 includes
its pseudo-header. Incoming reserved bits are ignored. A different configured
interval is observable and accepted. State transitions implement priority skew,
master-down detection, optional nonpreemption, address-owner priority, priority
zero, primary-address tie breaking, and immediate active assertions.

The receive queue is bounded. Malformed-packet logging is rate limited, and
rejected/dropped/interval-mismatch counts are included in ownership snapshots.
Counters represent the latest published transition or renewal. Advertisements
are not sent by backups. Activation notifications contain the virtual MAC;
IPv6 unsolicited NAs set Router and Override while clearing Solicited.

## Scope and compatibility limits

This is a native election engine, **not full original `VirtualAddress` or
`RouterdCluster` parity**. Integration must preserve the original resource
dependency/`when` behavior, health tracking penalties, readiness gates,
additional failover MAC interfaces, associated DHCP/RA state and cloud actions.
`RouterdCluster` lease/peer policy is a different mechanism and is not silently
mapped onto this local VRRP election.

VRRPv2, VRRPv2 authentication, legacy IPv4 pseudo-header checksums and
preempt-delay extensions are not implemented. Use an Ethernet logical interface for a VLAN;
the worker does not interpret encapsulated VLAN tags itself. Importers must
reject unsupported declarations rather than drop them. VRRP provides local-LAN
election, not authenticated quorum or protection against an on-link attacker.

`TestDaemonVRRPUnicastIPv4` and `TestDaemonVRRPUnicastIPv6` use an independent
keepalived process with real supervisor/control/network/VRRP workers in isolated
Linux namespaces. They check two-way unicast capture, TTL/Hop Limit, VIP TCP/UDP,
higher-priority takeover, worker SIGKILL/rebind, peer stop and lower-priority
recovery. The keepalived peer explicitly uses `v3_checksum_as_v2` for the
[RFC 9568 IPv4 checksum](https://keepalived.org/documentation/keepalived-conf/).
No legacy checksum fallback is added to routerd2. VRF unicast and target hardware
have not been qualified by these plain Ethernet fixtures.
Run `make check-vrrp-unicast` on the isolated Linux VM with keepalived installed
and root or noninteractive sudo available. The runner builds as the caller and
stages execution in a root-owned 0700 temporary directory; it never changes home
permissions or host interface configuration. Current logs go to
`dist/vrrp-unicast/ipv4.log` and `ipv6.log`.

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
