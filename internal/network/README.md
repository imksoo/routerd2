# Native Linux network engine

This package owns the native Linux network projection. Protocol workers produce
typed desired state and never write kernel state themselves. The engine uses
rtnetlink, generic netlink, nfnetlink and fixed procfs sysctl files directly; it
does not execute host commands. This remains a bounded typed API rather than an
arbitrary Linux networking command interface.

## Current behavior

- Create owned bridge, 802.1Q VLAN, VRF, VXLAN, macvlan and WireGuard links;
  manage explicit aliases, membership, MTU and administrative state.
- Manage IPv4/IPv6 addresses, finite preferred/valid lifetimes, explicit NoDAD
  and point-to-point peers. Automatic IPv6 address generation can be disabled
  on created links. Per-interface kernel RA/autoconf writers are explicitly
  owned/restored for native RA clients.
- Manage unicast routes, weighted ECMP, policy rules, tables and metrics. Finite
  IPv6 routes expire in the kernel while the control plane is stopped. The
  control plane resolves recursive next hops before submitting native routes.
- Carry each learned BGP route's original boot ID and BOOTTIME deadline into
  the ownership journal. A loop in the network worker removes expired routes
  even when supervisor, control and BGP cannot progress. Refreshing only that
  authority updates the journal without deleting/readding the kernel route.
- Enable or disable IPv4 and IPv6 forwarding only when requested explicitly.
- Replace only the `inet routerd2` nftables table in one kernel batch. The
  legacy forwarding chain drops invalid connections, accepts established/related
  traffic, then accepts explicit ingress/egress pairs. Its default policy is
  **drop**. Typed input/output/forward chains can add CIDR, interface, protocol,
  port/range, ICMP, accept/drop/reject and logging rules. Input/output hooks are
  absent when their respective chain is nil.
- Explicit masquerade, SNAT and DNAT support both families and optional port
  translation. NAT does not imply a forwarding permission. The separately owned
  HA table implements VRRP Accept_Mode receive guards with ND/VRRP exceptions.
- Create a native IPv4-in-IPv6 `ip6tnl` DS-Lite B4 tunnel with static underlay
  endpoints. Addresses, routes, forwarding permission and optional NAT are
  separate declarations. AFTR discovery and DHCP/RA-derived endpoints belong to
  protocol workers and the coordinator; this package applies their resolved
  projection. It does not implement an AFTR server.
- Configure the Linux WireGuard data plane through its IFINDEX generic-netlink
  API, with owned-device enforcement, public status and bounded peer sets.

`Firewall: nil` leaves an unowned firewall untouched, but removes a table that
this engine previously created and recorded. A non-nil empty firewall enables
the default-drop forwarding chain. Global forwarding changes affect the whole
network namespace. The caller must ensure this is the intended router namespace.

## Ownership and failures

The journal and lock are worker-owned 0600 singly linked regular files. Every
ancestor is opened without following symlinks and must belong to root or the
worker, with no group/other writes except a root-owned sticky temporary ancestor.
The final directory belongs to the worker and permits no group/other writes;
new directories use 0700. The engine pins this directory for reads and atomic
temporary-file/fsync/rename/directory-fsync publication. Replacing its original
pathname cannot redirect a running writer into a different directory.
An already opened read descriptor may refer to an unlinked old inode during
atomic replacement; it still yields one complete immutable record. Lock and
write targets must remain linked. Hard links remain rejected in both cases.

Reads and writes share a 64 MiB encoded limit and a two-million JSON-value limit
with depth at most 64. Duplicate, unknown and case-aliased typed fields, trailing
values, unsupported versions, invalid status and invalid ownership records are
rejected. The configuration and network readers share the strict shape decoder;
operator map keys and custom JSON types retain their own meaning. The writer
checks the structural budget before publishing, so an oversized record cannot
replace the previous journal. An I/O failure can still interrupt an apply;
these bounds are not a complete power-loss or storage-failure qualification.

`InspectJournal(ctx, path)` reads a pinned record without acquiring/creating a
lock, initializing directories, syncing, projecting the kernel or adjusting a
saved BOOTTIME deadline. Only a successful lookup in a checked existing parent
can report `present:false`; an uninspectable parent is an error. Its bounded
metadata contains presence, format version, revision, attempted revision,
persisted status and the complete payload SHA-256, not ownership bodies or key
references. This is one persisted record, not kernel health or a cross-worker
snapshot. Readers may see either complete side of a concurrent atomic rename.

