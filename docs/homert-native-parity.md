# Homert native profile and feature mapping

## Scope

The saved homert02 and homert03 router files were parsed read-only for resource kinds and reference shape. Each contains 109 resources, and both have the same per-kind counts and the same extracted typed dependency edges. No configuration values, addresses, identities, credentials, or keys were copied into this repository.

`examples/homert02-native.yaml` and `examples/homert03-native.yaml` are sanitized ResourceProgram examples using documentation-only addresses. They show a single-router DHCP/DNS/PD/RA/DS-Lite path and its VRRP/lease-replication peer settings. They are schema examples, not a translation of either saved file, not suitable for installation, and not evidence of HA or client interoperability. In particular, the DHCPv6 server lease-sync resource has no equivalent peer-replication field in the current native DHCPv6 server contract.

## Feature mapping

The counts below are resource-kind counts observed in each saved file; corresponding counts were equal between the two files. A row describes the closest current ResourceProgram area, not automatic one-to-one conversion.

| Saved resource kinds (count) | Current routerd2 representation | Status / unresolved point |
| --- | --- | --- |
| `Interface` (10), `IPv4StaticAddress` (7), `IPv4Route` (4) | `interface`, `address`, `route`, `host_route`, or `egress_route` | Core intent is representable. Exact route type, ownership, source, and interface values were not retained. |
| `IPv6DelegatedAddress` (5), `VirtualAddress` (2) | `service.dhcp6.delegations`; `service.vrrp` addresses and VIP interface | The native example keeps LAN PD on the physical LAN link and the IPv4 VIP on its VRRP-owned interface. Exact active/standby address roles still need real-peer acceptance. |
| `DHCPv4Server` (1), `DHCPv4Reservation` (2), `DHCPv4ServerLeaseSync` (1) | `service.dhcp_server` scopes, reservations, and `ha` TLS replication | The contract exists; the example keeps the DHCP worker present on standby and assigns serving ownership through `ha.owner`. Real lease transfer and client renewals remain to be demonstrated for this profile. |
| `DHCPv6PrefixDelegation` (1), `DHCPv6PrefixDelegationLeaseSync` (1), `DHCPv6Information` (1) | `service.dhcp6`, `dhcp6.ha`, delegated addresses, RA client facts, and their consumers | Native PD state replication and learned information are represented. The saved DUID/IAID and peer behavior were not copied or qualified here. |
| `DHCPv6Server` (1), `DHCPv6ServerLeaseSync` (1), `IPv6RouterAdvertisement` (1) | `service.dhcp6_server` and `service.ra` | RA and stateless DHCPv6 are representable. There is no DHCPv6 server lease-sync field in the current native service; a shared configured server DUID is not proof that old server-state synchronization is preserved. |
| `DNSResolver` (2), `DNSForwarder` (10), `DNSUpstream` (13), `DNSZone` (1) | `service.dns` listeners, ordered suffix sources, upstreams, zones, and DHCP-derived records | Conditional forwarding and DoH have native examples. Saved listener address dependencies and per-source details still need review against the full value-level configuration before claiming parity. |
| `DSLiteTunnel` (4), `EgressRoutePolicy` (3), `HealthCheck` (6) | `service.ds_lite`, `egress_policy`/`egress_route`, and `service.health` | The native types cover the function classes. The exact candidate set, tunnel dependencies, selection policy, MTU and recovery behavior have not been reproduced from saved values. |
| `NAT44Rule` (7), `NAT44SessionSync` (1) | `nat` resources and `service.vrrp.conntrack_sync` | Both native mechanisms exist. Actual source/egress matches and state-sync exclusions require value-level review and packet verification. |
| `FirewallZone` (3), `FirewallPolicy` (1), `FirewallRule` (2), `IPAddressSet` (2), `LocalServiceRedirect` (1), `FirewallEventLog` (1) | `firewall` rules, address sets, local redirects, and `service.firewall_log` | Native primitives exist, but there is no established one-to-one zone/policy translation. Do not replace these with a guessed rule set. |
| `BGPRouter` (1), `BGPPeer` (1) | `service.bgp` | Native BGP service supports the feature class; router/peer values and actual route exchange still require review and a real independent peer. |
| `NTPClient` (1), `NTPServer` (1) | `service.ntp` sources, listeners, and access prefixes | The native time service has client and server intent. Exact source selection and serving ACL were not copied. |
| `TailscaleNode` (1) | `service.tailscale` | Native service exists; identity and enrollment state must remain separately provisioned. No identity data was read into this mapping. |
| `LogSink` (2), `Telemetry` (1), `LogRetention` (1), `TrafficFlowLog` (1), `WebConsole` (1) | `service.otlp_logs`/`syslog`, retention settings, `service.traffic_flow`, and `service.web_console` | Typed service areas exist. The sink-to-telemetry reference and retention policy need exact value-level mapping. |
| `ManagementAccess` (1) | No matching ResourceProgram payload identified | Unmapped. Keep the management-access behavior explicit in the management/startup contract; do not infer it from the web console. |
| `Package` (1) | Image/build package manifest, outside ResourceProgram | Unmapped at runtime by design. Identify the required build package separately; do not add a generic package-execution resource. |
| `Sysctl` (1) | Typed `forwarding` fields only where semantics match | Unmapped until its key and desired value are reviewed. There is no generic arbitrary-sysctl resource. |

## Extracted dependency shape

The two files expose the same dependency categories. Resource identities and scalar values are deliberately omitted.

- A BGP peer references its BGP router. DHCPv4 lease sync references its server; DHCPv6 prefix sync references its PD client; DHCPv6 server sync references its DHCPv6 server.
- DHCPv6 delegated addresses depend on the PD client. The PD and learned-information resources then feed LAN RA, DHCPv6 information, DNS upstream/bootstrap selection, DS-Lite source selection, and NTP sources.
- DS-Lite tunnel resources depend on delegated IPv6 and local IPv4 addressing. Egress candidates refer to WAN/tunnel devices and health conditions; their dependencies also include firewall-zone resources.
- DHCPv4 gateway/DNS and NAT source addresses refer to virtual addresses. Local redirects refer to address sets. DNS listeners and records draw from interface, static, delegated, and virtual address resources.
- Some legacy references use names that occur under more than one resource kind. Because this pass intentionally did not retain scalar values, those references remain ambiguous here and must be resolved before producing a complete native profile.

## Values to replace before any isolated deployment

The examples use documentation networks and generic interfaces. A repeatable isolated run must explicitly substitute the test link names, WAN peer/AFTR behavior, LAN prefix and lease pool, stable DHCP client and server DUIDs, VRRP source/VIP/peer values, TLS certificate paths and provisioned certificates, DNS conditional upstream, and measured MTU/MSS policy. None of the example certificates or private keys exist; the paths are only schema values. Do not connect these documentation addresses to a live LAN.

## Validation boundary

Both examples were accepted by `routerd config validate`. That verifies ResourceProgram decoding and compile-time contracts only. It does not verify worker startup, physical WAN/ONU behavior, DHCP exchanges, client DNS/internet access, or HA failover. Those checks must use the actual routerd2 processes, kernel, an independent client/peer, and the isolated real-line fixture before these profiles can be treated as usable.
