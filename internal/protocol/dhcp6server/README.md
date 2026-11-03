# LAN DHCPv6 service

`services[].dhcp6_server` owns one device-bound UDP6 socket on port 547.
It answers Information-request with DNS (23), domain search (24), SNTP (31)
and Information Refresh Time (32). Unknown optional input is ignored. A client
identifier is echoed when present; anonymous Information-request is accepted.
The server sends unicast Reply to client port 546 and waits for normal link-local
DAD before opening service. It does not assign addresses or emit RA.

```json
{"name":"lan-info","dhcp6_server":{
  "interface":"lan0",
  "dns_from":["wan-pd"],
  "search_from":["wan-ra"],
  "domain_search":["home.arpa"],
  "refresh_seconds":600
}}
```

`dns_from` and `search_from` independently name configured DHCPv6 or RA
clients. `sntp_from` names DHCPv6 clients, since RA carries no SNTP option.
An unselected option is never copied merely because the same peer provides it.
Each source list retains its configured order; one shared provider is decoded
once per input read. Static `dns_servers`, `sntp_servers` and `domain_search`
precede learned values. Each option supports
32 static and 32 distinct learned values. Learned values keep their original
boot deadlines; each request rechecks expiry after receiving the datagram.
WAN link-local addresses are not advertised on a different LAN. Source failure,
expiry or disappearance does not remove unrelated static options. Polling does
not renew authority. Clients cache received options independently; the refresh
interval (minimum 600 seconds, default 86400) does not provide immediate
revocation of a previously received option.

Configure LAN addressing, forwarding and RA separately. For SLAAC clients using
DHCPv6 information, advertise `other_config: true`. A delegated LAN can instead
use its PD-generated RA with `delegations[].other_config: true`. The server
requires no pool and never writes learned DNS or domains back into operator configuration.

The server DUID is persisted atomically before serving and reused after worker,
control or daemon restart. Explicit `server_duid` selects a configured identity;
otherwise the first start generates a DUID-UUID. An explicit override belongs
to the configuration journal and does not overwrite that automatic identity;
confirmed rollback restores the previous selection. Its private state is not a lease
budget and remains usable across reboot. The corresponding WAN client retains
its own DUID/IAID and lease ownership separately.

A service resource can use `when: {fact: service.ha.active}` for an existing
VRRP owner. Configure the same explicit server DUID on both nodes when they
represent one service. The backup has no serving worker. The applied resource
selection supplies its original activation deadline to the socket owner,
including dependency readiness bounds. Input polling never extends that grant.
Each request rechecks it after the receive wait, so stopped control cannot keep
an old conditional server serving on a permanent LAN interface. Unconditional
configuration remains authorized for the current boot. Learned option expiry
and permission to serve are independent.

The snapshot reports the persisted server identity. Process/socket readiness
belongs to the supervisor; it does not assert current permission to answer.
There is no duplicated mutable `active` flag or final status-only stop RPC.

This follows the useful behavior of routerd's `pkg/render/dnsmasq.go` and
`pkg/controller/chain/phase2.go` stateless serving path, and its persistent
DUID/identity handling. routerd used the established dnsmasq server; its
`pkg/pdclient/message.go` is an IA_PD client codec, not a LAN server codec.
No discarded routerd2 format readers or prototype adapters are present.

`make check-dhcp6-server` runs the actual supervised product against BusyBox
`udhcpc6 -l`, captures packets, and checks static options and DUID preservation
through worker/control replacement, whole-daemon restart and confirmed rollback. `make check-dhcp6-ra-pd DHCP6_SERVER=/path/to/isc/dhcpd` connects independent ISC DHCP, radvd and Linux
SLAAC peers, then BusyBox on the delegated LAN, including UDP/TCP DNS and option
expiry while control is paused. It also selects PD DNS and independent radvd
search domains without copying PD SNTP/search, observes actual Reply convergence
within a five-second confirmed trial, and restores the original policy on
rollback. Commit acceptance does not mean the periodic worker input update has
already completed. Both fixtures use normal DAD
and discard raw logs/captures/binaries after inspection.

`make check-ha-pd DHCP6_SERVER=/path/to/isc/dhcpd` checks actual two-node
activation and DNS serving alongside synchronized PD and connection state. A
direct UDP request targets the paused old node separately from the legitimate
new master; multicast BusyBox responses cannot establish that distinction.

Prefix delegation pools, DHCPv6 relay, client Reconfigure,
wider HA partition policy and full hardware qualification remain
outside the current implementation.


## Address pools

A `pool` on `dhcp6_server` selects the existing bundled dnsmasq engine for
address assignment. Without a pool the native information service above remains
in use. Bind `engines.dhcp6_server` to the same `dnsmasq-engine.tar` used for
DHCPv4 in the boot configuration, or the corresponding signed catalog role.
The factory boot configuration includes both role bindings. Packaging retains
one copy of that engine and its corresponding sources and notices.

```json
{"name":"lan-addresses","dhcp6_server":{
  "interface":"lan0",
  "pool":{"start":"::100","end":"::1ff","lease_seconds":43200},
  "dns_servers":["2001:db8:80::1"],"domain_search":["lan.test"]
}}
```

Suffix ranges follow the usable prefix installed on the interface using
routerd's established dnsmasq `constructor` behavior. Full IPv6 ranges select
a fixed prefix. The prefix length defaults to 64; dnsmasq supports lengths
64 through 128. Configure the interface, routing and RA separately: use the
RA managed flag for DHCPv6 addresses, and enable autonomous prefixes as well
when clients should use both DHCPv6 and SLAAC. This engine never emits RA.
Learned options and serving permission retain their original deadlines; changes
restart the external engine with its lease file retained. Allocation, renewal,
release and lease persistence are owned by dnsmasq, not another Go allocator.
The existing atomic DUID identity is installed in the engine's lease file
before it serves and is retained through worker and daemon restart.

`make check-dhcp6-server` exercises the information path with BusyBox and the
address-pool path with independent dhcpcd,
real product processes and packet capture. The address-pool check uses the
client's actual assigned address for TCP/UDP, renewal and post-restart checks;
it also compares the server identifier in actual packets with the persisted
DUID. The dhcpcd client automatically configures both families from DHCPv4/v6
and the product RA. Queries use the distributed DNS addresses over IPv4/IPv6
UDP/TCP, including same-name A/AAAA, both PTRs and hostname renewal.

A DNS zone can consume `DHCPv6Server/<service-name>` alongside DHCPv4 sources.
The worker observes the actual engine lease file; the engine remains the sole
allocation owner. Each address retains its original lease expiry and finite
source observation deadline. Adding AAAA preserves A for the same name, and
expired addresses disappear independently. Constructor pools publish bindings
only under current interface prefixes; a retained old-prefix lease row is
allocation history, not a usable DNS address. Ancillary publication failure does
not stop allocation; an unrefreshed DNS observation expires.

The same check exercises a configured LAN prefix change through natural
client renewal, original lease expiry without Release, independent IPv4 DNS
retention and reacquisition. Stateful lease synchronization, upstream-PD-driven
renumbering/expiry, Reconfigure and complete deployment qualification remain open.
