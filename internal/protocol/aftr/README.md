# Native DS-Lite endpoint resolution

This package turns explicit IPv6 underlay facts into a bounded DS-Lite endpoint
snapshot. It resolves an AFTR name or selects an IPv6 literal. The coordinator
and the single network writer create the Linux `ip6tnl`, B4 address and routes.
The resolver does not inspect or change kernel interfaces, addresses, routes,
sysctls or firewall state, and it does not start another executable.

## API and ownership

```go
func Run(ctx context.Context, cfg Config, initial Input, updates <-chan Input,
    effects Effects, ready func()) error
func Validate(Config) error
func ValidateInput(Config, Input) error
func ValidateSnapshot(Config, Input, Snapshot) error
```

The three validators are pure. Immutable `Config` contains resource, underlay
interface, answer selection, attempt timeout and retry interval. The verified
module loader attaches `ConfigDigest`; that digest covers the complete private
instance, including dependency references and the coordinator's tunnel policy.
The digest is excluded from `Config` JSON so it does not hash itself.

`Input` is a full replacement of the coordinator's authorized dependencies:

| Input | Contract |
| --- | --- |
| Resource, interface, configuration digest | Must match the immutable worker instance exactly |
| Generation | Nonzero and increasing when endpoint dependencies change; lifetime renewal keeps the generation and the active DNS question |
| Active | False withdraws endpoint eligibility while the worker remains operational |
| Source | One canonical global IPv6 address with absolute preferred and valid deadlines |
| Candidates | Ordered configured or DHCPv6 AFTR names/literals, each with its own deadline; maximum four |
| Resolvers | Ordered literal IPv6 addresses, ports and receiving-interface scope, each with its own deadline; maximum eight |

The coordinator must first observe the source installed and owned on the
underlay, with DAD completed, no duplicate-address failure and positive
preferred/valid lifetime. This worker does not infer eligibility from an
address string or discover another source when it disappears. An empty source,
empty candidate set, or absent resolvers while using only names is an explicit
waiting state. A literal candidate needs no resolver.

The public service compiler supplies RA/DHCPv6 dependencies by service name,
not by arbitrary file paths. A DHCPv6 AFTR name or DNS server is bounded by its
information-refresh deadline and, for a stateful lease, lease expiry. RA-derived
DNS and source addresses retain their independent advertised deadlines. The
coordinator must also enforce producer ownership and revalidate current input
when accepting and projecting a snapshot. Projection intersects the published
endpoint deadline with the current source/candidate/resolver deadlines, so a
shorter budget takes effect immediately without discarding a usable endpoint.
A lifetime renewal does not start another DNS query. For the default `first`
selection, a DNS refresh retains the working remote when it remains in the new
answer set; rotating RRset order alone must not interrupt established flows.

`Effects.Update` receives the entire `Snapshot`, including the exact input
generation and digest, selected source/remote, candidate/resolver indexes,
ordered DNS answers, expiry, attempt/failure counters and explicit error text.
An unavailable snapshot contains no installable endpoint facts. Callback
failure is fatal; silently losing a withdrawal is not an accepted outcome.

`ready` runs after validating the initial input and publishing the initial
snapshot. It means the worker is ready to process dependencies; it does not
mean DNS succeeded or that a socket or tunnel already exists. DNS sockets are
opened per attempt. A nil update channel supports a fixed initial input; closing
a non-nil channel is an error because its dependency producer disappeared.

Cancellation closes active DNS sockets, joins lookup goroutines, and sends one
`Active=false`, unavailable snapshot through a cleanup context bounded to one
second. Effects must honor their contexts. A malformed update, changed content
without a new generation, stale generation or failed callback terminates the
worker explicitly. A DNS failure is reported in status and retried.

## Bootstrap and DNS transport

Every resolver must be an IPv6 literal scoped to this underlay. IPv4 servers,
loopback, multicast, unspecified addresses, resolver hostnames, embedded zone
strings and a different interface are rejected. Ports are explicit or default
to 53. Canonical global IPv6 literals, including ULA addresses, are permitted
as sources and AFTR endpoints; link-local AFTR endpoints are rejected.