The live engine method additionally fences its writer, fails immediately while
busy, rejects a closed engine and treats disappearance of an established journal
as an error. The private network socket exposes it as `GET /v1/journal`, with no
caller-selected paths or query parameters. Read/ownership/busy errors return
503; an unsupported backend returns 501. It re-reads disk rather than returning
cached memory. The signed boot preflight fixture compares this response with a
direct inspection of the actual journal before preparing a pair; operator paired
prepare/boot and qualification of other state families remain separate work.

Ownership journal format 4 records controller-generated boot-scoped address
lifetimes, route deadlines and virtual-link activation deadlines. Earlier unreleased formats are refused without rewriting their contents; no
prototype format migration or mixed-version rollback is supported. Operator
configuration cannot declare `boot_lifetime` or link/route `boot_id` and
`expires_nanos`. It is the internal projection
of an accepted finite protocol grant.

For such addresses, boot ID and absolute preferred/valid BOOTTIME deadlines
remain unchanged through writer restart and replay. Ownership checks and the
kernel relative lifetime calculation use that original clock, independently
of UTC corrections. An expired/foreign-boot intent cannot recreate an address,
and an externally lengthened live lifetime is not adopted or deleted. Linux
relative address expiry is also checked with the writer closed. Physical
suspend remains unqualified.

BGP route deadlines use the source observation's boot ID and absolute expiry;
neither receipt, writer restart nor reapply creates a new lease. Operator route
resources cannot supply these internal fields. Expiry and configuration removal
share the journal/live-route ownership check and leave changed foreign routes
alone. Before creating routes the writer checks their absence and persists all
add intentions, including original deadlines, in one batch. A crash after
netlink creation therefore does not lose the ownership needed for withdrawal.
Each operation reads a routing table once, rather than dumping the whole FIB
for every refreshed or expired route. Expiry removals are persisted together.

Conditional virtual links retain the original boot deadline of their conditions
and dependency readiness. The same network worker loop fences an expired owned
link DOWN and removes its managed addresses, including their local-delivery
routes. It verifies the recorded interface index, ownership marker and link
attributes first. A foreign replacement is left untouched; unrelated addresses
are not explicitly deleted. Link renewal changes the ownership journal without
rewriting kernel link attributes. The UP operation rechecks original authority
after intervening I/O, and an expired projection cannot reactivate the link.

Addresses managed on a finite link receive the tighter of their own lifetime and
the link's deadline. Static and UTC lifetimes are bounded in the prepared writer
projection; protocol BOOTTIME deadlines stay on their original clock. The source
configuration is unchanged. The kernel's second-resolution address timer also
limits the address if the writer is unavailable; exact link fencing requires a
running writer.

