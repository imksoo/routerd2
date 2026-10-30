# Supervised DHCPv4 service

The worker uses routerd's `pkg/lan/dhcpv4server.RenderDnsmasqLines` and a pinned
bundled dnsmasq engine. There is no second Go packet parser, allocator or lease
journal. The network writer provisions interfaces, addresses and routing.

`make dnsmasq-engine` produces the runtime archive and matching sources. A
`dhcp_server` role requires that archive in the boot catalog or signed release.
The supervisor passes its verified open descriptor to the worker; there is no
host dnsmasq lookup, shell launcher, systemd or OpenRC dependency. The engine
runs in its own mount namespace and root filesystem, with only its lease
state directory writable. DNS serving is disabled (`port=0`); routerd's Go DNS
worker owns DNS. The runtime contains dnsmasq and its musl dependency.

The actual `dnsmasq.leases` file is the allocation authority. Its original
absolute expiries and client identities produce typed observations for DNS and
status, with the existing boot-clock observation lifetime. No acquisition or
renewal timestamps are invented. dnsmasq currently clamps leases below 120
seconds to 120 seconds. Option values retain dnsmasq syntax; the old native
wire encoder and its option restrictions have been removed.

HA uses the configured VRRP owner. A standby opens no DHCP socket. Both
members exchange the engine's lease records and routerd's actual request and
removal history over the existing mutually authenticated TLS link. The active
worker is the only history writer; dnsmasq's direct Go hook reports to a Unix
socket unique to that engine lifetime. Startup `old` notifications marked
`DNSMASQ_DATA_MISSING` cannot establish a fresh client request. Older lease-file
rows cannot undo a later removal. A real subsequent request clears the hold.
The original request, removal and hold times survive transfer and restart;
no acquisition timestamp is reconstructed from a configured lease duration.

The old routerd `RecordLeaseEvent` and host-reservation path supply the sticky
behavior. Explicit reservations win over held history. Released or expired
bindings remain reserved until their original hold deadline; reacquisition
uses dnsmasq's host entry (routerd used a 12-hour held-host lease). This history
uses UTC, as dnsmasq's lease file does; cross-node UTC skew is not qualified.

Before takeover, the worker reconciles and durably transfers ownership, then
starts serving. Known leases can renew during a disconnected promotion, while
unknown dynamic allocations stay blocked until the peer can be reconciled.
An interrupted transfer leaves ownership unknown. Only the explicitly
configured bootstrap member may initialize missing state without its partner.
A recorded established owner continues through a sync-link outage.

Cancellation starts bounded notification draining. The worker remains the
subreaper and history writer until dnsmasq and its script children finish;
queued removal events are acknowledged before the listener closes. Shutdown
does not wait for a peer transfer or idle HTTP connections. Replication closes
its sockets and finishes an admitted atomic write before releasing state;
the next authenticated exchange reconciles retained history. The hook is a small
Go executable included in the verified engine archive, with its source and
license notices. Startup does not copy the full worker into the engine root;
there is no host helper lookup.

Current real communication: `lan-dnsmasq-history` on PVE Linux/amd64 with two
signed PID 1 product trees, independent BusyBox clients, kernel NAT and capture.
Both-direction handoff, unchanged TCP connection/UDP mapping, DHCP renewal,
new distinct allocations, worker/engine restart, sync outage/recovery, and
sticky reacquisition passed. RELEASE while the history worker was paused,
sync-link outage and normal product shutdown preserved the removals and hold
deadlines; stale peer lease rows did not revive them after recovery. Separate
natural-expiry and stale-owner cases passed the preceding implementation.
The one-second VRRP profile passed; a 200ms handoff re-election remains open.

`lan-dnsmasq-history` also passed a real HGW with two independent automatic
clients, DHCPv4 natural renewal, DHCPv6 information/SLAAC, conditional DNS/DoH,
IPv4/IPv6 HTTPS, PD SIGKILL/graceful recovery and whole-daemon recovery. This was
a 75-second traffic interval, not long-run or physical-device qualification.
`lan-reuse` additionally passed actual BusyBox/native and imported DHCP-to-DNS
paths, explicit option-6 delivery, stopped-source expiry and recovery.

Remaining: broader pool/profile/reservation interoperability, target-image
execution, load/clock/power/NIC qualification and the complete release plan.
DHCPv6 and RA still use the existing Go workers; choosing dnsmasq for DHCPv4
does not decide their implementation. Compare their production paths with
routerd while completing the actual homert acceptance profile.

On 2026-10-29, the bundled small hook passed the actual Alpine 3.24.3/Linux
6.18.59-0-lts signed PID 1 worker-recovery profile in QEMU/amd64 (512 MiB,
2 virtual CPUs, TCG). Independent dhcpcd, RP-PPPoE/pppd and ISC DHCP peers
exercised automatic client DNS and IPv4/IPv6 TCP/UDP, prefix renewal, PPP worker
replacement and control replacement with retained DUID and restored client
communication. This establishes that profile, not normal signed-update
confirmation, the full homert configuration, or physical-device qualification.

`make check-ha-dhcp-scopes` checks imported ordinary and secondary IPv4 pools
with two signed PID 1 product instances and independent BusyBox clients in
private namespaces. A /32 VRRP address identifies the server; each physical
LAN keeps its own secondary /16 address. One profile is selected by an OUI
and two exact MAC addresses, with all three selectors sharing its pool.
The clients must receive distinct addresses and the explicit mask, router,
DNS and virtual server identifier, then renew the same leases after both
handoff directions. The existing TCP connection and UDP mapping also remain.
The test needs the standard HA peer dependencies and bundled engines; it does
not need an ISC DHCPv6 server. Its isolated stages are removed on exit.

On 2026-10-30, base `7fc4d7c` gave a matching special client the ordinary pool
and ordinary options. dnsmasq selects ranges from the receiving interface's
subnets; HA had moved the listener to the VIP interface. The engine renderer
now uses dnsmasq's existing `shared-network` setting to associate other
configured subnets with the server address, without duplicating addresses or
changing lease ownership. The corrected profile passed at 06:01 UTC, including
all three selectors and both handoffs. An earlier corrected run stopped before
DHCP acceptance on a VRRP ownership notification timeout; that separate
intermittent failure is not claimed resolved here. This check verifies DHCP
assignment and renewal, not forwarding through the special profile's external
gateway or DNS resolver, a complete homert deployment, or physical hardware.
