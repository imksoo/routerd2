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

`internet` names a configured PPP service. Static `upstreams` may coexist with
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

Only PPP upstream references are implemented here. DHCPv4/DHCPv6/RA upstream
references, dynamic bootstrap/listen references, local DNSSEC and all remaining
resource fields still require implementation and interoperability qualification.
