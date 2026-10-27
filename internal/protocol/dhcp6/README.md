# Native DHCPv6 client

One worker owns UDP 546 on one interface. `request_address` requests IA_NA,
`request_prefix` requests IA_PD, and both can be enabled in the same exchange.
With neither enabled, the client performs Information-request only. DUID and
IAID belong to this client; additional workers cannot own separate IA types on
the same interface. The network writer installs learned IA_NA addresses as /128
and derives configured LAN addresses and advertisements from IA_PD prefixes.
The client does not infer a default router from DHCPv6.

The native parser and exchange implement [RFC 8415](https://www.rfc-editor.org/rfc/rfc8415.html):
Solicit/Advertise/Request/Reply, server-bound Renew, multicast Rebind, individual
assignment validity, zero-lifetime withdrawal, retained deprecated old prefixes,
and a separate information transaction. Server-selected timers and lifetimes
are not echoed as client-selected values. Invalid identities, transaction IDs,
IAIDs, duplicate/truncated options and unusable assignments are discarded.
Multiple addresses and prefixes are retained, up to 64 of each per client.
A completed or replaced transaction cannot refresh an assignment.

One private checkpoint under `state/leases/NAME.dhcp6.json` stores the full typed
snapshot, including DUID/IAID, server identity and original BOOTTIME deadlines.
Atomic rename and file/directory fsync precede publication and the first packet.
On the same boot, matching interface/IAID/DUID and live PD restore the remaining
grant without extending it. Startup sends Rebind with Confirm's bounded retry
parameters, as specified in [RFC 8415 section 18.2.12](https://www.rfc-editor.org/rfc/rfc8415.html#section-18.2.12).
Silence does not renew assignments; ordinary original T1/T2/expiry still apply.
Different boots, changed identity/interface/IAID, expired PD and IA_NA-only
clients use fresh acquisition. IA_NA-only Confirm recovery remains unimplemented.
Graceful shutdown acknowledges kernel/RA withdrawal, persists an empty stopped
checkpoint, then sends Release. SIGKILL leaves the last saved grant for recovery.
The current contract is compiler native-32, checkpoint version 1; no discarded
identity-file or PD-wrapper journal reader is retained.

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
independent dnsmasq, source-bound TCP/UDP, worker replacement, actual lease
renewal and server silence through Rebind/expiry/recovery. It also connects an
independent Linux duplicate host and verifies actual DAD/Decline packets, failed
address withdrawal and a usable replacement lease with TCP/UDP on the same
worker. The product configures the WAN bridge; no address flags are injected.
Both independent-server and DAD/Decline fixtures passed with `pd-recovery`.
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

These are isolated amd64 Linux VM checks. Actual suspend, automatic
RA-triggered DHCPv6, required-address readiness, complete legacy resource
selection, DAD failure alongside other IA_NA/IA_PD bindings, VRF and physical NIC
qualification remain separate work. Authenticated two-node PD transfer,
cross-boot deadline transfer, HA ownership/fencing and demotion without Release
remain unimplemented; local recovery does not complete HA-02. Latest native-32
ONU/target-device acceptance is not established. The former pinned PD state-machine
wrapper, timer corrections and prototype journal reader were removed; there is
no compatibility path.