Finite links with an explicit Ethernet MAC emit an immediate and one-second
[RARP request-reverse](https://www.rfc-editor.org/rfc/rfc903.html) while UP and
owned. This maintains switch learning when a fixed MAC moves between peers,
including after the previous owner sends a final RA or queued frame. It does
not announce an IP mapping or default route, nor renew any observation. The
socket send checks the original boot deadline. This requires raw Ethernet socket
permission in the network worker. The transient send schedule is rebuilt after
restart; it is not another persisted authority.

The authority loop requires a running network writer. It is independent of
the control worker, but it is not a kernel IPv4 timer. A stopped/unavailable
writer cannot fence virtual links or remove IPv4 routes until it resumes or restarts. Linux's native
[`expires` route option](https://man7.org/linux/man-pages/man8/ip-route.8.html)
is IPv6-only. BFD receive deadlines and recursive next-hop dependencies are
still evaluated by control; carrying all prerequisite lifetimes into the writer
is not established by the BGP observation deadline alone.

The state file is an ownership journal, not a configuration backup. Hold its
directory in persistent storage and retain it across process and image upgrades.
The engine locks it against a second instance using the same path.

An existing address or route is not silently adopted, even if it looks identical.
The engine needs both journal ownership and a compatible live object before
removing it. Route protocol 242, the tunnel alias and the nftables ownership
marker are additional checks, not sufficient evidence on their own. Unrelated
routes, addresses and nftables tables are retained. Whole-link deletion refuses
remaining address or route references, including automatically created addresses
that cannot be distinguished safely from externally installed addresses.

For MTU, interface UP and forwarding settings, the journal stores the original
value and the most recently written value. Relinquishing management restores the
original value only when live state is still the last value or already the
original value; conflicting external changes produce an error. This is a
single-writer design: cross-process races that occur between an observation and
a netlink operation cannot be fenced using this journal.

Only the nftables replacement is atomic. Interface, address, route, tunnel and
sysctl operations may partially succeed. Route additions use the recorded
intent described above. Other resource mutations synchronize ownership after
each successful mutation; a crash before persistence can leave an unclaimed
object. The engine fails conservatively on such a collision rather than
inventing ownership. `ApplyError.Partial` and a
`degraded` snapshot report failed reconciliation; they do not promise that the
previous state has been restored.

The control layer owns revision concurrency. `Apply` accepts equal and lower
revision numbers to repair drift or reconcile the last committed configuration
after a failed candidate. Such a rollback is another reconciliation and can
itself fail. Deleting a primary IPv4 address that could implicitly delete another
address is refused. Tunnel replacement and route changes are not hitless.

## WireGuard contract

Declare a `Link` of kind `wireguard`, its `WireGuard` configuration, and an
`Interface` when the device should be UP. Existing same-named WireGuard devices
are never adopted. The random link ownership token, interface index and durable
journal must all agree before any DOWN, key change or deletion. A new foreign
peer, unknown AllowedIP, changed private/PSK, fixed listen port or firewall mark
causes reconciliation to stop without changing that device.

| Field | Behavior |
| --- | --- |
| `private_key_file` | Required clean absolute path to a worker-owned regular key file with no group/other permissions. |
| `generate_private_key` | Explicit opt-in to publish a missing file from a private, fsynced temporary file using atomic no-replace rename; existing files are never overwritten. Its parent directory must already exist. |
| `listen_port` | 1–65535 is explicit; 0 requests an initial random port which is then retained for this owned device. |
| `firewall_mark` | Explicit uint32 mark, including zero to clear. No policy rule or firewall permission is added implicitly. |
| `peers` | Complete desired peer set. An empty set removes all owned peers. |
| `public_key` | Canonical nonzero base64 key; duplicate and self peers are rejected. |
| `preshared_key_file` | Optional versioned secret file. Empty clears the PSK. |
| `allowed_ips` | Canonical IPv4/IPv6 network prefixes. Identical prefixes cannot occur twice; nested distinct prefixes use WireGuard longest-prefix matching. |
| `endpoint` | Numeric unicast IP:port, with an interface zone for IPv6 link-local. Empty enables passive endpoint learning. Hostname resolution belongs to the control plane and is rejected here. |
| `persistent_keepalive` | 0–65535 seconds, with zero disabling it. |

No `wg`, `ip`, external daemon or userspace UAPI socket is involved. Existing
netlink dependencies encode the documented WireGuard kernel UAPI; the kernel
owns encryption, handshakes, replay protection and packet processing. AllowedIPs
does not install FIB routes. MTU, addresses, VRF membership, routes, policy rules
and firewall input permission are explicit independent declarations.

The kernel's REPLACE_PEERS/REPLACE_ALLOWEDIPS flags are **not an atomic
transaction**. The engine durably records intent, takes only its verified owned
device DOWN, sends the complete set in bounded batches, verifies full readback,
then brings it UP if requested. A failed or interrupted batch stays DOWN.
Failures after the UP syscall, including a lost acknowledgment, canceled apply,
or journal failure, attempt quarantine using a separate five-second context.
That path rechecks link and device ownership and sends DOWN before attempting
another journal write. Its native netlink sockets close at the deadline. If
ownership changed or the kernel cannot be observed/controlled, the error reports
that quarantine could not be verified; it does not alter foreign state or claim
that the device is closed. Blocking filesystem I/O still needs a platform
watchdog.
Restart/rollback accepts only known values and peer/prefix subsets from the
saved before-image and pending intent. Replacement interrupts sessions and can
cause Linux to remove IPv6 addresses on DOWN; the engine reapplies its owned
addresses during the same reconciliation. Do not describe this as hitless.

The current bounds are 64 configured devices, 1024 peers per device and 16384
AllowedIP prefixes per device. SET batches remain below 24 KiB; one large peer
can span batches, and GET readback joins its multipart prefix fragments. Limits
are explicit validation errors, never silent truncation.

Runtime roaming endpoints, handshake times and counters are observations. A
roaming endpoint survives unrelated peer updates and worker restart; changing
the declared endpoint deliberately overrides it. Changing a static endpoint to
empty recreates the peer without the old endpoint, because Linux does not
support clearing it with an unspecified sockaddr.
An interrupted explicit endpoint change is different from authenticated roaming:
rollback restores the committed declaration. The journal preserves this
distinction across a second interrupted rollback instead of adopting a candidate
bootstrap endpoint as an authenticated observation.

### Keys and rollback

On first use, each private/PSK file path is bound to an immutable secret snapshot
in `<StatePath>.wg-secrets/` (0700 directory, 0600 files). The ownership journal
stores file bindings and content hashes, never key bytes. The path binding is
stable: editing the source file in place does **not** rotate a key. Rotation is
an explicit configuration commit to a new versioned path; rollback can retrieve
the previous key even if its original source file has been removed. Back up the
private store together with the network journal. Old snapshots are retained for
rollback and are not garbage-collected automatically.

Both generated sources and immutable snapshots are published only after their
complete contents have been fsynced, using `RENAME_NOREPLACE` and directory fsync.
A crash before publication may leave a randomly named private temporary file;
readers ignore it and retries create a new temporary file. These orphan files
are retained rather than deleting potentially unrelated files. A crash after
publication can be retried by validating the immutable file and syncing its
directory again. Corrupt final files are reported, never silently overwritten.

The reader rejects symlinks in every path component and permissive file modes.
Ancestors must belong to root or the worker UID and must not allow group/other
writes, except a root-owned sticky temporary directory. Opens are nonblocking,
so an invalid FIFO cannot hang the worker before its type is checked. Kubernetes projected
Secrets normally use symlinks; install their contents as a versioned 0600 regular
file in the router namespace before referencing them. Key material is never
accepted inline by this native API. Public status contains only public keys,
listen port, mark, AllowedIPs, runtime endpoints, handshake times and counters.
Read/parse errors never echo key contents. This design separates key storage
from configuration and logs; it does not claim encrypted storage or guaranteed
erasure of all copies held by the Go runtime or kernel.

## Validation

`go test ./internal/network` uses an in-memory kernel fake and never mutates the
host network. It exercises restart ownership, failed-candidate recovery, lower
revision reconciliation, before-image restoration, conflict detection and
foreign-resource preservation.

`CGO_ENABLED=0 go test -tags integration ./internal/network -v` includes an
isolated native-kernel test. The parent re-executes the test in a fresh user and
network namespace before any mutation; the child verifies its network namespace
differs from the parent. It builds client/router/server veth topology and checks
IPv4/IPv6 forwarding, NAT, reverse-connection filtering, IPv4-in-IPv6 traffic and
cleanup. A restricted runtime skips the native test when it cannot create the
isolation. Set `ROUTERD2_INTEGRATION_REQUIRED=1` in a suitable CI kernel to make
that restriction a failure. A skipped test is **not** kernel or packet-path
verification.

`make qemu-wireguard` runs the fixed WireGuard gate against the appliance kernel
in a VM with no external NIC. Its two namespaces exchange encrypted IPv4/IPv6
TCP over IPv4 and IPv6 UDP transports, exercise authenticated endpoint roaming
and a 2050-prefix multipart update, and interrupt a real kernel update after a
128-prefix partial peer set. Restart must restore the saved configuration and
both address families while the interrupted device remains DOWN until verified.
Real-kernel UP acknowledgment loss and apply cancellation also exercise the
independent quarantine path and successful rollback afterward.
The gate also checks wrong-PSK/peer-removal traffic rejection and foreign peer
preservation. Its required PASS marker and artifact hashes are recorded in
`dist/wireguard-test`; a guest that exits before running the test is a failure.
