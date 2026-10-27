# Native WAN router-advertisement client

This package receives ICMPv6 router advertisements and emits a complete typed
snapshot. It does not install addresses, routes, DNS configuration or sysctls,
spawn a process, or depend on systemd, journald, `ip`, `nft`, `rdisc6` or
`dhcpcd`. The control service's single network writer applies the snapshot.

## API and ownership

`Run(ctx, Config, Effects, ready)` owns one raw receive socket and one private
state journal for its lifetime. `Validate` and `ValidateSnapshot` are pure.
`Effects.Update(ctx, Snapshot)` receives the entire learned set, including
resource/interface/source/interface-ID identity and original boot-scoped deadlines.
A callback error is fatal; there is no silent partial-success fallback.

`ready` runs after opening the socket, restoring/persisting state and publishing
the initial snapshot. It does **not** mean that a router, prefix or default
route has been learned. `Active` distinguishes an operational worker from
address/route availability. Cancellation sends one `Active=false` snapshot
through a cleanup context bounded to one second. The callback must honor its
context. The journal deliberately retains the learned leases for same-boot
worker restart.

The runtime compiler supplies `StatePath`, an absolute path in private durable
storage. It is not a path received from a network peer or accepted as remote
configuration intent. The state and exclusive lock must be private regular
files; symlinks, oversized files, malformed JSON and concurrent ownership fail
explicitly. Updates use file and directory fsync with atomic rename.

Before starting, the network writer must bring the interface up, provide a
usable link-local address, and own these per-interface settings:

- `net.ipv6.conf.<interface>.accept_ra = 0`
- `net.ipv6.conf.<interface>.autoconf = 0`

The worker checks those settings but never changes them. A changed interface
index, selected source, Ethernet identity, missing address, or lost sysctl
ownership terminates the worker instead of silently using another device.
An initially selected source remains pinned even if another link-local address
appears. MTU changes are observed for subsequent option validation.

Nil feature flags enable SLAAC, default routers, on-link prefixes, Route
Information Options and DNS options. `AllowedRouters` restricts link-local
advertisers; it is not cryptographic authentication. `SourceAddress` optionally
pins the local link-local address. `InterfaceID` is a nonzero IPv6 suffix whose
upper 64 bits are zero, such as `::1234`. When omitted, Ethernet modified
EUI-64 is used; non-Ethernet links require an explicit ID.

## Protocol behavior

| Learned item | Snapshot representation and lifetime |
| --- | --- |
| Default router | `Routes` entry for `::/0`, link-local gateway, preference -1/0/1, independent Router Lifetime |
| Route Information Option | One route candidate per canonical prefix and advertising gateway; a default RIO overrides the same RA's header default |
| PIO autonomous prefix | Generated address with **/128** prefix length; only /64 PIOs generate addresses |
| PIO on-link prefix | Independent `Prefixes` entry; its L flag and lifetime do not imply that the address itself has a connected /64 |
| RDNSS | IPv6 DNS server with receiving-interface scope, last advertiser and its own lifetime |
| DNSSL | Ordered canonical ASCII/punycode search domain with its own lifetime |
| M/O flags, MTU, hop limit, reachable/retransmit timers | Observations, not network-writer mutations |

The /128 SLAAC representation avoids an implicit Linux /64 route when the
advertised L bit is clear. The projector must install the separate on-link
route when appropriate. `DADFailed` records a kernel duplicate-address failure;
the projector must exclude that address. A later RA renewal does not clear the
failure. The address identity must change before that candidate can be retried.

Each packet is validated completely before modifying learned collections:
source must be link-local, hop limit 255, ICMPv6 type/code and checksum valid,
and the receiving interface must match. Multicast destination must be
`ff02::1`; unicast destination must be the worker's selected link-local source.
Malformed option framing or known option lengths, invalid RDNSS/DNSSL encoding,
and duplicate RIO prefixes reject the whole packet. Unknown options and
semantic-invalid PIO/RIO entries are ignored. Prefix routes and generated
addresses exclude link-local, multicast, loopback and unspecified prefixes,
with `::/0` permitted as the default route.

An RA omitting a previously learned option does not erase that option. PIO
address lifetimes implement RFC 4862's two-hour safeguard; zero preferred
lifetime deprecates immediately, while zero on-link/default/DNS lifetime
withdraws the respective independent item immediately. An A or L flag being
clear does not withdraw a previously advertised item.

RDNSS/DNSSL entries are keyed by interface plus address/domain, not by router.
A later advertisement from another router updates or removes the same entry.
New entries are promoted to the front in wire order, existing refreshes keep
their position, and expiry does not depend on Router Lifetime. No resolver
repository is written by this package. Consumers of link-local RDNSS must use
the snapshot's receiving interface as their scope.

