# Strict migration of original routerd resources

This package is a pure compiler from the Linux resource schema at
**imksoo/routerd commit 1329ef54f296507eda79bbbc3b853783dd27251e** to
the schema-2 ResourceProgram (`config.Document`). It neither invokes the original controllers nor runs commands,
reads host state, contacts an upstream, modifies a network, or imports a lease
database. It is not a compatibility implementation of every original resource.

The output is the new source graph, with separate interface and address resources.
Review mapping targets beginning with `network.*` or `services/*` describe the
derived engine input; `routerd config plan FILE` shows its new resource ownership.
This importer reads the original **routerd** product, not a previous routerd2 prototype format.

## API

```go
document, err := legacyconfig.Migrate(inputYAML)
result, err := legacyconfig.Compile(inputYAML)

// Deliberate native replacements for legacy WAN and DHCP-derived DNS behavior:
options, err := legacyconfig.DecodeOptions(policyReader)
result, err = legacyconfig.CompileWithOptions(inputYAML, options)
```

`Compile` additionally returns the original Router name, source revision, and
one `Mapping` per accepted resource. A mapping records native targets and any
structural changes. Service identities are deterministic, bounded names derived
from the original kind/name. Cross-kind name collisions therefore do not silently
merge services.

`Compile` never guesses a host network manager's RA defaults or silently adopts
the new AFTR or DHCP-derived DNS lifetime contracts. `CompileWithOptions` permits
**explicit replacement intent** for individual `IPv6RAAddress`, `DSLiteTunnel`,
DHCP-consuming `DNSResolver`, stateless `DHCPv6Server`, `HealthCheck`, VRRP `VirtualAddress`, and `TailscaleNode` resources. Each such mapping says which behavior
changes; a successful replacement is not a claim of
automatic semantic parity. There is no global permissive mode. Policy cannot
discard an unsupported field, resource, HA condition or ownership requirement.

Every error is a `*UnsupportedError` with `Issues []Issue`. An issue has `Path`,
`Code`, `Message` and, where available, `Line`. Diagnostics do not echo scalar
input values or the raw errors from YAML/native validators, which can contain
credentials. Unknown fields in the original schema and known fields whose
semantics cannot be represented are different errors.

**Any issue returns a zero Document/Result.** There is no partial document to
apply, no permissive fallback, and no option to ignore incompatible resources.
The CLI can print the list for an operator to resolve. A successful result still
needs the normal native candidate/validation/commit process; this compiler never
adopts live resources from the old daemon.

The CLI entry point is `routerd config migrate [--policy FILE] FILE`. It emits native YAML on
success. Code that also needs the mapping report should use `Compile`.

## VRRP election and VIP identity

Basic `VirtualAddress` VRRP elections retain the original Keepalived defaults:
IPv4 uses v2, IPv6 uses v3, advertisements are one second, priority defaults to
100, and omitted preempt remains **false**. Declared peers and priority are
retained. IPv4 PASS accepts the native 1..8 printable-byte password contract;
secret-provider references remain unsupported.

The native engine owns a separate VIP VMAC rather than putting the VIP directly
on the physical interface. Supply explicit local identities for that change:

```yaml
schema_version: 1
vrrp:
  VirtualAddress/gateway:
    source_address: 192.0.2.2
    vip_interface: ha
```

The election interface must be independently managed/admin-up and the source
must be available there. An IPv6 policy also selects
`vip_link_local_address: fe80::42/64` for the first virtual address. VIP literals
accept a host CIDR or bare IP and are normalized. The original interface's zone
ACL also covers its new VIP VMAC; the original family-scoped protocol112 service
exception remains on the election interface. No global INPUT accept is added.

Runtime `when`/dependencies, tracking, graceful readiness, additional failover
identities, dynamic VIPs and hostname publication remain unsupported by this
conversion. The complete batch rejects those declarations rather than erasing
them. Native implementations of these functions have separate acceptance;
basic migration is not full homert/cluster import.

`make check-vrrp-unicast RELEASE_ID=vrrp-import` includes actual CLI-converted
IPv4 v2/PASS and IPv6 v3 graphs. Independent Keepalived, real workers and capture
check VIP TCP/UDP, takeover, worker replacement and recovery. The imported INPUT
policy permits the declared application ports and denies another port with a
real listener on the VIP VMAC.

## Scoped HealthCheck probes

Literal unicast targets use the existing routerd probe and threshold engine.
The importer retains protocol, port, thresholds and the original omitted
interval/timeout defaults of 30s/3s. Native health defaults differ, so these
values are explicit in the generated configuration. Legacy `role` describes
the check; it does not become a native activation condition.

An unscoped legacy probe needs explicit routing intent so a failed WAN cannot
appear healthy through another WAN. The interface must be independently managed
and administratively enabled. An existing legacy interface must agree with the
policy. Source address and mark are optional explicit probe settings.

```yaml
schema_version: 1
health_checks:
  HealthCheck/wan-probe:
    interface: Interface/wan
    source_address: 192.0.2.2
    mark: 42
```

The example source and mark must match the intended address and probe route.
Conditional activation, hostname/automatic targets and live gateway/DS-Lite
target discovery remain unsupported; they reject the whole migration rather
than losing their meaning. Disabled checks create no healthy observation.
`make check-health-pbr RELEASE_ID=health-import` feeds the actual CLI-converted
probes into the two-WAN daemon fixture, alongside native routing resources.
That does not establish import of legacy `EgressRoutePolicy` or HA conditions.

