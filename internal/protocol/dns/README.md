# Native DNS worker

The worker reuses routerd's resolver, upstream transports and static-zone
behavior. `Run(ctx, cfg, input, report, ready)` owns its listeners and pools until
cancellation. The control resource transaction owns immutable configuration;
this module has no file-reload, config-write or raw DHCP-event API.

`Config` contains `Runtime`, `SocketPath`, `StatePath` and optional `EventPath`.
The service compiler assigns private runtime/state paths and the query-log path.
Configuration normalization copies only the listener/source slices it changes;
other nested intent is read-only. No JSON round trip clones configuration.

`dnsconfig.Input` carries complete finite DHCP lease sets and learned DNS
upstreams separately from configuration. The control service prepares DNS
configuration once and exposes input at `/v1/services/NAME/dns-input`.
The worker checks the committed digest, resource, declared sources and original
boot deadlines. A failed poll retains those deadlines; it does not renew them.
Missing sources contribute no records or servers. Repeated ownership heartbeats
keep existing pools and caches. Changed bindings invalidate affected-generation
cache entries while existing TCP listeners stay open.

Listener `addressFrom` accepts `Interface/IFNAME` with `addresses`,
`ipv4Addresses` or `ipv6Addresses`; `VirtualAddress/VRRP_SERVICE` with `address`;
and `IPv6DelegatedAddress/PD_SERVICE/ALLOCATION` with `address`.
The compiler resolves these references once. The DNS worker observes interface
addresses; control supplies only applied delegated addresses after DAD and
currently published VIPs with their original deadlines. Missing dynamic
addresses leave the process pending when no other listener exists.

Only added sockets are opened and removed sockets shut down. Retained sockets,
TCP connections and upstream pools stay alive. Each socket retains its logical
listener's source selection, including during publication of a new binding.
VIP and delegated sockets reject queries after the original grant expires,
including on retained TCP connections when control is unavailable. A worker-owned
observation tick removes expired sockets and follows interface changes without
creating another producer or storing learned values in operator intent.

The worker reports its actual typed socket endpoints through the existing
service observation channel. The two-second BOOTTIME observation is generated
by the worker; retransmission cannot renew it. A DHCPv6 server selecting its own
delegated DNS address waits for both DAD and matching port53 listeners, and its
serving input keeps the original prefix/observation deadlines. Delivery failure
expires readiness at the consumer while fixed DNS sockets keep serving intent.

Legacy DNSUpstream `addressFrom` and `bootstrapFrom` are translated to the same
prepared native provider references. They do not become frozen endpoint lists.
`make check-dhcp6-ra-pd` exercises CLI import and independent ISC, keepalived,
dnsmasq, BusyBox and Linux clients. Product/fixture results and unqualified scope
are recorded in `docs/pve-codex-handoff.md`.

A DNS source may select negotiated IPCP servers with:

```json
{
  "name": "provider",
  "kind": "forward",
  "match": ["."],
  "upstreamFrom": [
    {"resource": "PPPoESession/internet", "field": "dnsServers"}
  ]
}
```

`internet` names a configured PPP service. The same `dnsServers` reference accepts
`DHCPv4Client/NAME`, `DHCPv6Address/NAME`, `DHCPv6PrefixDelegation/NAME`,
`DHCPv6Information/NAME` and `IPv6RAAddress/NAME`, each bound to its declared
service role. Each endpoint carries its own original boot deadline. RA RDNSS
entries can expire independently; DHCPv4 uses its ACK lease, and DHCPv6 uses
its information budget capped by surviving assignments. IPv6 link-local DNS
addresses retain the provider interface scope. Provider and wire address order
are preserved. No protocol budget is replaced with a receipt timeout.

Static `upstreams` may coexist with
learned servers. Routing remains explicit operator intent, including a routing
table or `viaInterface` when needed; the resolver installs no hidden routes.
An incoming-interface condition keeps a LAN forwarding PBR rule from matching
the router's own DNS replies. A route through a protocol-owned link requires
an explicit dependency and active ownership condition; the normal resource
lifetime mechanism withdraws it when that fact expires.

The pool retains health for unchanged endpoints and uses each original deadline
for queries and probes. A single pool probe cycle owns its concurrent exchanges;
new addresses need no producer registry or extra per-address lifetime manager.
Expired or withdrawn endpoints are not queried. Status shows endpoint URLs and
marks expired or unobserved sources honestly; health is unknown without usable
observations and degraded when all observed endpoints have failed.

Static records and PTR ownership take precedence over native DHCP bindings.
Lease files, recovered sticky records and arbitrary lease injection are not
accepted: the DHCP worker owns leases and their original observation/client
expiry. DNS does not create a second lease journal or revive old addresses.

UDP replies use the client's EDNS payload size, or 512 bytes without EDNS.
The existing DNS library truncates oversized answers and sets TC so clients can
retry over TCP. TCP retains the complete answer, including after a cached UDP
reply was truncated. This also applies when the worker obtained a complete
answer by retrying a truncated upstream UDP reply over TCP.

