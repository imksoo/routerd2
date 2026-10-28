# Native WAN ownership

`resources[].service.ra_client`, `resources[].service.dhcp6` and `resources[].service.ds_lite` are separate
supervised processes. The example [wan-dslite.yaml](../examples/wan-dslite.yaml)
combines them. LAN DHCP, DNS and prefix advertisements are separate services;
the example does not implicitly enable them.

## RA client

The RA client opens an ICMPv6 socket and sends router solicitations. It checks
link-local source, hop limit 255, checksum and the complete option structure
before accepting an advertisement. `allowed_routers` can narrow the accepted
link-local sources; it does not authenticate an IPv6 neighbor.

The network worker sets the interface's `accept_ra=0` and `autoconf=0` before
starting the client. A conflicting explicit kernel configuration is rejected.
Removing the service restores the saved values, subject to ownership/drift
checks. This prevents a second kernel RA owner from silently creating routes
and addresses outside the routerd journal. Link-local address generation and
normal DAD remain kernel responsibilities.

Autonomous /64 prefixes generate /128 SLAAC addresses; on-link prefixes are
separate routes. The A and L bits do not substitute for each other. Preferred
and valid lifetimes, the RFC 4862 two-hour rule, route preference, default/RIO
routes, RDNSS and DNSSL are tracked independently. DAD-failed or deprecated
addresses cannot become a new DS-Lite source. The default IID is derived from
the interface MAC; `interface_id` can explicitly select its lower 64 bits.

Finite address and IPv6 route lifetimes are installed in Linux, so they expire
even if the supervisor, controller, network writer and RA producer stop together. Extremely
long finite routes use a renewable 6–7 day kernel grant; the protocol's complete
advertised lifetime remains in its snapshot. Explicit infinite lifetimes stay
infinite. The state journal is scoped to the kernel boot ID; previous-boot
advertisements are not silently restored as current router authorization.
Published RA facts and their journal carry original BOOTTIME deadlines, including
explicit infinity in that boot. Status-only updates, UTC corrections and
same-boot restarts do not rebase those deadlines. Kernel route timer ownership
uses its actual monotonic expiry. `make check-ra-client` verifies real RA and
source/default-route TCP/UDP with capture; `make qemu-ra-clock` additionally
checks actual UTC corrections, worker restarts, kernel expiry with all owners
stopped, stale delivery and fresh RA recovery. Physical suspend remains open.

## AFTR resolution and DS-Lite

The DS-Lite service owns its generated tunnel, B4 address and optional IPv4
default route. No static interface or route may also own that generated tunnel.
Firewall rules may match its name before it exists.

| Field | Meaning |
| --- | --- |
| `interface` | IPv6 underlay, shared by all referenced WAN sources |
| `tunnel` | Dynamically owned kernel ip6tnl interface |
| `source_from` | Named RA client providing an eligible global IPv6 address |
| `local_address` | Alternative explicit source, already declared on that underlay by an `address` resource |
| `aftr_from` | Named DHCPv6 service supplying the preferred AFTR candidate |
| `candidates` | Ordered configured hostname or IPv6 fallbacks |
| `dns_from` | Explicit RA/DHCPv6 service names supplying IPv6 DNS servers |
| `resolvers` | Explicit `{address, port}` IPv6 DNS servers; port defaults to 53 |
| `selection` | `first` preserves the original Linux controller's DNS answer order; `ordinal` / `ordinalModulo` explicitly sort and select |
| `b4_address` | Inner IPv4 host/prefix; default `192.0.0.2/32` |
| `b4_peer` | Optional inner IPv4 peer as a canonical `/32`, different from the local address; the local address must also use `/32` |
| `default_route` | Default true; route metric/table are independently configurable |
| `route_gateway` | Optional IPv4 default gateway, equal to the explicitly configured `b4_peer` IP; forbidden with `default_route: false` |

Exactly one of `source_from` and `local_address` is required. The coordinator
also observes the installed address through the network worker: it must be
owned, preferred and neither tentative nor DAD-failed. The deterministic source
selection currently chooses the lexically first eligible RA address; this is
not complete legacy `SelfAddressPolicy` compatibility.

With no peer or gateway, the existing native default is an inner address and a
device route. To preserve the original Linux B4 point-to-point convention, set
`b4_peer: 192.0.0.1/32` and `route_gateway: 192.0.0.1`. Peer, address and route
belong to the same dynamic service and are withdrawn together. Changing them
changes the private instance digest; a result from the previous producer cannot
reinstall the old projection.

