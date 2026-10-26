# Native DHCPv4 relay

The `dhcp_relay` service relays Ethernet BOOTP/DHCPv4 between explicitly declared
client interfaces and their upstream IPv4 servers. It keeps no leases, transaction
map, retry queue or persistent protocol state. A client interface has a committed
local IPv4 address used as `giaddr`; its address and interface resources are
automatic dependencies of the worker.

```yaml
schema_version: 2
resources:
  - id: clients
    interface: {name: lan0}
  - id: gateway
    address: {interface: lan0, cidr: 192.0.2.1/24}
  - id: relay
    service:
      name: relay
      dhcp_relay:
        interfaces:
          - name: lan0
            address: 192.0.2.1
            servers: [198.51.100.2]
```

Declare the upstream interface/routes separately. The server needs a return
route to the client subnet through this router. Normal client traffic also needs
explicit forwarding, routes and any firewall permissions. No network state or
firewall permission is implicitly adopted.

One relay worker owns UDP 67 per routing domain. `vrf` selects a declared VRF;
client interfaces and their addresses must belong to that domain. Native DHCP
servers may run on other interfaces. Configuration and namespace-scoped socket
claims reject overlapping client listeners. Up to 32 client interfaces and eight
upstream servers per interface are supported; requests go to every listed server.

Following [RFC 1542 section 4.1](https://www.rfc-editor.org/rfc/rfc1542.html#section-4.1),
requests increment `hops` and fill an empty `giaddr`, preserving other bytes.
`max_hops` defaults to four and may be 1..16; requests with a larger received
`hops` value are discarded. Replies must come from a server
configured for their `giaddr`. The original reply payload is preserved. Delivery
uses the broadcast flag or client `yiaddr`/hardware address; DHCPINFORM ACKs use
`ciaddr`. Checksummed Ethernet output reaches clients before they can answer ARP.
The worker exits when its interface identity, VRF, readiness or local address
changes. The supervisor owns process recovery.

DHCP options are opaque; the relay does not insert Option 82.
Input datagrams are bounded at 4096 bytes. Client replies must fit the Ethernet
MTU without fragmentation. Oversized reply fragmentation,
HA relay failover and VRF interoperability qualification remain unfinished.

`make check-dhcp-relay` requires Linux user/network namespaces, root or
passwordless sudo, dnsmasq and BusyBox with udhcpc. It runs actual product workers,
two independent client namespaces and an independent server namespace. It checks
both scopes, unicast/broadcast delivery, DHCPINFORM, the hop limit, worker recovery,
forwarded TCP/UDP and captures on both LANs and the WAN.