## Stateless DHCPv6 and delegated LAN RA

The active routerd chain enables dnsmasq RA together with its stateless DHCPv6
range. Importing only UDP/547 would lose SLAAC and DNS for RA-only clients.
The explicit policy replaces the interface-prefix constructor and unbounded
retained information with one managed PD allocation and original lease/option
budgets. Static values remain independent when learned information expires;
missing information no longer stops all serving. This is deliberate native
replacement behavior, not byte-for-byte dnsmasq parity.

```yaml
schema_version: 1
dhcp6_servers:
  DHCPv6Server/lan-info:
    delegated_address: IPv6DelegatedAddress/lan-address
    router_lifetime_seconds: 30
    server_duid: "00030001020000000042"
```

The reference must select the serving interface's compiled allocation. A declared
`delegatedAddress` or `defaultRoute` must agree with it. Delegated-address
`sendRA`/`announce` declarations require this explicit selecting policy; the
selected RA implements that intent. Router lifetime must be
zero or 10..9000 seconds. PIO and learned DNS lifetimes retain their original
PD/information deadlines; `leaseTime` overrides are not imported. Static
`dnsServers`, `sntpServers`, `domainSearch` and exact PD/Information
`dnsServerFrom`, `sntpServerFrom`, `domainSearchFrom` are converted. RA DNS/search
references must use this selected PD; native serving can independently select
other providers, but the old implicit RA conversion does not invent that graph.
No DNS declaration means no learned RDNSS, while search and SNTP remain usable.
Set `server_duid` to the existing server's DUID when that identity must survive
the move, or use the same explicit DUID on HA partners. This is operator intent,
not an import of the old dnsmasq lease database. When omitted, the native server
creates and retains its own automatic identity. An explicit override never
overwrites that automatic identity; confirmed rollback restores its selection.
The example DUID is fixture data and must be replaced with the intended identity.

WAN identity is independent: `DHCPv6PrefixDelegation.spec.clientDUID` is copied
into the native client. Set it to the old client's observed DUID when changing
NICs or moving to a new state directory. Without it, a new Ethernet client starts
with DUID-LL from its MAC, then retains that value across MAC changes and
restarts. Lease deadlines and server bindings are not transferred by selecting
an identity, and the upstream decides which prefix it grants.

The shared synthetic fixture runs through the actual product CLI:

```sh
dist/bin/routerd config migrate \
  --policy internal/integration/testdata/dhcp6-pd-import-policy.yaml \
  internal/integration/testdata/dhcp6-pd-import.yaml > /tmp/imported-router.yaml
dist/bin/routerd config plan /tmp/imported-router.yaml
make check-dhcp6-ra-pd DHCP6_SERVER=/path/to/isc/dhcpd RELEASE_ID=dhcp6-import
```

The real communication fixture applies that CLI result, adds only its bridge/
duplicate-host wiring and the explicit native RA-triggered IA_NA scenario, then
uses independent ISC/radvd/BusyBox and a Linux SLAAC client. Its LAN INPUT ACL
drops all other traffic to self: UDP/547 is the migrated service exception.
It checks actual RA RDNSS/DNSSL and finite DNS lifetimes, DHCPv6 options,
TCP/UDP and DNS, expiry/recovery and confirmed rollback. The source fixture's
IAID is preserved in packet assertions. Conditional HA, stateful/both/RA-only
serving, custom lease/RA settings and full deployed-router import remain open.

## Implemented conversions