DNS queries bind their source and device to the IPv6 underlay. Resolver
addresses must be explicit IPv6 literals, including scoped link-local servers
on that interface. Host DNS configuration, loopback DNS and IPv4 resolver
fallbacks are not used. This avoids requiring the DS-Lite tunnel to resolve the
AFTR needed to create that same tunnel. Direct IPv6 AFTR candidates need no DNS.

DHCPv6 information stops authorizing candidates and DNS servers at its refresh
deadline, and PD-mode information is additionally bounded by the lease. DHCPv6
option 32's explicit `0xffffffff` permits no refresh deadline only after an
information reply has actually been received; a missing timestamp or default
zero value does not imply infinity. Infinite refresh remains bounded by the PD
lease when PD is enabled, and it does not make an AFTR DNS answer's TTL infinite.
RA DNS options keep their own deadlines. Inputs carry a configuration digest and an
increasing dependency generation; polls do not renew lifetimes, and a result
from an older generation cannot install a tunnel.

The resolver validates DNS response source, ID, question and bounded CNAME
chains, and uses TCP after truncated UDP. DNSSEC validation is not implemented
by this bootstrap resolver. Endpoint validity is bounded by source preferred/
valid lifetime, candidate/resolver lifetime and minimum DNS chain TTL. A failed
refresh retains the last good endpoint only until those original deadlines.
This deliberately differs from retaining a stale AFTR forever while pending.

Static IPv4-in-IPv6 tunnels use an ordinary `link` resource with `kind: ip6tnl`,
`parent`, `local` and `remote`. Configure MTU and the B4 address through an
`interface` resource and routing through `route`; see `examples/ds-lite-static.yaml`.
Dynamic AFTR resolution generates that same link type. There is one ownership
journal and one link writer, with interface index, random marker and attribute
readback checks.

The original AFTR deadline is attached to the owned link. The network worker
independently fences it DOWN and removes managed addresses and routes using it,
including multipath routes, even while control and supervisor are stopped.
Restarting the writer retains the original deadline. Expired notifications do
not acquire a new lease at delivery. Control subsequently removes the inactive
projection; fresh DNS can recreate it. Linux ip6tnl has no DNS TTL timer, so a
stopped network writer cannot enforce this IPv4 expiry. Kernel address timers,
link fencing and process supervision remain distinct mechanisms.

## PD-derived host endpoints

PD allocations select their installed address mask with
`delegations[].prefix_length`: zero/64 installs a LAN address, and 128 installs
a host endpoint without another connected /64 route. This permits multiple
DS-Lite sources on WAN while the same delegated /64 remains connected to LAN.
Set the DS-Lite `source_delegation` to the selected allocation name. Its address
and lifetime still belong to the one PD client; a host endpoint does not generate
LAN advertisements. Original `IPv6DelegatedAddress.prefixLength` is retained on
import, and a distinct DS-Lite host-suffix override creates a /128 endpoint.

## Learned DNS in LAN advertisements

A delegation with `rdnss_router` advertises its own delegated LAN address as
an RDNSS server. Its advertisement expires with the delegated prefix. The DNS
service must actually listen on that address. Set its `listen_interface` to
the LAN interface when using a wildcard listener: Linux then accepts DNS only
on that link and follows the addresses installed and removed there. A wildcard
still accepts other addresses on the same link. A DHCPv6 information server
can select the same allocation with `dns_delegation`, specifying its service
and delegation names. It supplies only addresses in the applied network
projection and retains their original expiry.

A DHCPv6 delegation independently selects `use_learned_rdnss` and
`use_learned_dnssl`. It keeps configured `rdnss` and
`dnssl` entries separate from learned DNS. Learned entries carry the earlier of
the information freshness deadline and the PD lease expiry into the RA worker.
Each outgoing RDNSS/DNSSL lifetime is capped by that absolute deadline. Closing
the controller update stream therefore cannot refresh learned DNS indefinitely;
expired entries advertise lifetime zero. This is an explicit stale-information
policy, not a claim that the DHCPv6 refresh timer itself is a prefix lease.

An identical configured entry wins over its learned copy. Renewing a retained
entry never sends a conflicting zero lifetime, and removing one name does not
withdraw the others. Prefix and DNS renewals update the running RA worker's
live configuration without changing its immutable template or restarting its
socket. Static DNS continues to use its configured advertisement lifetime.

## Inspection

