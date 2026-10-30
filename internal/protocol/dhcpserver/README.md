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

Before first startup, active IPv4 bindings from a stopped old dnsmasq server
can be initialized explicitly:

```
routerd dhcp import-leases --config /path/to/router.yaml --service leases --state-dir /path/to/new-state /path/to/old-dnsmasq.leases
```

Use the compiled native service name and the new router's actual state path.
Stop the old DHCP server before copying its final lease file and starting the
new server. The command reads the configuration without applying it. It shares
the worker's exclusive state lock, requires an unused service directory, and
atomically installs the dnsmasq file without overwriting an existing file.
Expired bindings and DHCPv6 entries are omitted; live client identifiers and
absolute deadlines are retained. Conflicting bindings, foreign subnets and
reservation conflicts are rejected before creating state. The command does
not extend a lease, manufacture request history, or create HA ownership.

DHCP service follows the elected role; authenticated peer reconciliation
merges received leases and hold history. The initial two-member IPv4 lease
path is exercised by `make check-ha-dhcp-initial`: an independent dnsmasq
grants a real binding, then stops before both fresh members import the same
final file. Synchronization and takeover retain the original expiry.

While the former owner is unavailable, the original client uses its acquired
DNS over UDP/TCP before sending DHCP again and renews its known lease. A new
client requests that occupied address but receives a distinct address, route
and DNS automatically. It uses that DNS over UDP/TCP and its acquired gateway
for routed IPv4 TCP/UDP traffic through NAT. After the former owner returns,
the original client retains its address.

This check covers received leases. It does not establish protection for an
allocation that the former owner never synchronized. It does not import
sticky history or PD, or qualify physical migration, Internet forwarding,
NAT connection continuity or a power cut.

When sticky holds are configured, also import the stopped old routerd server's
sticky database before starting the new service:

```
routerd dhcp import-sticky --config /path/to/router.yaml --service leases --state-dir /path/to/new-state /path/to/old-dhcp-sticky.db
```

Run `import-leases` first when live IPv4 leases exist, then `import-sticky`,
and start the service only after both operations succeed. The sticky operation
also supports an installation with no live leases. Keep the old SQLite database
and its `-wal`/`-shm` siblings together when copying the stopped source; the
read-only SQLite connection includes committed WAL records. The operation uses
routerd's existing schema and retains allocation, request, release and hold
timestamps exactly. It selects only IPv4 history in this service's declared
subnets from the old shared database. It does not replay events at migration
time or extend the original holding period. Operator reservations retain
their normal precedence.

The two commands initialize separate files; they do not constitute one atomic
two-file migration. Sticky import holds the same worker lock, preserves an
already imported lease file, rejects existing history or initialized service
state, and atomically publishes the new SQLite file without sidecar dependencies.

`TestDaemonImportedDHCPLeases` obtains a real lease from an independent dnsmasq,
stops that server, imports its final file and starts the normal routerd build.
It checks DNS before the client sends DHCP again, the original client's retained
address, refusal to give that address to another client, acquired DNS over UDP
and TCP, and both clients' IPv4 reachability to the router. Run it with
`python3 scripts/daemon-tests.py dhcp-server --test TestDaemonImportedDHCPLeases`.

Periodic lease observations do not own the engine's serving lifetime. A failed
state publication is logged; the existing bounded periodic publication continues
with a fresh observation, while consumers expire the last accepted observation
at its original deadline. Configuration withdrawal, worker cancellation and
engine failures still stop serving. This retains routerd's separation between
status persistence and the existing dnsmasq process without extending deadlines.

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

VRRP selects serving authority; the lease synchronization link is not a second
ownership election. When reachable, the standby's retained bindings and history
are merged before startup. An unavailable peer is reported in sync status while
local DHCP continues with the configured pool. When synchronization returns,
the engine stops before its lease file is merged and resumes with both members'
received facts. Unsupported domains and a positively identified concurrently
serving peer are rejected.

Like routerd's asynchronous file synchronization, this cannot reconstruct a
binding never received from a failed member. In the observed sync-link outage,
the former active allocated to a client that then slept; after that member was
lost, the stale survivor assigned the same requested IP to another client. ARP
probing does not protect an offline client. This condition remains a limitation;
there is no automatic pool split or separate ownership journal.

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

DHCP fingerprint history is enabled explicitly with
`dhcp_server.fingerprint_retention`. The legacy importer connects
`LogRetention.signals: [dhcpFingerprints]` to each DHCPv4 service. The existing
bundled dnsmasq notification helper carries the client hostname, vendor class
and requested option list from dnsmasq's own environment. `script-on-renewal`
keeps actual renewals observable; startup lease-file replay and removals do not
refresh an observation. Collection does not require sticky reservations or
verbose DHCP text logs.

The DHCP worker owns `state/services/<name>/dhcp-fingerprints.db`, reusing
routerd's inference and SQLite schema. It stores the latest observation per MAC
with its original notification time. Its finite in-memory queue does not wait
for storage; overflow is counted and logged. Failed writes are reported and
are not retried as a durable delivery queue. DHCP replies, lease notification
acknowledgments and replication continue independently. Shutdown first drains
the existing dnsmasq hook, then gives queued fingerprint records two seconds.
Removing this setting stops collection and retention without deleting retained
history or stopping DHCP service.

Age retention and optional export-before-deletion reuse the existing retention
implementation. Configured UDP syslog export runs before deletion; a send error
keeps the affected rows for the next sweep. UDP send success cannot prove
receiver storage. The old fingerprint watcher parsed dnsmasq text logs, but its
automatic startup was not found in the inspected old repository. Migration
therefore enables the connected producer explicitly; it does not claim prior
watcher operation or import an old database.

`make check-dhcp-dns` includes `TestDaemonDHCPFingerprints`. On 2026-10-30,
independent BusyBox clients acquired and used distinct pool addresses while
SQLite writes were locked. Real storage errors did not stop their gateway
communication. New observations were saved after the lock was released, age
retention removed them, and removing collection allowed another client to
acquire and communicate without adding history. This does not establish
fingerprint replication, a traffic-load limit, or physical-device qualification.
