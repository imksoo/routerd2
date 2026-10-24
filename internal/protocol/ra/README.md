# Native IPv6 Router Advertisement worker

`Validate(Config)` checks wire and timing constraints. `Run(ctx, config, effects,
ready)` opens a native ICMPv6 socket, joins ff02::2, sends an initial advertisement
and reports readiness. It periodically advertises and responds to valid Router
Solicitations, including unspecified-source solicitations. Incoming hop limit,
interface, source scope, ICMP type/code and option lengths are checked. Multicast
advertisements are rate-limited and use hop limit 255 with an explicit link-local
source selected by IPV6_PKTINFO.

The worker emits prefix information, M/O flags, preference, source-link-layer,
MTU, RDNSS and DNSSL options. Router lifetime zero is explicit; the compiler must
set a positive lifetime when the router should be a default router. Interval
defaults are min 200/max 600 seconds; DNS lifetime is also explicit. Prefix
`ValidUntil`/`PreferredUntil` prevent a previous delegated prefix from having its
remaining lifetime renewed by every advertisement. Autonomous prefixes must be
/64. The worker does not configure the interface, enable forwarding, allocate
DHCPv6 addresses or install routes.

Configured DNS uses `RDNSS []string` and `DNSSL []string`. Learned DNS uses
`LearnedRDNSS []DNSServer{Address, ValidUntil}` and
`LearnedDNSSL []SearchDomain{Domain, ValidUntil}`. The sender caps each learned
wire lifetime by both `DNSSLifetimeSeconds` and the remaining absolute source
lifetime, rounded down. A passed deadline produces lifetime zero even when the
controller stops sending updates. Zero `ValidUntil` means an explicitly
unlimited source, not a missing source; the controller must omit missing data.
An identical configured address/domain takes precedence, so expiration of its
learned copy cannot withdraw static intent. Duplicate learned sources use the
longest deadline, and each canonical name appears only once in an advertisement.
Validation reserves space for different deadline groups even when their
initial capped lifetimes happen to be equal.

Shutdown attempts one final advertisement with router, prefix and DNS lifetimes
zero before closing the socket, and reports the withdrawing status. It is a
bounded best-effort network transmission; there is no acknowledgment from every
LAN host. A hard crash cannot transmit a withdrawal. The declared lifetimes
therefore remain part of the failure/recovery contract.

Tests validate packet options, absolute lifetime decay, withdrawal and
solicitation rejection without opening a socket. The isolated native test in
`../dhcp4/kernel_integration_test.go` additionally verifies kernel multicast
delivery with hop limit 255, a received RS and shutdown packet reception across
private veth peers. Client SLAAC behavior and PD-driven renumbering still require
control-plane integration evidence. An RA engine is not DHCPv6 server parity.

## Prefix updates without restarting the process

`RunWithUpdates(ctx, initial, updates, effects, ready)` retains the same ICMPv6
socket and identity. A controller may replace prefixes, absolute deadlines and
DNS options while the worker runs. The resource, interface and source identity
cannot change. Invalid updates leave the previous configuration in service and
are reported through `RejectedUpdates` and `LastUpdateError`; failure to publish
that status still stops the worker.

A renewal of a retained prefix never produces a zero-lifetime router or prefix.
Only removed prefixes or DNS options are withdrawn, over three advertisements.
Updated advertisements retain the minimum multicast interval. Closing the
update stream retains the final configuration; absolute deadlines still decay.
`Generation` and `LastConfigUpdate` expose accepted updates. The isolated packet
test verifies renewal, partial prefix replacement, rejection of an interface
change, continued RS responses after stream closure, and final shutdown.

`TestIsolatedLearnedDNSLifetimes` in this package additionally re-executes into
verified fresh user/network namespaces and captures real ICMPv6 over private
veth peers. It checks learned RDNSS/DNSSL countdown, configured duplicate
precedence, renewal without withdrawal, removed-name withdrawal, deadline
expiration with a closed update stream, and final shutdown. Set
`ROUTERD2_INTEGRATION_REQUIRED=1` with `go test -tags integration` to fail if
isolation is unavailable instead of skipping the packet test.

The bundled controller uses this API for PD-derived LAN advertisements. Its
immutable instance identity contains the LAN template; a lease renewal changes
live prefix and learned DNS data rather than restarting the worker. Learned
DNS is usable only after an information reply and until the controller's
DHCPv6 information freshness deadline, additionally capped by the PD lease.
Explicit infinite information refresh still has that PD cap. Kernel addresses include
both current and still-valid deprecated prefixes, with finite Linux lifetimes.