For an explicitly authorized local access network, run `make check-uplink
UPLINK_INTERFACE=... UPLINK_AFTR=... UPLINK_SECONDS=7800`. A separate MAC and LAN
namespaces use the actual daemon, independent BusyBox and dhcpcd clients, and a
dnsmasq conditional upstream. Host management addresses, routes and resolver
settings are not reconfigured. The DHCP clients obtain their LAN configuration;
the standard dhcpcd resolver hook installs the received DNS in a private
resolver mount; integration with systemd-resolved is not exercised.
`UPLINK_STATE_DIR` can select a private directory that retains the test WAN MAC,
PD and DHCPv4 leases, and LAN DHCPv6 server DUID between runs. The directory
must not be shared with another active test or a production router. The test
checks IPv4 DS-Lite and IPv6 HTTPS, UDP/TCP DNS, conditional forwarding,
upstream failure and recovery. Set `UPLINK_DNS_UPSTREAM` to exercise a DoH
endpoint using WAN-learned bootstrap DNS. RA and DHCPv6 information advertise
the delegated router address. The DNS server listens on the IPv6 wildcard
bound to the LAN interface; the test firewall also blocks WAN DNS input.
Set `UPLINK_WAN_OUTAGE=1` to blackhole only the isolated router's WAN packets,
then verify client connectivity and PD identity after removing the blackhole.
Set `UPLINK_FULL_RESTART=1` to restart the daemon with the same state and
verify that the connected clients regain communication.

Set `UPLINK_TAILNET=1` to combine the automatic WAN/PD and LAN services with
independent Headscale and an official Tailscale peer. Their dedicated test
underlay is fixed at `192.0.2.0/24`; Internet exit traffic uses the actual HGW,
DS-Lite and native IPv6. The test advertises the IPv4 LAN, matching the observed
homert profile. LAN-originated IPv6 uses an explicit masquerade rule toward
`tailscale0`. An exit route alone does not authorize unadvertised LAN access:
upstream Tailscale excludes local LANs from its default-route filter, including
replies addressed to an unadvertised delegated prefix. This fixture does not
establish inbound access to the delegated IPv6 LAN. It checks TCP/UDP application
traffic, both-family Internet HTTPS/UDP DNS and the same node identity after a
whole-daemon restart. The fixed-underlay `make check-tailscale` additionally
checks both-family advertised subnets, DERP, revocation and rollback.

The test kills and gracefully restarts the PD worker, checks retained DUID/IAID
and recovered communication, and verifies Rebind/Reply in capture. When the
interval includes the initial T1, a captured natural Renew/Reply before restart
is required. A changed deadline alone is not counted as Renew: Rebind can
legitimately obtain a new grant. Short runs do not prove natural renewal.
Graceful shutdown must withdraw the delegated LAN RA/address without releasing
the upstream grant. Temporary state and bounded captures are removed on exit.

Use `show services` for a bounded summary, `show services wan-ra` or
`show services internet` for the complete individual snapshot, and `show
network` for kernel observations. Large service snapshots are omitted from the
summary with `snapshot_truncated` / `snapshot_bytes`, not silently shortened.
`show route-issues` and `show route-issues offset N` paginate routing diagnostics.

The protocol implementations and their source/RFC notes live in
[`internal/protocol/raclient`](../internal/protocol/raclient) and
[`internal/protocol/aftr`](../internal/protocol/aftr). The required QEMU tests use
an independent packet/DNS peer and the actual two-ELF process tree. These tests
do not establish full original-resource compatibility or verify a particular
ISP/homert deployment.

## Migrating original WAN resource graphs

`routerd config migrate` refuses to guess a replacement for host-managed RA
defaults or the original process-local AFTR resolver. Supply a strict policy
document when intentionally replacing those behaviors:

```sh
routerd config migrate --policy wan-policy.yaml original-router.yaml > candidate.yaml
routerd config validate candidate.yaml
```

The policy names exact original resources such as `IPv6RAAddress/wan-address`.
RA feature booleans, metric/table, AFTR source selection and bootstrap DNS must
be explicit. Unused or unknown entries, incompatible dependencies and unsupported
fields fail the entire migration. The native YAML goes to stdout only after
validation; quoted review notes go to stderr. Keep those notes with the proposed
configuration because an explicit policy records a deliberate change of
semantics, not automatic compatibility.

A same-interface `DHCPv6Information` resource can share the original PD client's
socket and lease context. The compiler preserves the original B4 peer and the
independent IPv4 default route when it can represent the complete graph. It
rejects unsupported HA and delegated-source selection rather than dropping them.
See the [migration contract](../internal/legacyconfig/README.md) and its
[source](../internal/legacyconfig/testdata/wan-dynamic.yaml) and
[policy](../internal/legacyconfig/testdata/wan-dynamic-policy.yaml) fixtures.
