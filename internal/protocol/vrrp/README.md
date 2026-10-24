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

IPv6 is a separate instance with `family: ipv6`, a real link-local source and
a virtual link-local address first in `addresses`, followed by optional global
VIPs. Virtual addresses are host CIDRs, including their prefix lengths. An
explicit `address_owner: true` uses reserved priority 255 and activates without
the backup timer. Ordinary backup priorities are 1–254. IPv4 and IPv6 can reuse
the same VRID independently. An optional `vrf` requires both the election
interface's VRF association and the active virtual interface's VRF membership.

## Single-writer effects and liveness contract

`Effects.Update(ctx, Snapshot) error` is an acknowledgment of completed effects,
not a queue submission. `Snapshot` includes resource identity, process epoch,
transition number, active state, virtual MAC, virtual interface, host CIDRs,
accept policy, peer observation and counters.

For an active request, the single kernel writer must create or reconcile its
owned macvlan on the election interface, set the requested virtual MAC, bring
it up, assign every requested VIP, and enforce the accept policy. Set IPv6
address generation mode to `IN6_ADDR_GEN_MODE_NONE` before link-up and install
the explicit virtual link-local address. VIPs must have completed DAD, or the
writer must explicitly suppress DAD for these owned HA addresses. The protocol
worker checks link type, parent, MAC, UP state, optional VRF, and non-tentative
CIDRs through netlink before advertising ownership. It never changes them.

For backup or initialize requests, the writer must withdraw owned VIPs and
stop answering ARP/ND or forwarding frames for the virtual MAC. It must also
gate any RA producer tied to this virtual router. `accept_mode: false` requires
an input policy preventing local delivery to non-owner VIPs while retaining
necessary IPv6 ND; this package does not install that policy. Returning success
without these effects violates the module contract.

Every positive advertisement is preceded by an acknowledged lease renewal.
`ValidUntil` is three local advertisement intervals plus priority skew into the
future. The writer **must withdraw ownership when that lease expires**, using
the authenticated worker generation, config digest, producer epoch and
sequence to fence stale publishers. Convert the accepted bounded remaining
duration to a local monotonic deadline. A failed or expired acknowledgment, or
a missing/tentative VIP, prevents a positive advertisement and triggers
withdrawal. A normal shutdown sends priority zero before withdrawing.
Parent cancellation during effects, active verification, advertisement or
neighbor announcement is also a normal stop; independent action errors and
withdrawal failures remain errors. Cleanup uses a fresh bounded context.
Kernel
ownership cannot rely only on a process's graceful exit: SIGKILL cannot execute
cleanup, and expiry belongs to the surviving single writer.

Callbacks must honor their context deadlines. Extremely short intervals need
enough CPU and IPC headroom for the complete renewal/verification/send cycle;
failure to meet a lease stops ownership rather than advertising unverified
readiness. There is no claim of hitless config or worker restart.

## Native wire and timer behavior

Linux IPv4/IPv6 raw receive sockets join the VRRP group and provide interface
and hop-limit metadata. AF_PACKET transmission supplies the correct Ethernet
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
mapped onto this local multicast election.

VRRPv2, VRRPv2 authentication, legacy IPv4 pseudo-header checksums, unicast peer
extensions, preempt-delay extensions, packet fragmentation and IPv6 extension
header chains are not accepted. Use an Ethernet logical interface for a VLAN;
the worker does not interpret encapsulated VLAN tags itself. Importers must
reject unsupported declarations rather than drop them. VRRP provides local-LAN
election, not authenticated quorum or protection against an on-link attacker.

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
