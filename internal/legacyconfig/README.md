# Strict migration of original routerd resources

This package is a pure compiler from the Linux resource schema at
**imksoo/routerd commit 1329ef54f296507eda79bbbc3b853783dd27251e** to
`config.Document`. It neither invokes the original controllers nor runs commands,
reads host state, contacts an upstream, modifies a network, or imports a lease
database. It is not a compatibility implementation of every original resource.

## API

```go
document, err := legacyconfig.Migrate(inputYAML)
result, err := legacyconfig.Compile(inputYAML)

// Deliberate native replacements for implicit legacy WAN behavior:
options, err := legacyconfig.DecodeOptions(policyReader)
result, err = legacyconfig.CompileWithOptions(inputYAML, options)
```

`Compile` additionally returns the original Router name, source revision, and
one `Mapping` per accepted resource. A mapping records native targets and any
structural changes. Service identities are deterministic, bounded names derived
from the original kind/name. Cross-kind name collisions therefore do not silently
merge services.

`Compile` never guesses a host network manager's RA defaults or silently adopts
the new AFTR lifetime contract. `CompileWithOptions` permits **explicit replacement
intent** for individual `IPv6RAAddress` and `DSLiteTunnel` resources. Each such
mapping says which behavior changes; a successful replacement is not a claim of
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

The CLI entry point is `routerd config migrate FILE`. It emits native YAML on
success. Code that also needs the mapping report should use `Compile`.

## Implemented conversions

| Original declaration | Native intent and restrictions |
| --- | --- |
| `Interface` | An explicitly `managed: true`, `adminUp: true` interface owned by routerd becomes a native Interface, retaining MTU. External/observe-only aliases remain reference-only and are recorded in the report. Address or master attachment to such an alias is rejected because native interface management brings a link up. |
| `Link` | Existing-link alias only; no link type, ownership or administrative state is invented. |
| `VRF` | A native VRF link with the same route table and explicit member masters. Members must independently authorize administrative management. |
| `Bridge` | A native bridge with explicit members, MAC, MTU and the original multicast-snooping default of false. Requires `stp: false` and `rstp: false`. Custom timers are rejected; dormant timing attributes use kernel defaults. |
| `IPv4StaticAddress` | Preserves host/prefix bits. `exclusive: true` is rejected because it removes addresses owned by other managers. Explicit overlap policy and its justification are checked. |
| `VirtualAddress` in static mode | IPv4 /32 or IPv6 /128, including a provably static resource address reference. Runtime tracking, automatic hostname publication and gratuitous-ARP intent are rejected. |
| `IPv4StaticRoute`, `IPv6StaticRoute` | Same destination, gateway, device and metric; interface aliases are resolved. |
| `IPv4Route` | Static unicast routes. A static DS-Lite device reference and explicit static address gateway reference can be resolved. Blackhole, preferred-source and dynamic references are rejected in this compiler. |
| `Sysctl` | Required runtime `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` with exact 0/1 values. No implicit forwarding is enabled. Other sysctls, optional/non-exact policies and persistent host files are rejected. |
| `DHCPv4Client` | Native wire identity, requested address, hostname/class/client ID, default-route policy and metric. Requires `useDNS: false` because the old default of true changes the host resolver. Native learned DNS is status data rather than a substitute for that side effect. |
| `DHCPv6PrefixDelegation` | Native IA_PD service, pinned DUID-LL/IAID 1 defaults, explicit DUID, provider profile and prefix-length preference. Explicit external clients and required-prefix readiness semantics are rejected. |
| `DHCPv6Information` with one matching PD | An alias of that PD service, sharing its DUID/IAID, UDP 546 socket and supplementary Information-Request. The original **literal interface reference** must match exactly; merely resolving two different aliases to the same NIC does not reproduce the old association. Standalone, ambiguous and nonempty `request` declarations are rejected. |
| `IPv6RAAddress` with explicit policy | One native `ra_client`, using the policy's same-interface SLAAC/default-route/on-link/RIO/DNS flags, source/IID/router filters and explicit route table/metric. Requires managed RA, independently managed/admin-up underlay, and `required: false`. Host defaults and learned state are not imported. |
| `IPv6DelegatedAddress` | A /64 allocation under its PD service, retaining the suffix and **hexadecimal** subnet ID. In the old schema, `subnetID: "10"` means hexadecimal 0x10, not decimal 10. IDs must fit within the provider prefix. /128 installation, zero host ID and implicit advertisement flags are rejected. |
| `DSLiteTunnel` | Fixed outer IPv6 endpoints, inner IPv4 /32 **peer 192.0.0.1/32**, explicit tunnel identity, no encapsulation-limit option and original fixed default MTU of **1460**. The default inner address is 192.0.0.2. `localAddressFrom` means the **inner IPv4** source in the pinned controller, not the outer IPv6 endpoint. |
| `DSLiteTunnel` with explicit policy | One native `ds_lite` service, preserving candidate precedence and B4/MTU identity while explicitly changing DNS bootstrap, source ownership/selection and endpoint expiry. Accepts an owned static IPv6 source or the policy's compiled RA source. Information references map to their existing PD service. One independently declared IPv4 default route can join the dynamic tunnel's lifetime owner. PD-derived/HA/suffix sources remain rejected. |
| `DNSResolver` + `DNSForwarder` + `DNSUpstream` + `DNSZone` | Resolves the static resource graph into native DNS runtime intent. Preserves source order, listener selection, transport/TLS identity, bootstrap servers, interface binding, cache/timeouts/health checks, query-log enablement, zones and static records/reverse zones. Statically known address references are resolved; learned/delegated references are rejected. |
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
and PD client exceptions are placed before ACLs only for untrusted ingress,
matching the original service-hole policy. On trust/mgmt zones, explicit ACLs
can still deny those ports. The original `ipip` matcher is IPv6 outer protocol 4;
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
- VXLAN FDB peers, L2 filtering, outer-DF defaults, PPPoE, DHCP servers/relays,
  dynamic route protocols, tunnels/VPNs, cluster synchronization, plugins,
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