Learning is bounded to 128 addresses, 128 on-link prefixes, 256 route
candidates, 32 DNS servers and 32 search domains. A full DNS repository evicts
its earliest-expiring entry as required by RFC 8106; independent route/prefix
updates remain usable. Address/prefix/route capacity exhaustion still rejects
the advertisement and increments the rejection counter. Snapshots also
validate canonical identities, exact address derivation, uniqueness, feature
scope and bounded metadata at the IPC boundary.

## Time, expiry and restart

Learned lifetimes use Linux `CLOCK_BOOTTIME` and retain the originating boot
identity. `preferred_until` and `valid_until` are typed deadline values. Explicit
infinity has that boot ID and zero nanoseconds; an empty value is not learned
infinity. `ObservedNanos` timestamps publication, and `ReceivedNanos` identifies
the last accepted RA. `UpdatedAt` and `LastReceivedAt` are display metadata.
Publication, status events, wall-clock corrections and polling never rewrite
learned deadlines. There is no UTC conversion, clock anchor or rebasing state.

Journal format 2 contains the immutable configuration/link identity and the
same snapshot. The identity is prepared once at worker construction. A same-boot
restart reuses original deadlines and prunes expired facts. A reboot or identity
change discards learning and solicits fresh advertisements. Discarded prerelease
journals are not migrated.

The writer translates a finite route lifetime to Linux's relative timer at
installation. Its ownership journal keeps the actual CLOCK_MONOTONIC kernel
timer separately from original UTC/BOOTTIME authority. UTC correction therefore
does not look like foreign kernel modification. Long finite lifetimes get a
seven-day kernel grant renewed by the writer with one day remaining; the
original protocol deadline is unchanged. Infinite lifetimes have no finite
kernel timer. After actual suspend the running writer enforces BOOTTIME;
qualification with suspended/stopped writers remains incomplete.

Router solicitations use the configured link-local source, Ethernet source
link-layer option when available, hop limit 255 and `ff02::2`. The initial
series is at most three solicitations at four-second spacing with initial
jitter up to one second. Valid RA reception stops that series. Loss of all
previously learned routes through expiry starts another series.

## Scope limits

This is a native WAN RA client, not a replacement for the kernel's complete
Neighbor Discovery implementation. Linux still owns NS/NA, neighbour-cache
reachability and DAD. The package does not implement SEND, privacy/stable-opaque
address generation, duplicate-ID retries, redirects, DHCPv6 IA_NA, or a full
RFC 4191 reachability-driven router-selection algorithm. All live router
candidates and preference values are passed to the control service; its
selection/ECMP policy is separate. A source allowlist is not SEND. MTU and
other RA header observations currently do not reconfigure kernel timers.

## Verification

Pure tests exercise independent finite/infinite lifetimes, the two-hour rule,
A/L flag independence, multiple routers, RIO precedence, DNS repository order,
atomic malformed-packet rejection, capacity limits, DAD persistence, canonical
snapshot scope, EUI-64 derivation, wall-clock steps, same-boot restart, reboot
and identity invalidation, and durable exclusive state ownership. The parser
fuzz target requires every accepted packet to produce a valid bounded
snapshot and every rejected packet to leave learned data unchanged.

The integration test creates fresh user, network, PID and mount namespaces,
verifies their separation, then mounts a namespace-local procfs. Fixture
netlink/sysctl operations are guarded and never run in the host namespace.
It exchanges real raw RS/RA packets over a veth pair, checks hop-limit rejection,
full snapshots, absence of autonomous kernel address writes, restart,
zero-lifetime deprecation/withdrawal, independent expiry, shutdown withdrawal,
and explicit callback/ownership failures.

```sh
go test -race ./internal/protocol/raclient
ROUTERD2_INTEGRATION_REQUIRED=1 go test -tags integration -run '^TestIsolatedNativeRAClient$' -v ./internal/protocol/raclient
```

When container mount restrictions prevent private procfs creation, run the same
required test in an isolated appliance-kernel VM with **no external NIC**:

```sh
python3 internal/protocol/raclient/testdata/qemu.py --image-dir dist/image --output dist/ra-client-test
```

The VM launcher produces the serial transcript and a manifest containing the
kernel, initializer and test-binary hashes. It requires the exact test PASS
marker and rejects a skipped test. The test-only initializer is never included
in production images.

References: [RFC 4861](https://www.rfc-editor.org/rfc/rfc4861.html),
[RFC 4862](https://www.rfc-editor.org/rfc/rfc4862.html),
[RFC 4191](https://www.rfc-editor.org/rfc/rfc4191.html), and
[RFC 8106](https://www.rfc-editor.org/rfc/rfc8106.html).

`make check-ra-client` runs actual workers against an independent RA sender and
IPv6 TCP/UDP backend in private PVE namespaces, with bidirectional WAN capture.
`make qemu-ra-clock` additionally uses the dedicated no-NIC guest clock broker:
actual UTC +/-24h, unchanged frozen-producer notifications, RA/network worker
restarts, all owners stopped during kernel expiry, expired replay rejection,
fresh advertisement recovery and original guest-clock restoration. This does
not qualify physical suspend, target NICs or the complete WAN resource model.
