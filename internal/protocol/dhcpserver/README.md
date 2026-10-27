# Native DHCPv4 server

This module replaces the DHCPv4 server process, lease database and protocol
socket responsibilities of dnsmasq. It does not start dnsmasq, Kea, dhcpd, shell
utilities, systemd or journald. Linux remains the IPv4 data plane. The worker
uses its existing network namespace and never configures client addresses,
routes, firewall rules or DNS directly.

## API and ownership

```go
cfg := dhcpserverconfig.Config{
    Resource: "DHCPv4Server/lan",
    Interface: "lan0",
    ServerAddress: "192.0.2.1",
    StatePath: "/var/lib/routerd/services/lan-dhcp/leases.json",
    SocketPath: "/run/routerd/services/lan-dhcp/control.sock",
    Primary: dhcpserverconfig.Scope{
        Prefix: "192.0.2.0/24",
        Start: "192.0.2.100",
        End: "192.0.2.199",
        LeaseSeconds: 43200,
        Router: "192.0.2.1",
        DNSServers: []string{"192.0.2.1"},
        Domain: "lan.example",
    },
    Authoritative: true,
}
err := dhcpserver.Run(ctx, cfg, dhcpserver.EffectsFunc(
    func(ctx context.Context, snapshot dhcpserver.Snapshot) error {
        return publish(ctx, snapshot) // Owning service/control boundary.
    },
), ready)
```

Config, Scope, Reservation, Lease, Snapshot, Stats and Event live in the pure
dhcpserverconfig package; the worker uses aliases. PID 1 need not import worker
sockets or lifecycle code. Normalize, Validate, ValidateSnapshot and FromSpec
are available in the pure package.

One worker owns one Ethernet interface. It verifies interface name/index, MAC,
MTU, up state, exact VRF membership and presence of ServerAddress. It repeats
checks while running and before sending replies. A namespace-local abstract
socket lock rejects duplicate server instances on the same interface; a file
lock protects the journal. The control plane must create the server address and
any VRRP virtual interface first. Standalone DHCP serves on that interface;
configured HA keeps a replication worker running on the standby too.

Readiness requires native sockets, an initialized/recovered durable journal,
and acknowledgment of the first full lease snapshot. It does not assert that
a client has acquired a lease.

Binding and renewal order:

1. Reserve an address with a bounded lease lifetime.
2. Write a private temporary journal, fsync it, atomically rename it and fsync
   its directory. With HA configured, send the same complete checkpoint to the
   standby and wait for its durable acknowledgment.
3. Publish the full typed snapshot and receive the control acknowledgment.
4. Verify ownership deadline and interface identity, then send DHCPACK.

Persistence or effects errors stop the worker before ACK. A persisted binding
remains an allocation exclusion even when its ACK was lost. Graceful shutdown
publishes Active=false and no leases, while retaining the journal. Stopping a server cannot revoke its clients' existing leases.

Every snapshot carries BootID, ObservedNanos and ExpiresNanos. CLOCK_BOOTTIME
is captured before walking the lease table, and the original deadline is at
most five seconds later. Publication refreshes every second and on lease changes.
The worker checks the same budget after control acknowledges and before replying.
Control decodes and validates a notification once, retaining typed lease data
for DNS, conditions and status. Committed digest, supervisor instance, worker
generation, producer epochs and sequence ordering fence retired publishers.
DNS receives the unchanged source deadline and checks it at each A/PTR answer.
There is no receipt lease in control or DNS; polling, replay, reload, UTC changes
and delayed ACK cannot create extra source authority.

Client allocation, journal retention and individual Lease.ExpiresAt still use
UTC. This observation-clock change does not qualify client allocation across
wall-clock correction, power loss or physical suspend. DNS also respects each
client expiry separately from the source deadline.

## Two-node replication

The operator's `dhcp_server` service can name its IPv4 VRRP owner:

```json
"ha": {
  "owner": "lan-ha",
  "replication": {
    "listen_address": "169.254.42.2:8443",
    "peer_address": "169.254.42.3:8443",
    "certificate_file": "/etc/routerd/ha/local.crt",
    "private_key_file": "/etc/routerd/ha/local.key",
    "peer_certificate_file": "/etc/routerd/ha/peer.crt"
  }
}
```

Both nodes need stable, reachable sync addresses independent of the VIP. Reverse
the endpoints and certificates on the other node. Use the same service name,
server address, pools, reservations, options and sticky policy. The authenticated
allocation-domain fingerprint covers these shared facts, excluding local paths
and interface names. The DHCP interface and server address must belong to the
named VRRP owner. Do not gate the DHCP resource with `when`: the standby needs
its listener even while the VIP is absent.

Replication uses TLS 1.3 with mutual certificate authentication and exact peer
leaf pinning. Certificates need the endpoint IP SAN and both serverAuth and
clientAuth usages. Keep private keys as private regular files with mode 0600.
Credentials are read at worker startup; install replacement files and restart
the worker to rotate them.

