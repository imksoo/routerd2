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
queued removal events are acknowledged before the listener closes. A final
peer transfer is attempted from the retained history. The hook reuses verified
Go executable bytes in the private root; there is no host helper lookup.

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