Reproduce this boundary with independent dnsmasq and dig through two product
resolvers using `python3 scripts/daemon-tests.py dns-upstream --test
TestDaemonDNSResponseFitsClientUDPSize --binary-dir dist/bin`. The isolated peers
use static addresses; this check does not qualify DHCP startup or a physical LAN.

The private socket exposes `GET /v1/healthz`, `/v1/status`, `/v1/events` and
`/v1/config`. Query logging and bounded event history remain module-owned.
PAP/CHAP communication is reproduced with `make check-pppoe`; PD with
`make check-pppoe-pd DHCP6_SERVER=/path/to/dhcpd`. Independent RP-PPPoE and dnsmasq
peers exercise actual LAN UDP/TCP queries, original expiry with a reachable
peer, changed negotiated servers and the same dig TCP connection.

`make check-ra-route-refresh` additionally uses independent dnsmasq DHCPv4,
DHCPv6 information and RA advertisements plus a separate DNS peer. It checks
LAN UDP/TCP DNS, scoped link-local queries, original RDNSS expiry while the
peer remains reachable and recovery alongside actual AFTR forwarding.

`bootstrapResolverFrom` accepts the same provider references. Its typed endpoints
keep original deadlines and WAN scopes. Each pool retains one Go resolver for
its lifetime and binds bootstrap traffic to `viaInterface` too. When declared
bootstrap sources are absent/expired, new hostname lookups fail rather than
using the host resolver. Existing HTTP/TLS connections remain ordinary transport
connections; DNS expiry alone does not revoke them. Status exposes
`bootstrapResolvers` separately from upstream health. The deadline-aware socket
preserves UDP's `net.PacketConn` contract, which Go uses to select DNS framing.

The input watcher uses half the remaining original authority for its polling
interval, clamped between 50 ms and one second. A failed poll
leaves the old deadline in force. It does not renew the source.

The coordinator prepares provider/target bindings once per configuration and
decodes each referenced provider once per input view. Each source's provider
order remains its own; sharing one provider does not reorder another source.
The worker's IPC boundary validates a complete input before applying it.
`make check-uplink UPLINK_INTERFACE=... UPLINK_AFTR=... UPLINK_SECONDS=...`
uses a local authorized access network with a separate MAC and LAN. Set
`UPLINK_DNS_UPSTREAM=https://cloudflare-dns.com/dns-query` to exercise learned
bootstrap against that independent DoH implementation. This does not qualify
physical hardware or every encrypted-DNS transport.

Dynamic listen/record references, local DNSSEC and all remaining
resource fields still require implementation and interoperability qualification.

A source can explicitly forward the configured tailnet domain to the local
Tailscale resolver with `TailscaleNode/NAME` and `field: dnsServers`:

```json
{
  "name": "tailnet",
  "kind": "forward",
  "match": ["your-tailnet.ts.net"],
  "viaInterface": "tailscale0",
  "upstreamFrom": [{"resource": "TailscaleNode/tailnet", "field": "dnsServers"}]
}
```

`NAME` is the native Tailscale service name. The fixed daemon handles MagicDNS
at `100.100.100.100` while its OS DNS management remains disabled. The
coordinator supplies that endpoint only for a Running/Online observation and
retains its original boot deadline, capped by the applied service authority.
A stopped, expired or revoked producer contributes no endpoint. Scope the
source to the intended tailnet domain; this is not automatic installation of
control-server global/split resolvers or search domains. Host DNS and other
native sources keep their own configuration. Allow LAN UDP/TCP 53 explicitly
when the native INPUT policy drops other traffic.

## Query history retention

The resolver service can set `query_log_retention` with `max_age_seconds`,
`interval_seconds` (default one day), `vacuum` and UDP `syslog` destinations.
It requires query logging. The existing DNS worker owns both recording and
retention; no service, daemon or second history file is added. The old routerd
`logstore.ApplyRetention` supplies expiry, before-delete archive and vacuum.
Maintenance uses a separate SQLite connection without holding the query-writer
lock. DNS answers continue when archival submission fails; expired rows remain
for the next sweep. A UDP write is not an acknowledgement by the destination,
and a failed sweep can retransmit rows already submitted before that failure.
The resolver status exposes `queryRetentionDeleted` and `queryRetentionError`.

`LogRetention` imports `dnsQueries` into each query-logging resolver using its
configured age, daily schedule, archive destinations and vacuum setting. Old
query-log filenames are deliberately relocated below each native service's
state directory, with the change recorded in the migration mapping; existing
legacy database contents are not read or imported. Event retention remains
with its existing owner. Other unimplemented history datasets are still
reported as unsupported rather than silently receiving event retention.

`make check-dns-retention` runs the actual product in private namespaces with
an independent UDP receiver and real UDP/TCP `dig` queries. It imports a legacy
DNS/LogRetention document, uses a short natural-expiry window, checks original
archived columns and deletion, and makes only the log destination unreachable.
It verifies retained rows and continuing DNS/forwarding during that failure,
recovery, resolver restart, and removal of the retention policy. Temporary
stages are removed after success. This is protocol/OS acceptance, not long-run
storage-load or physical-device qualification.