| Original declaration | Native intent and restrictions |
| --- | --- |
| `Interface` | An explicitly `managed: true`, `adminUp: true` interface owned by routerd becomes a native Interface, retaining MTU. External/observe-only aliases remain reference-only and are recorded in the report. Address or master attachment to such an alias is rejected because native interface management brings a link up. |
| `Link` | Existing-link alias only; no link type, ownership or administrative state is invented. |
| `VRF` | A native VRF link with the same route table and explicit member masters. Members must independently authorize administrative management. |
| `Bridge` | A native bridge with explicit members, MAC, MTU and the original multicast-snooping default of false. Requires `stp: false` and `rstp: false`. Custom timers are rejected; dormant timing attributes use kernel defaults. |
| `WireGuardInterface` + `WireGuardPeer` | Static interfaces with explicit private-key file paths and complete peer sets. Preserves MTU, numeric/scoped endpoints, passive peers, keepalive, AllowedIPs and explicit routes without inferring routes or marks. Fixed listen ports require an owned INPUT filter so the original service-hole policy can be preserved; see the static WireGuard section. |
| `IPv4StaticAddress` | Preserves host/prefix bits. `exclusive: true` is rejected because it removes addresses owned by other managers. Explicit overlap policy and its justification are checked. |
| `VirtualAddress` in static mode | IPv4 /32 or IPv6 /128, including a provably static resource address reference. Runtime tracking, automatic hostname publication and gratuitous-ARP intent are rejected. |
| `VirtualAddress` in VRRP mode | Explicit `vrrp` policy selects local election and VIP VMAC identities. Retains IPv4 v2/IPv6 v3, one-second advertisements, priority, peers and the original non-preempt default. Zone ACLs follow the VIP VMAC while protocol112 exceptions remain family/interface scoped. Conditions, readiness, tracking, additional identities and secret-provider references remain unsupported. |
| `IPv4StaticRoute`, `IPv6StaticRoute` | Same destination, gateway, device and metric; interface aliases are resolved. |
| `IPv4Route` | Static unicast routes. A static DS-Lite device reference and explicit static address gateway reference can be resolved. Blackhole, preferred-source and dynamic references are rejected in this compiler. |
| `Sysctl` | Required runtime `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` with exact 0/1 values. No implicit forwarding is enabled. Other sysctls, optional/non-exact policies and persistent host files are rejected. |
| `HealthCheck` | Literal scoped ICMP/TCP/DNS/HTTP probes retain routerd's engine, thresholds and 30s/3s timing defaults. Explicit `health_checks` policy supplies missing interface/source/mark intent. Conditional activation and dynamic targets remain unsupported. |
| `DHCPv4Client` | Native wire identity, requested address, hostname/class/client ID, default-route policy and metric. Requires `useDNS: false` because the old default of true changes the host resolver. Native learned DNS is status data rather than a substitute for that side effect. |
| `DHCPv4Server` + `DHCPv4Reservation` | Static local servers become separate native DHCP workers with owned journals. Retains finite pool leases, disjoint MAC/OUI profile scopes, explicit/static-source gateway/DNS/NTP/domain options, and MAC/IP/hostname reservations with typed options. Requires one owned static IPv4 address per interface; no live lease import, HA conditions or ambiguous backend directives. The active-chain/default distinctions are detailed below. |
| `DHCPv4Relay` | Explicit `interfaces` and numeric IPv4 `upstream` become one native relay service. Each client interface requires one unconditional owned static IPv4 address for `giaddr`. Multiple relay declarations retain per-interface upstream sets. Conditional/dynamic source selection and ambiguous legacy VRF binding remain unsupported. |
| `DHCPv6PrefixDelegation` | Native IA_PD service, pinned DUID-LL/IAID 1 defaults, explicit DUID, provider profile and prefix-length preference. Explicit external clients and required-prefix readiness semantics are rejected. |
| `DHCPv6Information` with one matching PD | An alias of that PD service, sharing its DUID/IAID, UDP 546 socket and supplementary Information-Request. The original **literal interface reference** must match exactly; merely resolving two different aliases to the same NIC does not reproduce the old association. Standalone, ambiguous and nonempty `request` declarations are rejected. |
| `DHCPv6Server` in stateless mode | Explicit `dhcp6_servers` policy selects one compiled PD LAN allocation and router lifetime. Imports static DNS/SNTP/search and PD/information references, generates RA with O plus independently selected RDNSS/DNSSL, and preserves the original untrust UDP/547 INPUT exception. The RA DNS sources must be the selected PD; unsupported fields and HA conditions reject the entire input. |
| `IPv6RAAddress` with explicit policy | One native `ra_client`, using the policy's same-interface SLAAC/default-route/on-link/RIO/DNS flags, source/IID/router filters and explicit route table/metric. Requires managed RA, independently managed/admin-up underlay, and `required: false`. Host defaults and learned state are not imported. |
| `IPv6DelegatedAddress` | A /64 LAN address or /128 host endpoint under its PD service, retaining the installed mask, suffix and **hexadecimal** subnet ID. In the old schema, `subnetID: "10"` means hexadecimal 0x10, not decimal 10. IDs must fit within the provider prefix; zero host ID remains unsupported. LAN advertisement declarations require a `dhcp6_servers` policy selecting that /64 allocation. |
| `DSLiteTunnel` | Fixed outer IPv6 endpoints, inner IPv4 /32 **peer 192.0.0.1/32**, explicit tunnel identity, no encapsulation-limit option and original fixed default MTU of **1460**. The default inner address is 192.0.0.2. `localAddressFrom` means the **inner IPv4** source in the pinned controller, not the outer IPv6 endpoint. |
| `DSLiteTunnel` with explicit policy | One native `ds_lite` service, preserving candidate precedence and B4/MTU identity while explicitly changing DNS bootstrap, source ownership/selection and endpoint expiry. Accepts an owned static IPv6 source or the policy's compiled RA source. Information references map to their existing PD service. One independently declared IPv4 default route can join the dynamic tunnel's lifetime owner. PD-derived/HA/suffix sources remain rejected. |
| `DNSResolver` + `DNSForwarder` + `DNSUpstream` + `DNSZone` | Resolves the static resource graph into native DNS runtime intent. Preserves source order, listener selection, transport/TLS identity, bootstrap servers, interface binding, cache/timeouts/health checks, query-log enablement, zones and static records/reverse zones. Statically known address references are resolved; learned/delegated references are rejected. DHCP-derived zones require successfully migrated DHCP producers and an explicit per-resolver `dhcp_dns` replacement policy. |
| `FirewallZone` + `FirewallPolicy` + `FirewallRule` | Native INPUT/FORWARD hooks with the original conntrack precedence, loopback/ICMPv6 acceptance, explicit ordered ACLs and role-based terminal actions. Retains source/destination CIDRs, TCP/UDP port sets/ranges, ICMP types, accept/drop/reject and unsampled kernel logging. Does not invent an OUTPUT hook. Overlapping zones, dynamic sets, rate/connection limits and NFLOG are rejected. |
| `NAT44Rule` in standard form | Explicit `type: masquerade` or `snat`, `egressInterface`, `sourceRanges`, positive destination CIDRs and destination exclusions. Resolves provably static SNAT address references. NAT-only configurations retain **no filter hook**. Exclusions are compiled to bounded positive CIDR sets. |

