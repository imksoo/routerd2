# LAN DHCPv6 information service

`services[].dhcp6_server` owns one device-bound UDP6 socket on port 547.
It answers Information-request with DNS (23), domain search (24), SNTP (31)
and Information Refresh Time (32). Unknown optional input is ignored. A client
identifier is echoed when present; anonymous Information-request is accepted.
The server sends unicast Reply to client port 546 and waits for normal link-local
DAD before opening service. It does not assign addresses or emit RA.

```json
{"name":"lan-info","dhcp6_server":{
  "interface":"lan0",
  "options_from":["wan-pd"],
  "domain_search":["home.arpa"],
  "refresh_seconds":600
}}
```

`options_from` names configured DHCPv6 or RA clients. Static `dns_servers`,
`sntp_servers` and `domain_search` precede learned values. Each option supports
32 static and 32 distinct learned values. Learned values keep their original
boot deadlines; each request rechecks expiry after receiving the datagram.
WAN link-local addresses are not advertised on a different LAN. Source failure,
expiry or disappearance does not remove unrelated static options. Polling does
not renew authority. Clients cache received options independently; the refresh
interval (minimum 600 seconds, default 86400) does not provide immediate
revocation of a previously received option.

Configure LAN addressing, forwarding and RA separately. For SLAAC clients using
DHCPv6 information, advertise `other_config: true`. A delegated LAN can instead
use its PD-generated RA with `delegations[].other_config: true`. The server requires no pool and never writes learned
DNS or domains back into operator configuration.

The server DUID is persisted atomically before serving and reused after worker,
control or daemon restart. Explicit `server_duid` selects a configured identity;
otherwise the first start generates a DUID-UUID. Its private state is not a lease
budget and remains usable across reboot. The corresponding WAN client retains
its own DUID/IAID and lease ownership separately.

This follows the useful behavior of routerd's `pkg/render/dnsmasq.go` and
`pkg/controller/chain/phase2.go` stateless serving path, and its persistent
DUID/identity handling. routerd used the established dnsmasq server; its
`pkg/pdclient/message.go` is an IA_PD client codec, not a LAN server codec.
No discarded routerd2 format readers or prototype adapters are present.

`make check-dhcp6-server` runs the actual supervised product against BusyBox
`udhcpc6 -l`, captures packets, and checks static options and DUID preservation
through worker/control replacement and whole-daemon restart. `make check-dhcp6-ra-pd DHCP6_SERVER=/path/to/isc/dhcpd` connects independent ISC DHCP, radvd and Linux
SLAAC peers, then BusyBox on the delegated LAN, including UDP/TCP DNS and option
expiry while control is paused. Both fixtures use normal DAD
and discard raw logs/captures/binaries after inspection.

Stateful address/prefix pools, Rapid Commit, DHCPv6 relay, client Reconfigure,
legacy resource import, HA activation and full hardware qualification remain
outside the implemented stateless path.
