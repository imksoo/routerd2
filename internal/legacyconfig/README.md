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
```

`Compile` additionally returns the original Router name, source revision, and
one `Mapping` per accepted resource. A mapping records native targets and any
structural changes. Service identities are deterministic, bounded names derived
from the original kind/name. Cross-kind name collisions therefore do not silently
merge services.

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
| `IPv6DelegatedAddress` | A /64 allocation under its PD service, retaining the suffix and **hexadecimal** subnet ID. In the old schema, `subnetID: "10"` means hexadecimal 0x10, not decimal 10. IDs must fit within the provider prefix. /128 installation, zero host ID and implicit advertisement flags are rejected. |
| `DSLiteTunnel` | Fixed outer IPv6 endpoints, inner IPv4 /32 **peer 192.0.0.1/32**, explicit tunnel identity, no encapsulation-limit option and original fixed default MTU of **1460**. The default inner address is 192.0.0.2. `localAddressFrom` means the **inner IPv4** source in the pinned controller, not the outer IPv6 endpoint. |
| `DNSResolver` + `DNSForwarder` + `DNSUpstream` + `DNSZone` | Resolves the static resource graph into native DNS runtime intent. Preserves source order, listener selection, transport/TLS identity, bootstrap servers, interface binding, cache/timeouts/health checks, query-log enablement, zones and static records/reverse zones. Statically known address references are resolved; learned/delegated references are rejected. |
| `FirewallZone` + `FirewallPolicy` + `FirewallRule` | Native INPUT/FORWARD hooks with the original conntrack precedence, loopback/ICMPv6 acceptance, explicit ordered ACLs and role-based terminal actions. Retains source/destination CIDRs, TCP/UDP port sets/ranges, ICMP types, accept/drop/reject and unsampled kernel logging. Does not invent an OUTPUT hook. Overlapping zones, dynamic sets, rate/connection limits and NFLOG are rejected. |
| `NAT44Rule` in standard form | Explicit `type: masquerade` or `snat`, `egressInterface`, `sourceRanges`, positive destination CIDRs and destination exclusions. Resolves provably static SNAT address references. NAT-only configurations retain **no filter hook**. Exclusions are compiled to bounded positive CIDR sets. |

DS-Lite retains the old tunnel-name fallback order: `tunnelName`, then the literal
`spec.interface` value, then the resource name. MTU 1460 is retained even when the
underlay MTU differs from 1500; substituting underlay-minus-40 would change the old
configuration. The pinned **Linux** tunnel controller does not apply
`defaultRoute`/`routeMetric` itself; the migration rejects those settings instead
of inventing routes. Declare an independent `IPv4Route`. The native `ds_lite`
service provides dynamic AFTR resolution, but this compiler does not yet map
legacy DNS discovery, provider fallback or PD-derived outer endpoints into it.
Those inputs are not frozen into guessed addresses. Native AFTR uses explicit
IPv6 bootstrap dependencies and finite last-good expiry; the old Linux
controller used local DNS and could retain a pending tunnel indefinitely.
That change, its local-source selection, and the old inner IPv4 peer require an
explicit semantic migration. See [the native AFTR contract](../protocol/aftr/README.md).

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
- The native WAN `ra_client` receives RA/SLAAC/default-router/DNS lifetimes,
  but the old `IPv6RAAddress` resource's managed/kernel-RA ownership and
  required-address readiness have no automatic conversion here. Native
  availability does not make those implicit policies interchangeable.
- Original `DHCPv6Information` supplements an existing bound PD client.
  Creating a second independent native process on UDP 546 is not equivalent.
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
failure. Tests here are pure compiler tests; native packet/socket validation
belongs to the protocol/network integration gates. Fuzz seeds use all positive
fixtures and malformed YAML; the invariant is that success passes native
validation and failure returns no document.

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
