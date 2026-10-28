# Native DNS worker

The worker reuses routerd's resolver, upstream transports and static-zone
behavior. `Run(ctx, cfg, input, ready)` owns its listeners and pools until
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
