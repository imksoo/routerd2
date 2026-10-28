# Native DHCPv6 client

One worker owns UDP 546 on one interface. `request_address` requests IA_NA,
`request_prefix` requests IA_PD, and both can be enabled in the same exchange.
With neither enabled and no RA gating, the client performs Information-request
only. DUID and IAID belong to this client; additional workers cannot own separate IA types on
the same interface. The network writer installs learned IA_NA addresses as /128
and derives configured LAN addresses and advertisements from IA_PD prefixes.
The client does not infer a default router from DHCPv6.

The optional `ra_from` names a native RA client on the same interface. With
this explicit policy, IA_NA waits for M, and information-only acquisition waits
for M or O ([RFC 4861 section 4.2](https://www.rfc-editor.org/rfc/rfc4861.html#section-4.2)).
O can start Information-request before M adds IA_NA in the same worker, socket,
DUID and IAID. A configured IA_PD starts explicitly, independently of these
flags. Current flags are projected from the RA owner; the DHCP owner persists
its latched start decisions as `ra_started`. Later clear flags do not revoke a
grant or erase those decisions. Worker and operator restarts on the same
boot/interface/IAID/DUID retain them; a different boot/identity waits again.
A stopped checkpoint retains identity and start decisions, never assignments.
Without `ra_from`, requested IA types start immediately as before.

This opt-in was compared with routerd's netplan rendering and
`networkdAdoptionDropin`: its `DHCPv6Client=no` deliberately disables RA-driven
DHCP while retaining RA. Merely declaring an RA receiver must not enable DHCP.

The native parser and exchanges follow [RFC 8415](https://www.rfc-editor.org/rfc/rfc8415.html)
and the corresponding [RFC 9915](https://www.rfc-editor.org/rfc/rfc9915.html) recovery exchanges:
Solicit/Advertise/Request/Reply, server-bound Renew, multicast Rebind, individual
assignment validity, zero-lifetime withdrawal, retained deprecated old prefixes,
and a separate information transaction. Server-selected timers and lifetimes
are not echoed as client-selected values. Invalid identities, transaction IDs,
IAIDs and malformed option framing are discarded. An unusable IA assignment
is ignored independently of usable sibling assignments.
Multiple addresses and prefixes are retained, up to 64 of each per client.
A completed or replaced transaction cannot refresh an assignment.

The private automatic-identity file `state/leases/NAME.dhcp6.json.duid` is written
once before communication, then reused independently of the current lease or
link MAC. Explicit `client_duid` belongs to the configuration journal and never
overwrites that file, so confirmed rollback restores the automatic selection.
The WAN client and LAN server share the atomic identity writer under their
existing locks. A missing automatic file creates a fresh identity; discarded
prototype checkpoints are not used as an identity migration source.

The checkpoint `state/leases/NAME.dhcp6.json` stores the full typed
snapshot, including DUID/IAID, server identity and original BOOTTIME deadlines.
Atomic rename and file/directory fsync precede publication and the first packet.
On the same boot, matching interface/IAID/DUID and live assignments restore
their original remaining budgets. A client requesting PD uses Rebind; an
address-only client uses Confirm, following
[RFC 9915 sections 18.2.3/18.2.12](https://www.rfc-editor.org/rfc/rfc9915.html#section-18.2.3).
Both have a random initial delay and a ten-second verification window measured
from first transmission. Confirming addresses and their derived DNS are not
projected as usable until Success or the silent exchange's end. Confirm never
changes the assignment, original T1/T2, expiry or saved lease server. Success
from any matching-transaction server wins over earlier NotOnLink replies.
Only NotOnLink replies through the window discard old IA_NA and trigger fresh
discovery. Silence retains only still-valid original grants; expiry during
confirmation starts discovery immediately. Different boots, changed
identity/interface/IAID and expired grants use fresh acquisition. No UTC lease
rebasing or cross-boot restoration is implemented.
Graceful shutdown acknowledges kernel/RA withdrawal, persists an empty stopped
checkpoint, then sends Release. SIGKILL leaves the last saved grant for recovery.
The current contract is compiler native-44, checkpoint version 2; no discarded
prototype reader is retained.

This path was compared with routerd's `cmd/routerd-dhcpv6-client` DUID selection,
`restoreLease`/`saveLeaseLocked`, `pkg/pdclient.Restore` and configuration matching.
Explicit DUID wins; otherwise the saved DUID wins over a newly observed MAC.
Kernel DAD failure notifications remove the proposed address and start Decline. The
client subscribes before publishing assignments and reads kernel events in the
same exchange loop. Finite addresses disappear on DAD failure, so periodically
listing current addresses cannot detect the failure reliably. A valid Decline
Reply completes the exchange even with NoBinding. Worker readiness means the
socket is open, not that an address has completed DAD.

Without an explicit or saved DUID, Ethernet uses DUID-LL. Links without an
Ethernet address, including PPP, generate and persist a random DUID-UUID
([RFC 6355](https://www.rfc-editor.org/rfc/rfc6355.html)). Both worker and PPP
session replacements reuse the stored identity; saved leases remain observations
with their original expiry, separate from intent. `make check-pppoe-pd` exercises PPP
PD with independent ISC DHCP and a separate Linux SLAAC client. It requires
`dhcpd`; an extracted binary can be selected with `DHCP6_SERVER=/path/to/dhcpd`.
PVE Linux6.8/amd64, `pd-recovery`: independent RP-PPPoE and ISC DHCP4.4.3-P1
passed PAP and CHAP with actual PID1 supervisor/control/network/PPP/PD/RA workers
and an independent Linux SLAAC client. With ISC paused, a killed PD worker
restored identical DUID/IAID/prefix deadlines and actual IPv6 TCP/UDP; capture
contained Rebind before the peer resumed. Rebind/Reply then renewed normally.
The final CHAP run also restarted the worker with ISC absent, observed router
and client expiry at the original deadline, and restarted again without reviving
expired PD. Existing stale-notification, rollback, PPP/control/operator restart
and RA-withdrawal/Release ordering checks passed.

`make check-dhcp6` uses the real supervisor/control/network/client processes,
independent radvd and dnsmasq, RA M/O transitions, source-bound TCP/UDP, worker replacement, actual lease
renewal and server silence through Rebind/expiry/recovery. It also connects an
independent Linux duplicate host and verifies actual DAD/Decline packets, failed
address withdrawal and a usable replacement lease with TCP/UDP on the same
worker. The product configures the WAN bridge; no address flags are injected.
The RA fixture also checks RA-learned default/on-link routes, IPv6 MTU/hop-limit
application, writer restart, physical-MTU confirmed rollback and shutdown restoration.
`make check-dhcp6-ra-pd DHCP6_SERVER=/path/to/isc/dhcpd` connects independent
ISC DHCP4.4.3-P1/radvd2.19, a Linux duplicate host and a Linux SLAAC LAN client.
Explicit PD begins with M clear; M adds IA_NA in the same worker/socket/IAID.
Actual DAD/Decline leaves delegated LAN TCP/UDP usable; removing the duplicate
lets that worker acquire IA_NA. A paused server, cleared RA flag and worker
SIGKILL exercise identical combined grants/deadlines and Rebind recovery.
The delegating peer explicitly routes its PD pool via the client's real
link-local address. The product learns its WAN routes from radvd and the LAN
client learns its address/default route from the product, with normal DAD.
The same imported graph installs two PD-derived /128 endpoints on the WAN in
the LAN's delegated /64. Both endpoints pass source-bound TCP/UDP after normal
DAD, acquisition, worker recovery and expiry recovery. Kernel lookup for another
host in that /64 still selects LAN; the WAN endpoints add no competing subnet
route. Their installed mask comes from `delegations[].prefix_length` (zero/64
for LAN, 128 for a host endpoint). Host endpoints cannot generate LAN RA.

`make check-pd-ra`
uses an independent wire server and captures generated LAN advertisements;
IA_NA and IA_PD must share each stateful transaction and IAID.

Assignment, T1/T2, information-refresh and retransmission timing use
`CLOCK_BOOTTIME`. Snapshots preserve the originating boot and each original
budget. UTC is not used to reconstruct a lease at an IPC receiver. Explicit
infinity is scoped to the same boot. The network writer owns finite and
infinite address lifetimes and preserves them across its journal restart.

`make qemu-dhcp6-clock` runs the actual workers and an independent wire peer in
a dedicated no-NIC guest. It steps UTC forward/backward 24 hours, verifies
unchanged frozen-producer lease budgets and LAN TCP/UDP, then pauses control and
supervisor repair as well. The writer removes IA_NA/delegated addresses at their
original deadline. RA router/PIO/RDNSS lifetimes are bounded by original PD
authority and withdrawn; control unavailability may stop the RA worker early.
Expired higher-sequence delivery cannot restore ownership; fresh acquisition
recovers the same prepared RA identity. Only dedicated guest PID 1 can change
the guest clock, and restores its original baseline on exit.

These are isolated amd64 Linux VM checks. Actual suspend, RA gating combined with HA,
required-address readiness, complete legacy resource
selection, DAD failure alongside multiple IA_NA bindings, VRF and physical NIC
qualification remain separate work. Two-node transfer is described below. Separate-VM reboot,
partition/fencing, load, ARM64 execution and latest ONU/target-device acceptance
remain open. The former pinned PD state-machine
wrapper, timer corrections and prototype journal reader were removed; there is
no compatibility path.

## Two-node PD checkpoint transfer

The `dhcp6` service accepts `ha` with `owner` naming its configured VRRP service
and `replication` containing the same endpoint/certificate fields as
[DHCPv4 replication](../dhcpserver/README.md#two-node-replication). Use a separate
TCP port for each service. Both nodes need the same resource name, explicit
`client_duid`, IAID, requested IA types and prefix length. Reverse the local and
peer endpoints/certificates. Keep sync addresses independent of the shared WAN.
The shared WAN link and its link-local address must follow the same VRRP owner.
Do not put `when` on the PD service: its standby must run without the WAN link.

The compiler prepares active and standby workers once; current acknowledged
The live VRRP election selects PD acquisition through ordinary worker
replacement, before VIP publication. Graceful activation can therefore wait
for `service.pd.active` without waiting for its own unpublished VIP. LAN DHCPv4
allocation still waits for published ownership. Removing the owner or expiring
its election returns PD to standby; a poll does not extend the election.
Both immutable HA role files are prepared when accepting the source configuration;
observation updates select a role without rewriting worker configuration files.
Standby opens no
DHCP or DAD socket and projects no WAN/LAN assignment or RA. It pulls the active
member's complete checkpoint once a second over mutually authenticated TLS1.3
with the configured peer leaf pinned. An identity fingerprint covers the shared
PD contract. The active DHCP loop owns each response and samples its own
BOOTTIME; HTTP handlers never access mutable client state directly.

A received checkpoint retains the source snapshot unchanged and records the
source clock sample together with the local request-start clock. Takeover
derives local deadlines from this mapping; network/processing delay shortens
rather than extends the grant. UTC is not involved. An identical revision keeps
its original mapping even after repeated fetches or standby restart. Active
startup advances the persisted ownership epoch; snapshots advance revision.
Older epochs/revisions and conflicting equal revisions are rejected. These
counters prevent accepted checkpoint replay; they do not fence two isolated
VRRP masters. The active member continues its ordinary upstream protocol when
sync is unavailable. Standby reports the last successful receive time and any
sync error in `snapshot.sync`.

A member withdraws its local kernel/RA effects before stopping, but never
Releases the pair's shared upstream grant on demotion, process replacement or
member removal. Its last checkpoint remains recoverable. Standalone clients
still clear their checkpoint and perform bounded Release. An explicit pair-wide
release operation is not yet implemented. A standby reboot invalidates its
saved local clock mapping; it must fetch again or acquire afresh on promotion.

`make check-ha-pd DHCP6_SERVER=/path/to/isc/dhcpd DOCKER='sudo -n docker'`
uses two signed actual PID1 trees, independent ISC DHCP, Linux SLAAC and actual
IPv6 TCP/UDP. One router has a real Linux time namespace with BOOTTIME +7200s;
the host clock is unchanged. The existing DHCPv4/conntrack handoff checks also
exercise the shared TLS implementation. This establishes independent kernel
clock domains on one PVE amd64 VM, not qualification of two physical routers.
Cross-node transfer remains periodic complete snapshots, with an unreplicated
window between polls. Delta/gap recovery, disconnected-peer return/fencing and
target-hardware acceptance remain unfinished HA-02 work.