UDP and TCP sockets use `SO_BINDTODEVICE`. Global resolver queries also bind
the selected global source. Link-local resolver queries carry the explicit
interface zone and use the kernel's link-local source selection on that device.
The connected socket filters the peer address and source port. There is no
`resolv.conf`, NSS, IPv4 DNS, local recursive resolver or host fallback. This
keeps name resolution on the IPv6 underlay and avoids a dependency on the
IPv4 tunnel it is intended to create. The compiler rejects service-reference
cycles; it must also make underlay reachability available before DNS can work.

Each query asks for AAAA/IN and advertises an EDNS UDP size of 1232 bytes. The
response must match ID, opcode and the exact question. A valid truncated UDP
response is retried over TCP; truncated TCP responses fail. CNAME traversal is
bounded to eight links and retains the original absolute CNAME expiry across
additional queries. Loops, conflicting targets, a CNAME and AAAA at the same
owner, unrelated owners, invalid IPv6 endpoints and oversized answer sets
cannot become tunnel facts.

AAAA wire order is preserved after deduplication. All members of an AAAA RRset
use its shortest TTL, as specified for explicitly configured resolvers by
[RFC 2181 section 5.2](https://www.rfc-editor.org/rfc/rfc2181.html#section-5.2).
The complete result is limited to 64 unique addresses and 256 answer records.
Zero TTL, including a TTL with the high bit set, cannot authorize a persistent
endpoint and is an explicit resolution error. It never means infinite life.

## Selection, expiry and refresh

Candidates are tried in input order. Each name tries eligible resolvers in
input order. A failed name or server allows the next candidate/server to be
attempted. A successful fallback retains the earlier failure explanation in
status and periodically retries higher priorities. The default `first` policy
selects the first live AAAA in response order. `ordinal` and `ordinalModulo`
are explicit options that select from lexically sorted canonical IPv6 strings;
their ordinal is one-based, with zero selecting the default ordinal one.

The installed endpoint's expiry is the earliest of the source preferred
deadline, source valid deadline, candidate deadline, resolver deadline, AAAA
RRset TTL and CNAME-chain expiry. Zero means an explicitly infinite dependency,
and only a literal with entirely infinite dependencies can be infinite.

A successful DNS answer schedules refresh at half of its remaining DNS TTL,
capped at 30 seconds and floored at 100 milliseconds. The default attempt
timeout is three seconds and the default failure retry is five seconds.
Configured timeout/retry ranges are 1–30 and 1–300 seconds. Each attempt is
additionally bounded by the remaining dependency lifetime. Candidate/resolver
attempts run sequentially in a cancellable lookup goroutine; input changes and
expiry continue to be processed while DNS is waiting.

On refresh failure, the last successful endpoint remains usable only until its
existing deadline. A retry, input poll, status publication or lifetime-only
dependency update does not renew a cached DNS answer. A new positive DNS
response can renew its TTL. Source deprecation withdraws the endpoint even if
the address is still valid. Changing the source, candidate order/identity,
resolver order/identity or active state invalidates the cached selection.

Input updates cancel previous lookups. Each result must match both its launch
sequence and the current input generation, so a delayed old reply cannot
replace a newer selection. The coordinator validates the same generation again
at the IPC boundary and checks current dependency facts during projection.

DNS receipts and expiry use the originating Linux `CLOCK_BOOTTIME`, including
suspended time. Inputs retain their actual deadline kind: operator UTC cutoffs
or original boot budgets. `internal/lifetime.Deadline` permits only one kind per
value, and scopes infinity to a boot when a protocol supplies it. DHCPv6 budgets
are passed through unchanged; RA-derived inputs still retain the RA client's
UTC deadline contract, whose publisher conversion remains separate work.

The machine keeps one immutable input, the selected DNS answer and its original
budget. It no longer translates a second input, maintains a wall/boot mapping,
or rebases outputs on clock steps. `Snapshot.BootID`, `ObservedNanos` and each
answer's `ReceivedNanos` identify their original clock; `UpdatedAt` is display
metadata. Minimum-of-dependencies selection returns an original deadline.
UTC steps cannot refresh a BOOTTIME answer or DHCPv6 input, while an explicit
operator UTC cutoff is still enforced against UTC. Endpoint snapshots carry
these same deadlines to the coordinator and B4 address writer. Autonomous
DS-Lite link/route fencing while control is frozen remains unfinished.

## Migration from the pinned Linux implementation

The source audit uses routerd revision
`1329ef54f296507eda79bbbc3b853783dd27251e`. The active Linux controller is
`pkg/controller/chain/phase2.go` (`DSLiteTunnelController`), constructed by
`pkg/controller/chain/chain.go`. Its candidate order is DHCPv6 AFTR name,
configured AFTR FQDN, configured AFTR IPv6, then `remoteAddress`; it tries later
candidates after resolution failure and selects the first returned IPv6 result.

The similarly named `cmd/routerd/runtime_network.go` helper has a different
candidate order and ordinal selection. Its production caller is the FreeBSD
apply path, `cmd/routerd/freebsd_apply.go`. Those differing behaviors must not
be treated as one universal legacy contract. This package defaults to the
Linux selection behavior; sorted ordinal selection requires explicit opt-in.

Two intentional migration differences require operator review:

- The old Linux pending state could retain an existing tunnel indefinitely
  while name resolution or local-source discovery failed. This implementation
  retains a last-good DNS endpoint only to its original bounded expiry, then
  withdraws it. Source preferred-lifetime expiry also withdraws it.
- The old Linux controller resolved through the local DNS service. This
  implementation requires explicit or learned IPv6 underlay DNS servers and
  rejects a recursive bootstrap dependency through the tunnel itself.

The migration compiler must report these differences and must reject an old
input whose other semantics it cannot preserve. This package does not by
itself establish full legacy DS-Lite or deployed homert02/03 compatibility.

## Scope and verification

This is a B4 endpoint resolver, not the provider-side AFTR NAT implementation.
It does not probe endpoint health, select by RTT, maintain a hold-down policy,
validate DNSSEC, or support DNS-over-TLS/HTTPS, DNAME, IPv4 DNS or IA_NA source
acquisition. Linux owns the data plane and PMTU/encapsulation; network intent
separately controls forwarding, firewall policy and any NAT.

Pure/race tests cover bootstrap validation, exact input generations, stale
queries, min-of-dependencies expiry, original TTL retention, finite fallback,
source deprecation, cancellation, malformed DNS chains, RRset lifetimes,
selection policies, bounded forged snapshots and clock changes. The DNS parser
has a bounded fuzz target.

The native test first enters fresh user/network namespaces and verifies it is
outside the parent network namespace. An independent IPv6 DNS peer exercises
UDP/TCP, truncation, CNAME expiry, wire order, wrong question/ID/source port,
silence, recovery through a literal and source expiry. Required mode fails if
isolation is unavailable; a skip is not native socket evidence.

```sh
go test -race ./internal/protocol/aftr
go test -run '^$' -fuzz '^FuzzAFTRDNSAnswerScope$' -fuzztime=10s ./internal/protocol/aftr
ROUTERD2_INTEGRATION_REQUIRED=1 go test -race -tags integration -run '^TestIsolatedNativeAFTR$' -v ./internal/protocol/aftr
```

`internal/integration/aftr_test.go` exercises the actual two-binary process tree:
independent IPv6 DNS, tunnel/B4/route creation, an updated answer, last-good
expiry during a DNS outage, recovery and configuration removal. Its dedicated
no-NIC guest target is `make qemu-daemon-aftr`; transcripts and artifact hashes
are written to `dist/aftr-daemon-test`. The test initializer is not part of a
production image.

Protocol references: [RFC 6333](https://www.rfc-editor.org/rfc/rfc6333.html),
[RFC 1035](https://www.rfc-editor.org/rfc/rfc1035.html),
[RFC 2181](https://www.rfc-editor.org/rfc/rfc2181.html), and
[RFC 3596](https://www.rfc-editor.org/rfc/rfc3596.html).