DS-Lite retains the old tunnel-name fallback order: `tunnelName`, then the literal
`spec.interface` value, then the resource name. MTU 1460 is retained even when the
underlay MTU differs from 1500; substituting underlay-minus-40 would change the old
configuration. The pinned **Linux** tunnel controller does not apply
`defaultRoute`/`routeMetric` itself; the migration rejects those settings instead
of inventing routes. Declare an independent `IPv4Route`. For a policy-managed
dynamic tunnel, the compiler attaches a single `0.0.0.0/0` route, optionally via
its 192.0.0.1 peer, to the service and retains its metric. No route resource means
`default_route: false`. Device-only routes stay device-only. Other route prefixes,
multiple defaults and different gateways are rejected until the native dynamic
route ownership API can represent them. Static fixed-tunnel conversion continues
to keep routes in static network intent.

An inner `localAddressFrom` reference requires explicit `field: address`, matching
the pinned configuration validator. The controller's historical fallback for an
omitted field is not imported, because that would accept a configuration which
the original public validator rejects. This applies to both fixed tunnels and
policy-managed dynamic tunnels.

## Static DHCP server migration

The behavioral source is the pinned active
[`dnsmasqLANServiceLines` controller](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/controller/chain/chain.go),
with its reservation host-file and profile handling. The older standalone
[`pkg/render/dnsmasq.go`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/render/dnsmasq.go)
interprets several fields differently. The compiler does not choose one of those
conflicting interpretations silently.

A server requires a managed, administratively UP interface and exactly one
unconditional owned `IPv4StaticAddress` with a `/1` through `/30` subnet. Additional
IPv4 addresses, a DHCP client on that interface, bridge-port/VRF-master bindings
and inferred relay topology are rejected. A bridge interface itself can be the
owned local interface when its kernel name equals its original resource name.
The server's interface reference must be the bare `Interface` or `Bridge` resource
name understood by the active chain, rather than a generic `Kind/name` or kernel
alias. A `listenInterfaces` declaration may contain only the
identical single interface reference. Separate servers require distinct
interfaces and disjoint subnets.

