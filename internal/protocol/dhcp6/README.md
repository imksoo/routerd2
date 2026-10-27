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

Only the DUID identity is persisted under `state/identities`; restart requests
fresh server confirmation through a new acquisition. No saved lease is
published as current, and no discarded PD-wrapper journal is imported. Kernel
DAD failure notifications remove the proposed address and start Decline. The
client subscribes before publishing assignments and reads kernel events in the
same exchange loop. Finite addresses disappear on DAD failure, so periodically
listing current addresses cannot detect the failure reliably. A valid Decline
Reply completes the exchange even with NoBinding. Worker readiness means the
socket is open, not that an address has completed DAD.

Without an explicit or saved DUID, Ethernet uses DUID-LL. Links without an
Ethernet address, including PPP, generate and persist a random DUID-UUID
([RFC 6355](https://www.rfc-editor.org/rfc/rfc6355.html)). Both worker and PPP
session replacements reuse the stored identity; learned leases remain live
observations rather than persisted intent. `make check-pppoe-pd` exercises PPP
PD with independent ISC DHCP and a separate Linux SLAAC client. It requires
`dhcpd`; an extracted binary can be selected with `DHCP6_SERVER=/path/to/dhcpd`.

`make check-dhcp6` uses the real supervisor/control/network/client processes,
independent dnsmasq, source-bound TCP/UDP, worker replacement, actual lease
renewal and server silence through Rebind/expiry/recovery. It also connects an
independent Linux duplicate host and verifies actual DAD/Decline packets, failed
address withdrawal and a usable replacement lease with TCP/UDP on the same
worker. The product configures the WAN bridge; no address flags are injected.
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
unchanged frozen-producer snapshots and LAN TCP/UDP, then pauses control and
supervisor repair as well. The writer removes IA_NA/delegated addresses at their
original deadline. RA router/PIO/RDNSS lifetimes are bounded by original PD
authority and withdrawn; control unavailability may stop the RA worker early.
Expired higher-sequence delivery cannot restore ownership; fresh acquisition
recovers the same prepared RA identity. Only dedicated guest PID 1 can change
the guest clock, and restores its original baseline on exit.

These are isolated amd64 Linux VM checks. Actual suspend, reboot, automatic
RA-triggered DHCPv6, required-address readiness, complete legacy resource
selection, DAD failure alongside other IA_NA/IA_PD bindings, VRF, graceful
Release and physical NIC
qualification remain separate work. The former pinned PD state-machine
wrapper, timer corrections and prototype journal reader were removed; there is
no compatibility path.