The runtime prepares standby and serving configurations once. Current VRRP
ownership selects the serving configuration through normal worker replacement.
The standby opens no DHCP socket and does not independently sweep or advance
the journal. Its status reports `standby=true`, `active=false` and no authoritative
DNS leases. One DHCP loop owns the journal, including authenticated peer writes.
PUT acknowledges only after local atomic persistence; duplicate checkpoints are
idempotent, conflicting equal revisions and older revisions are rejected.
Lease, sticky and quarantine deadlines are transferred unchanged. A serving
worker pulls its standby's complete journal before allocation; equal peer
revisions take precedence over an unacknowledged local branch. Serving endpoints
reject replication writes. `checkpoint_revision` reports the last completed
checkpoint; after a peer failure, the local file may contain a later provisional
write which was never acknowledged to a client.

This is an initial synchronous two-live-node implementation. Startup and new or
extended grants require the peer: loss of the peer stops DHCP admission until
it returns. This is not complete partner-down HA. Owner epochs, delta/gap
recovery, safe disconnected allocation and clock-skew/load/physical qualification remain
open. PD checkpoint transfer is implemented separately by the
[DHCPv6 worker](../dhcp6/README.md#two-node-pd-checkpoint-transfer). VRRP alone is not split-brain fencing.
The private journal is version 2 and the compiler contract is native-33. There
is no reader or migration for discarded prerelease journals and no supported
mixed old/new rollout. Recreate disposable development state; real deployments
still need a validated maintenance cutover procedure.

`make check-ha-state DOCKER='sudo -n docker'` uses two signed actual PID 1 trees,
independent BusyBox udhcpc clients and Linux TCP/UDP peers. It checks renewal
through takeover, worker replacement, interrupted uploads, authentication,
replays, external sync-link loss and recovery, and actual 60-second expiry with
sticky retention. Captures and temporary signed environments are deleted after
inspection. It does not qualify a physical NIC or ONU.

## Allocation and wire behavior

The engine implements DISCOVER/OFFER, selecting REQUEST/ACK/NAK, INIT-REBOOT,
unicast renewal, rebinding, RELEASE, DECLINE and INFORM. It bounds every
Ethernet request before slicing, validates critical option lengths, supports
file/sname option overload and long-option concatenation, ignores reserved
flag bits, and echoes the exact client identifier in replies.

A REQUEST selecting another server releases only this server's pending offer.
Unknown INIT-REBOOT clients requesting an address on the correct subnet receive
no response, including in authoritative mode. NAK policy accounts for server
selection and authoritative mode. Renewals must match an existing binding or
configured static reservation; they cannot take another client's live address.

Client identity is option 61 when present, otherwise the Ethernet address, and
is scoped by the configured pool scope. Exact MAC selectors precede OUI
selectors; duplicate selectors are rejected. Static reservations may be outside
the dynamic range but must be usable addresses in their selected subnet.
Server/router addresses, reservations, live leases, held offers and quarantines
are excluded from unrelated allocation. Routers expressed through option 3
are also excluded.

New on-link allocations receive a native ARP probe before OFFER. Conflicts are
persisted in quarantine, as are valid DECLINE messages for this server's own
offer or binding. Quarantine defaults to ten minutes. A probe is a best-effort
check, not proof that no sleeping or misconfigured host uses the address.

RELEASE verifies server identifier and client identity. StickyHoldDays can
reserve an old address for that client after release or expiry. Client messages
cannot release another identity's binding or quarantine an arbitrary address.

A device-bound UDP67 receiver checks packet interface/destination metadata.
Initial replies use broadcast when requested. Otherwise AF_PACKET sends a valid
IPv4/UDP frame directly to chaddr/yiaddr, so an unconfigured client need not
answer ARP. Configured clients receive routed UDP replies to ciaddr. Direct NAKs
always use broadcast.

Relays are disabled unless TrustedRelays explicitly allows both the source
address and giaddr, and the selected scope contains giaddr. Replies use relay
UDP67, echo well-formed relay-agent suboptions and set the broadcast flag for
relayed NAKs. Direct clients cannot inject option 82. This is a network IP
allowlist, not cryptographic authentication.

## Original resource compatibility

FromSpec(seed, api.DHCPv4ServerSpec, []api.DHCPv4ReservationSpec) reuses the original
public types. Seed provides resolved interface/server identity, primary subnet
and private paths. The adapter expands profiles/scopes, preserves options and
reservations, and retains the original direct-service versus legacy scope-style
gateway/DNS defaults. The original public reservation kind is
DHCPv4Reservation; no separate StaticLease kind is invented.

The compiler must first resolve status sources, aliases and when conditions,
and compile embedded DNS into the separate native DNS module. The adapter
explicitly rejects unresolved inputs, legacy backend semantics, transit role,
opaque legacy lease files and unsupported option syntax.

Options and Match retain original pkg/api types. Common IPv4, integer, text,
domain-search and classless-route options have typed encoders. Unknown numbered
options require an explicit hex: value. Protocol-owned fields, authentication
and rapid commit cannot be configured as arbitrary options. Options 119/121
support long TLVs. Replies respect parameter request ordering, the client's
maximum IP packet size and interface MTU. Optional values that do not fit are
omitted whole; mandatory identifiers are never truncated. Transmitted classless
routes suppress legacy router/static-route options for that client.

### Deliberate limits

- Ethernet DHCPv4 with usable IPv4 /1 through /30 subnets, and finite leases of
  60 through 4,294,967,294 seconds. BOOTP-only, DHCPv6 serving, non-Ethernet
  clients, authentication and rapid commit need separate implementations.
- Default 1,024 stored leases; maximum 2,048. Offers and quarantines are
  independently bounded by the same setting. Active snapshots are capped at
  1,792 KiB at publication and the receiving boundary to fit the 2 MiB service
  envelope. The worker stops before readiness or ACK if publication cannot fit;
  durable allocation exclusions are retained. Reducing max_leases cannot discard
  unexpired bindings to repair oversized state.
- Maximum 64 selector scopes and 128 trusted relays, plus bounded option
  payloads. Relay scope selection uses configured MAC/OUI selectors. RFC 3527
  link selection, arbitrary relay pool discovery, backend-specific tags and
  DHCP failover protocols are not implemented.
- The private journal is not dnsmasq's text file. There is no implicit import,
  SSH/rsync synchronization or active/active lease sharing. The HA mode above
  requires explicit coordination; VRRP alone does not provide it.
- Changed configuration preserves unexpired old allocations as exclusions.
  Records outside current pool/scope/reservation/lifetime policy are withheld
  from DNS ownership, not silently discarded to free addresses.
- Durable lifetimes require a usable wall clock. A journal substantially ahead
  of the local clock is rejected. Appliances with unreliable clocks must
  establish time before admitting leases.
- ARP probing and durable admission are serialized. Bulk admission throughput
  and large campus restart storms have not been benchmarked. DHCP control-plane
  tests do not establish forwarding performance.

This is a native protocol and state-ownership module. Complete compatibility of
the original policy graph, lease-sync controller and every dnsmasq extension is
not asserted by this worker implementation.

## Private observation

Parent directories must be private 0700 directories without symlink ancestors.
Journal, status, events and sockets are 0600. Live sockets and regular files
are not replaced during API creation. No TCP management listener is opened.

Optional read-only Unix API:

- GET /v1/status: latest typed snapshot.
- GET /v1/leases: the same current ownership snapshot.
- GET /v1/events?epoch=...&after=...: a 256-event cursor with explicit history gaps.

StatusPath is atomic. EventPath retains a 1 MiB current file and one archive.
Journal corruption fails startup rather than yielding an empty allocation
table. LogDHCP emits structured lifecycle/lease information; opaque client
identifiers stay in the private journal/API.

## Validation

Pure tests cover allocation/renewal rules, scopes, reservations, sticky holds,
identity checks, relay trust, conflicts, option wire shapes, independent packet
checksum verification, malformed packets and snapshot validation.

Runtime race tests use private temporary sockets/files and verify readiness,
fsync/effects-before-ACK ordering, failed-effects suppression, restart reuse,
owner locks, symlink refusal, event bounds/rotation and snapshot publication caps.

The integration fixture creates fresh user/network namespaces and veth peers
and runs the actual server against dhcp4.Run. It exercises DORA, a real
60-second lease's 30-second T1 renewal, server restart with INIT-REBOOT reuse,
independent NAK/RELEASE/DECLINE packets, quarantine, and Ethernet/IP unicast
before client address installation. It rejects duplicate instances and wrong
VRF intent; actual VRF-bound UDP is exercised when the kernel has the driver.
The outer process never changes host networking.

```sh
go test -race ./internal/protocol/dhcpserverconfig ./internal/protocol/dhcpserver
ROUTERD2_INTEGRATION_REQUIRED=1 go test -race -tags integration \
  -timeout=90s ./internal/protocol/dhcpserver
```

The required flag fails rather than skips when namespace isolation is unavailable.

## Protocol sources and license

- [RFC 2131: DHCP](https://www.rfc-editor.org/rfc/rfc2131.html)
- [RFC 2132: DHCP options](https://www.rfc-editor.org/rfc/rfc2132.html)
- [RFC 3046: relay information](https://www.rfc-editor.org/rfc/rfc3046.html)
- [RFC 3396: long options](https://www.rfc-editor.org/rfc/rfc3396.html)
- [RFC 3442: classless routes](https://www.rfc-editor.org/rfc/rfc3442.html)
- [RFC 6842: client identifier echo](https://www.rfc-editor.org/rfc/rfc6842.html)
- [Original routerd source](https://github.com/imksoo/routerd/tree/1329ef54f296507eda79bbbc3b853783dd27251e)

The new code is BSD-3-Clause. Original public types, compatibility behavior and
test client helpers retain the complete upstream attribution in NOTICE. No
dnsmasq, Kea or ISC DHCP source is embedded.