`addressPool` takes the old `rangeStart`/`rangeEnd`/`leaseTime` aliases when absent;
conflicting duplicate declarations are errors. The omitted lease duration remains
12 hours. The local address is materialized as the default gateway and DNS server
when the active legacy renderer would emit no override; the subnet broadcast
option is also explicit. These are the documented
[dnsmasq DHCP defaults](https://dnsmasq.org/docs/dnsmasq-man.html), rather than the
native worker's omission semantics. A single worker retains dnsmasq's default
1000-lease limit. Multiple workers are accepted only when the combined configured
pool/reservation capacity is at most 1000, so splitting the shared daemon does not
increase its possible total lease budget.

Static gateway, DNS and NTP address sources require explicit `Kind/name` and
`field: address`; optional fallbacks and live status dependencies are rejected.
`domainFrom` accepts a required static `DNSZone/name` with `field: zone`. Inline
domain and domain-source declarations cannot both be present. A primary domain
retains both domain-name and default domain-search options; an explicit
domain-search option retains its override.

Profiles are expanded into their selected native scopes. Their omitted lease
duration inherits the primary duration, as in the active chain. The primary
domain is **not** inherited by selector scopes. Each scope must use the same
owned subnet, nonoverlapping pools and disjoint selectors. Overlapping OUI/exact
MAC matches are rejected instead of assigning a new preference order. Unused
profiles, profile-plus-inline shadowing, and collisions in the original shared
dnsmasq range/selector/reservation tag namespace are errors.

Reservations retain their MAC, address, hostname and per-host options, including
usable addresses outside the dynamic pool. `server` is the original bare server
name; omission is accepted only with one unambiguous server. Reservation `scope`
is rejected by the pinned public validator and skipped by the active controller.
Reservation `leaseTime` is ignored by that active renderer. Neither is silently
activated as a new native feature.

Options use the shared typed name/number encoding: exactly one name or code and
a nonempty value, retaining list order and per-reservation overrides. Protocol
owned options, unsupported names/codes, backend quoting, raw bytes, native-only
`hex:` syntax and textual boolean aliases are rejected. Native configuration can
express additional wire intent, but it is not inferred from legacy backend text.

Nonempty `managed`, `authoritative`, `logDHCP`, `routerSource`, `router`, `dnsSource`
and `dnsInterface` settings have inconsistent or ignored behavior in the pinned
active chain, so they require explicit native configuration. `authoritative:
false` remains non-authoritative. Transit/relay roles, kea/dhcpd selection,
embedded DNS settings, conditions and custom `leaseFile` declarations are also
rejected. Positive `stickyHoldDays` is rejected: the old daemon applies the
maximum hold across all servers and later recovers sticky hosts with a fixed
12-hour lease, while native holds belong to a specific worker/scope. Copying that
number would change behavior even without importing an existing database.
The compiler does not read the old lease/sticky databases, adopt their
live allocations or import HA replication state. The native journal and future
lease lifecycle belong to the new worker; the mapping report states this boundary.

The static example is [`testdata/dhcp-server.yaml`](testdata/dhcp-server.yaml).
Tests cover its native config round trip, DHCP option bytes, profile inheritance,
reservation ownership, and explicit rejection paths.

## Explicit DHCP-derived DNS replacement

The pinned resolver treats a nonempty `DNSZone.dhcpDerived.sources` list as an
enable switch for global lease events and recovered host-file entries. It does
not filter those events by producer identity or give them the native producer's
finite ownership deadline. Consequently ordinary `Compile` rejects this graph.
`CompileWithOptions` requires the following explicit replacement for **each DNS
resolver** that receives DHCP-derived zones:

```yaml
schema_version: 1
dhcp_dns:
  DNSResolver/resolver:
    source_scoped_finite_leases: true
```

The flag must be present and true. A policy key must be an exact existing
`DNSResolver/name`; an unused policy, wrong-kind reference, null object or extra
field is an error. The policy cannot alter the source list, invent a producer or
override a validation failure. Each zone's declared source must resolve to a
distinct successfully compiled `DHCPv4Server`. The compiler rewrites it to that
worker's deterministic native identity and records the intentional semantic
replacement in `Mapping.Notes`.

Current lease records remain dynamic. The runtime verifies the committed source
digest/generation and producer ownership before returning a source-scoped DNS
projection. Producer loss, withdrawal, expired leases and removed subscriptions
remove records; replaying an old grant does not renew its authority. The existing
native DNS implementation retains explicit static record/PTR priority and zone
TTL policy. The replacement does not import a global lease file or recover sticky
hostnames without a currently authorized lease. Nonempty `hostnameSuffix` and
`ddns` are rejected because the pinned resolver does not implement them; merely
carrying those fields would claim a feature neither runtime supplies.

The complete synthetic graph and policy are
[`testdata/dhcp-dns.yaml`](testdata/dhcp-dns.yaml) and
[`testdata/dhcp-dns-policy.yaml`](testdata/dhcp-dns-policy.yaml). The public-runtime
boundary test verifies source isolation, digest fencing, finite producer expiry,
withdrawal, recovery and subscription removal while applying network intent only
to an in-memory fixture. The real-daemon integration gate reuses the independent
DHCP client/DNS query fixture with a migrated document.

## Static WireGuard migration

The compiler joins `WireGuardPeer.interface` to the exact metadata name of its
`WireGuardInterface`, as the pinned engine does. It creates a native WireGuard
link and complete peer set, retaining explicit addresses, routes, MTU, listen
port, keepalive and preshared-key file references. AllowedIPs selects and
validates peers; it does not synthesize FIB routes or routing marks. Prefixes are
canonicalized and duplicate owners are rejected. Interface aliases that omit
MTU do not erase an explicit WireGuard MTU; conflicting values fail compilation.

A clean absolute `privateKeyFile` is required. If that explicit file is absent,
the native missing-only generator retains the old engine's first-apply behavior.
The compiler never reads or creates the file. To retain an existing identity,
provision that existing key before applying the configuration. Native apply
requires a private, worker-owned regular file and trusted nonsymlink parents,
then pins an immutable snapshot. Rotation uses a new versioned path. Inline
private keys and PSKs, implicit key locations, hostname endpoints, dynamic SAM
peer sources and enrollment identities are rejected. Numeric IPv4/IPv6 endpoints
and passive peers are supported; IPv6 link-local endpoints require a zone.

The original controller inserts an IPv4 iptables listener exception, while its
owned nftables filter separately limits the effective dual-family exception to
the name-sorted first untrust zone. The compiler retains the latter's exact
interface-selection and pre-ACL placement, including the original Bridge index
behavior. A first untrust zone containing only bridges produced an ineffective
empty-interface match; migration retains that zone's ACL decisions and adds no
listener accept there. Trust/mgmt ACLs remain authoritative. A nonzero listen port without
an owned INPUT filter is rejected: creating a native filter there would also
introduce its conntrack-invalid drop, which was absent from a standalone old
listener exception. Omitted/zero listen ports keep automatic-port behavior and
do not create a filter or a listener exception. No foreign rules or existing
WireGuard links/peers are adopted.

The synthetic [WireGuard source fixture](testdata/wireguard.yaml) can be converted
with `routerd config migrate`. Its public key and addresses are test values.
See [native WireGuard ownership](../../docs/wireguard.md) for the runtime key,
rollback and quarantine contract. Limits remain 64 devices, 1024 peers per
device and 16384 allowed prefixes per device; exceeding them fails atomically.

## Resource-scoped WAN policy

A policy has its own version and exact `Kind/name` keys. Unknown, extra,
unqualified, wrong-kind and ambiguous references are errors. RA policy interface
must equal the kernel name resolved from the original resource. All five RA
feature booleans and positive table/metric values are mandatory, so an omitted
value cannot choose an OS default accidentally. The two documents are parsed
strictly; policy field names are case-sensitive and cannot use YAML aliases,
duplicate keys, null override objects or unknown fields.

```yaml
schema_version: 1
ra_clients:
  IPv6RAAddress/wan-ra:
    interface: eth0
    interface_id: "::2"
    slaac: true
    default_route: true
    on_link: true
    route_information: true
    dns: true
    table: 254
    metric: 200
ds_lite:
  DSLiteTunnel/provider:
    source_from: IPv6RAAddress/wan-ra
    dns_from: [DHCPv6Information/wan-info, IPv6RAAddress/wan-ra]
    resolvers: [{address: "2001:db8::53", port: 53}]
    selection: first
    timeout_seconds: 2
    retry_seconds: 10
```

These are example native choices, not recommended defaults for every provider.
The complete synthetic source graph and policy are in
[`testdata/wan-dynamic.yaml`](testdata/wan-dynamic.yaml) and
[`testdata/wan-dynamic-policy.yaml`](testdata/wan-dynamic-policy.yaml).

### RA ownership and implicit OS behavior

The pinned `IPv6RAAddress` apply path delegates managed RA to the host renderer;
the netplan renderer enables RA/link-local reception without specifying the full
address-generation, route, DNS, MTU or DHCP-trigger policy. The host dependencies
also set WAN RA sysctls. That does not identify one portable native policy. The
default compiler therefore rejects it even though routerd2 has a capable native
RA receiver. The source audit is in pinned
[`pkg/apply/engine.go`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/apply/engine.go),
[`pkg/render/netplan.go`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/render/netplan.go)
and [`pkg/hostdeps/hostdeps.go`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/hostdeps/hostdeps.go).

Explicit policy gives the native network writer exclusive RA/autoconf ownership.
An omitted `interface_id` selects native EUI-64, and omitted `source_address`
selects the interface link-local address. Learned DNS remains bounded typed state
and does not rewrite `resolv.conf`. Received RA MTU/hop-limit values are
observations; this policy does not activate their application or automatic
IA_NA/DHCPv6 triggering. `managed: false`, `required: true`, an externally managed
underlay and conditional/HA ownership remain errors even with policy.

### PD and supplementary information share one owner

The pinned Linux `DHCPv6InformationController` associates information with a bound
PD resource by its exact `spec.interface` string (or legacy owner metadata, which
this compiler rejects). It reads that PD daemon and requests information through
the same socket. It does not start a standalone information-only client. The
migration preserves that graph: downstream `aftrFrom` and policy `dns_from`
references to the information resource become references to the same native PD
service. Two aliases of one provider cannot appear as duplicate DNS sources.

The controller never reads `spec.request`; its client always requests the fixed
DNS, domain-search, SNTP and AFTR option set. A nonempty selection could represent
unfulfilled operator intent, so the compiler rejects it with that reason instead
of silently ignoring it. Native information-refresh tracking adds the refresh
option and bounds the learned facts by their absolute information/PD deadlines.
No current status or historical lease database is imported. See pinned
[`pkg/controller/chain/phase2.go`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/controller/chain/phase2.go)
for the actual Linux association and request path.

### Dynamic AFTR changes are explicit

The active Linux controller tries `aftrFrom`, `aftrFQDN`, `aftrIPv6`, then
`remoteAddress`. The compiler retains that order. `aftrFrom` must explicitly use
non-optional `field: aftrName` from a compiled same-underlay PD/information
resource; an omitted field means `phase` in the legacy query helper and is not
an AFTR-name shorthand.

The source pin's active Linux path resolves hostnames through the process-local
DNS listener and chooses the first returned IPv6 address. Its separate
FreeBSD/runtime helper reads `aftrDNSServers` and ordinal-selection fields, but
the active Linux path does not. Migration policy does not silently choose the
other helper's behavior. If those legacy fields are present, policy must repeat
their supported values exactly, and mapping notes state that they are being
**intentionally activated**. A conflicting or IPv4-only server list must be
resolved in the source configuration. Otherwise `selection: first` preserves
Linux answer order; an explicit ordinal policy deliberately selects from sorted
canonical addresses. Timeout and retry intervals are explicit policy values.

Named or learned AFTR candidates require explicit IPv6 bootstrap resolvers or
compiled same-underlay RA/PD DNS sources. Literal IPv6 candidates need no DNS.
The compiler rejects loopback/IPv4/hostname resolver addresses, DNS-service or
arbitrary status dependencies, duplicate provider aliases and statically known
self-resolver cycles. Configured resolvers precede the ordered learned sources.
It does not consult the host resolver or infer reachability from a pending tunnel.

An interface-discovered local source must name a compiled RA resource with SLAAC
enabled. This replaces the legacy first-usable-kernel-address heuristic with the
native named producer's lexically selected **owned, DAD-complete and preferred**
source, retaining its absolute lifetimes. A canonical
`${IPv6RAAddress/name.status.address}` in `localIPv6Source` is accepted only when
it names that same policy source. A literal source instead requires an
unconditional migrated static IPv6 address on the underlay; policy cannot
override it with a different producer. PD-derived, suffix-derived, external and
HA/VMAC-staged source addresses are not adopted.

The old controller could retain a tunnel indefinitely while pending. Native AFTR
withdraws the tunnel, B4 address and associated default route on answer/dependency
expiry or source deprecation. Repeated polls/retries do not extend the last
answer's TTL. This availability change is always recorded in the mapping report.
See [the native AFTR contract](../protocol/aftr/README.md) for the full worker and
generation/fencing behavior.

DNS uses the same original resource composition rules and transport defaults:
UDP/TCP 53, DoT 853, DoH 443 and `/dns-query`. A path omitted from a query-log
declaration stays omitted in public intent; the native service compiler assigns
the private database path. Explicit legacy query-log paths are rejected. Local
zone DNSSEC is rejected because the native engine does not sign or cryptographically
validate those zones. DHCP-derived zones require an actual migrated lease source
and recovery contract; their settings are not discarded. Unused upstreams and
zones without a resolver produce diagnostics.

Firewall rules retain the original **metadata-name ordering**, independent of
their order in YAML. Zone-to-self and zone-to-zone chains are flattened with
terminal decisions; they do not fall through to another zone's policy. DHCPv4
client/server and PD client exceptions are placed before ACLs only for untrusted ingress,
matching the original service-hole policy. On trust/mgmt zones, explicit ACLs
can still deny those ports. Automatic DNS candidates do not bypass explicit
ACLs in any zone role; address-source resolution creates no additional hole.
The original `ipip` matcher is IPv6 outer protocol 4;
the projection keeps that family restriction.

The running Linux NAT controller and the combined configuration exporter are
not equivalent at the pinned revision. The exporter invents a trust-to-untrust
masquerade when no NAT resource is declared; the controller does not. The
exporter also handles `outboundInterface/sourceCIDRs/translation`, while the
running NAT controller reads the standard `type/egressInterface/sourceRanges`
form. Those disagreements produce explicit migration diagnostics. Declare
standard-form NAT resources to settle the intended behavior. Policy-only
firewall configurations, DS-Lite names that differ between controllers, and
virtual-link NAT aliases with different `ifname`/resource names are similarly
rejected rather than guessed.

The pinned firewall renderer always enables same-role forwarding: setting
`sameRoleAccept: false` never disables it. The compiler requires this ambiguity
to be resolved by explicit `true` or explicit deny rules. It does not silently
turn an ineffective legacy field into a new security policy. Kernel logs remain
kernel log events and do not imply NFLOG sampling or old log-retention behavior.

## Explicitly incompatible areas

- Nonempty `when`, `dependsOn` and `ready_when` conditions, metadata ownership,
  dynamic annotations and observed `status` are rejected. Static migration must
  not convert a conditional address, tunnel or advertisement into an always-on
  resource.
- Firewall address sets, flow pinholes, connection/rate limits, NFLOG sampling,
  client policy, packet marks, NAT pools/persistent port mappings, port-forward
  and hairpin composition are not migration rules yet. Self-originating ACLs
  are rejected because the original Linux renderer has no OUTPUT hook.
- Egress policies contain candidate selection, health/freshness, hysteresis,
  marking, preferred-source and dynamic gateway semantics. Static policy rules
  alone do not preserve them.
- Original RA is implemented through dnsmasq interface-address constructors.
  Its `validLifetime` sets the router lifetime in `ra-param`, not merely a PIO
  valid lifetime. Router, PIO and DNS lifetimes must be selected explicitly for
  native RA; the compiler does not copy a duration into a different field.
- `IPv6RAAddress` has no automatic conversion of host-network-manager defaults.
  Resource-scoped replacement policy is required; observe-only ownership,
  required-address readiness and HA conditions are still not convertible.
- Standalone `DHCPv6Information`, ambiguous PD ownership, differing literal
  interface references and nonempty ignored request selections are rejected.
- PD-derived/HA/suffix DS-Lite sources, arbitrary dynamic status references,
  unowned literal sources and non-default dynamically owned tunnel routes need
  additional native ownership APIs; explicit policy cannot erase those gaps.
- Legacy IPv4 VRRP leaves the wire version to Keepalived defaults, puts VIPs on
  the original interface, and defaults to non-preemption. Native VRRPv3 and
  election-owned VMACs require an explicit peer-aware transition. Authentication,
  unicast peers, tracking, graceful activation and additional failover VMACs are
  not erased.
- VXLAN FDB peers, L2 filtering, outer-DF defaults, PPPoE, stateful/both/RA-only DHCPv6 servers, DHCPv6 relays
  and unsupported DHCPv4 server modes, dynamic route protocols, other
  tunnel/VPN types, cluster synchronization, plugins,
  cloud/federation/mobility resources, logging/retention/system services and
  other unmapped kinds are rejected with a resource path.

These are **compiler restrictions**, not a statement that every named native
module is absent. Additional native APIs can be incorporated only after a
semantics-preserving conversion and fixture are implemented.

## Parsing and failure boundaries

The decoder uses pinned `api.ConfigResourceKinds` and the still-supported older
Link/Bridge/VXLANSegment/static-route types. It recursively checks public **YAML**
tags before typed decoding. Internal JSON-only fields such as
`DNSResolver.spec.sources` and `DHCPv6PrefixDelegation.spec.iaid` are rejected.
Unknown fields are never accepted by the old non-strict `Resource.UnmarshalYAML`.

One document, string mapping keys and unique keys are required. Anchors, aliases,
merge keys and custom tags must be expanded/removed first. The compiler limits
input to 2 MiB, depth to 96, nodes to 100,000 and resource count to 4096.
Diagnostics are capped at 512 plus an explicit truncation issue. The resulting
document must pass the ordinary native validator.

Firewall projection permits at most 64 zones, 90 interface memberships, 1024
CIDRs per selector, 128 unique destination exclusions and 8192 expanded packet
rules. Exceeding a limit is an error, never truncation of the applied policy.

The module imports pinned API/type and pure DNS normalization packages, not the
old controller graph or its host-state/SQLite configuration validator.

## Verification and homert scope

Tests cover static links/VRF/addresses/routes/forwarding, DHCP wire identity,
hexadecimal PD subnet allocation, DNS resource joining and static references,
DS-Lite peer-address semantics/defaults, native encoding/decoding, deterministic
identity mapping, named ACL precedence, role-policy packet decisions, management
protection, client exceptions, SNAT priority, NAT-only hooks, CIDR exclusion
boundaries, unknown/internal fields, duplicate keys, YAML constructs,
unsupported lifecycle/ACL/VRRP semantics, secret-free diagnostics and atomic
failure. WAN policy tests additionally cover strict resource scoping, source and
DNS dependency cycles, explicit changes to ignored legacy fields, shared PD/info
identity, dynamic route joining, deterministic output and mutation isolation.
Most tests are pure compiler tests. The public-runtime boundary test uses a
private temporary Unix supervisor and a validating **in-memory** network writer;
it checks the compiled graph through RA/PD snapshots, DAD gating, ordered AFTR
inputs, finite B4/route projection, stale-information withdrawal and configuration
removal. It never mutates host networking or asserts wire interoperability.
Native packet/socket validation belongs to the protocol/network integration
gates. Fuzz invariants require successful output to pass native validation and
failure to return no document.

No deployed homert02 or homert03 running-config was present in the reviewed
checkout. The homert02 notes mention PD/information services, DNS, firewall zones
and policies, health checks, DS-Lite/PPPoE failover, egress selection and NAT.
The notes are deployment evidence, not complete desired configurations. Therefore
this compiler has **not** established migration or runtime parity for those
machines. Native WAN RA/SLAAC/default-router, AFTR and DHCP-to-DNS modules have
their own protocol/integration gates. Applying them to these deployments still
requires the actual configurations, explicit migration of their dependencies
and lifetimes, and deployment-specific tests. HA gating/VMAC transitions and
policy failover also remain outside this compiler's supported conversion.

The package fixtures contain synthetic documentation addresses and no production
configuration or credentials.

The public WAN tutorial's PD + information + DS-Lite pattern is the structural
reference for the synthetic graph. Its unmodified PD-derived source, suffix and
implicit tunnel default route are **not** accepted by this compiler. The public
wizard's home DS-Lite fixture also uses external WAN ownership, delegated source,
implicit LAN RA, health checks and DHCP-derived DNS, so it is not a positive
full-migration fixture. Their pinned source locations are
[`docs/tutorials/wan-side-services.md`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/docs/tutorials/wan-side-services.md)
and [`website/fixtures/wizard/home/home-dslite-ipv6.yaml`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/website/fixtures/wizard/home/home-dslite-ipv6.yaml).

## Tailscale daemon replacement

`TailscaleNode` with `acceptDNS:false` and SSH disabled is imported using an
explicit resource-scoped policy:

```yaml
schema_version: 1
tailscale:
  TailscaleNode/overlay:
    interface: tailscale0
    bootstrap_dns: [192.0.2.53]
    control_ca_file: /etc/ssl/certs/ca-certificates.crt
    enrollment: saved_identity
```

Use reachable bootstrap resolvers and the intended trust bundle. Choose
`enrollment: key_file` with `auth_key_file: /private/native.key` to enroll or
reauthenticate with an owned raw key file. `saved_identity` requires no key path
and uses previously provisioned vendor identity. The legacy `authKeyFile` is a
systemd EnvironmentFile, not a raw native key. Legacy inline keys and environment
references are deliberately replaced by this policy; no secret value or file
content is copied into the output.

Retained intent includes hostname, login server (the original public default
when absent), advertised subnet/exit routes and tags, acceptRoutes and shieldsUp.
An absent node creates no native service. Omitted acceptDNS defaults to true in
`tailscale up`, so it is rejected along with explicit true: that OS resolver mutation differs
from explicit native MagicDNS forwarding. Tailscale SSH and runtime conditions remain unsupported
instead of being silently removed; any such issue rejects the entire batch.

Stop the external daemon and verify its old kernel ownership has gone before
provisioning its opaque `tailscaled.state` into the native persistent
`services/tailscale/tailscaled.state` with owner-only permissions. Provisioning
is an explicit deployment operation; this pure converter never reads state or
stops a daemon. The checked case uses an independently enrolled official
Tailscale1.104.2 state file with the pinned native1.104.2 engine; other versions
and hardware-sealed state have not been qualified.

The native engine owns TUN/table52 and uses netfilter off. Declare forwarding,
FW/NAT and WAN policy ordering as native intent. Existing routerd-owned INPUT
exceptions on UDP41641 retain the exact first-untrust interface scope before
its ACL, sharing the WireGuard hole-index implementation. External daemon or
foreign host netfilter rules are not adopted. No new INPUT hook is created for
a standalone Tailscale declaration. The actual communication fixture combines
the imported interface/INPUT policy and node with explicit native LAN/WAN
forwarding/NAT and conditional activation before first boot.

`make check-tailscale RELEASE_ID=tailscale-import` executes the real migration
CLI, signed catalog and PID1 workers against Headscale/dnsmasq/official peer.
It checks identity provisioning, key removal, rollback, original condition expiry,
node revoke/reauthentication and accepted IPv4/IPv6 routes with real TCP/UDP.

For MagicDNS, add an explicit native DNS source referring to the imported native
service name with `TailscaleNode/NAME` and `field: dnsServers`; see
[DNS forwarding](../protocol/dns/README.md). This does not import the old
`acceptDNS:true` OS resolver mutation, global/split resolvers or search domains.
