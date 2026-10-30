# Read-only operator console

A configured `web_console` service runs in the existing Go module executable.
It owns its TCP listener and HTTP lifetime; there is no external web server,
JavaScript build, second resource store, or systemd service.

```yaml
schema_version: 2
resources:
  - id: service/console
    service:
      name: console
      web_console:
        listen: 198.18.0.1:8080
        base_path: /ops/
        title: routerd2
```

Declare the local address and its firewall access separately. The listener uses
HTTP, as the original read-only WebConsole does. `ManagementAccess` import checks
its declared trusted interface/zone and binding; it does not invent a source ACL.

The initial pages display live control health, scoped kernel interfaces/routes,
resource-condition pages, current DHCPv4 leases, and confirmed-commit metadata. Health failure remains
HTTP 503 with the actual observation. Missing observations and truncated kernel
views remain explicit. Resource condition readiness is not a kernel readback.
The resource continuation uses the control API's existing observation identity;
a concurrent change returns 409 and the operator can start a fresh page.

The DHCP page lists configured DHCP services from `/v1/services` and reads the
selected service's complete typed observation from `/v1/services/{name}`. It
shows hostname, MAC/IP, lease deadline, serving/standby and synchronization state.
The observation's original boot-clock deadline determines whether the view is
current. A lease is not evidence of present connectivity; released leases are
removed by the DHCP owner, and this page does not store or invent a history.

The old console's title, base path, links, escaped HTML and read-only method
handling are retained. Its resource-store adapter and React status model are
replaced by the current typed control APIs. This is an initial management path:
correlated client inventory, further protocol detail, WireGuard/SAM pages,
and the old complete UI surface are still outstanding. Configuration documents,
mutations, and private worker APIs are not exposed by this listener.

## DHCP console acceptance

On 2026-10-30 at 20:38:32–20:38:46 JST, the management communication check ran
against base `eb66441a4f29` plus this console change. The embedded release identity
is the base ID, not a claim that the unmodified base has the new page. Build the
standard artifacts and bundled engine with `make build dnsmasq-engine`, then run
`python3 scripts/daemon-tests.py management --output dist/web-history-management-final`
(or use `make check-management` without retaining artifacts).

A separate BusyBox client acquired its address from the configured dynamic pool.
The independent HTTP client read the actual hostname, MAC, address and deadline;
a requested renewal changed the displayed deadline, and a real DHCPRELEASE
removed the row. The management peer retained its explicitly configured address;
the DHCP client used a separate initially unaddressed interface. The same run
passed existing SSH commit/confirmation/rollback and control-process recovery
checks in 16.34 seconds with the race-enabled integration harness. The original
75-second child and 100-second harness limits were unchanged.

The management fixture now uses the existing network-namespace isolation helper:
root checks use a private network namespace with the engine's normal privilege
setup. Its former user namespace with denied setgroups could not start dnsmasq.
The product privilege behavior was not changed. The acceptance compares decoded
HTML text, preserving normal escaping of the timezone's plus sign.

SHA-256 of the tested standard build artifacts:

| Artifact | SHA-256 |
| --- | --- |
| `routerd` | `9269af7a4b7f3cec3e14aec1e50716ad70fd967d08db9e88ac0c2fd3680d0ea7` |
| `routerd-modules` | `b9a7890c35dcec2e31d96416b708fb4cdbabf2195f0d65c77fd48ba2735b88b6` |
| `routerd-logging` | `31f0349d7486829c95e31ad2a31301b487614615375fc0b0b2a42e1613df673e` |

The captured HTTP page was rendered at 1200×800 and 500×844; all five columns
wrapped without overlap. Historical connection search, correlated client history,
HA synchronization display under failure, and physical homert acceptance remain
separate work. This short client-requested renewal does not establish natural
lease renewal over the normal operating interval.

## Saved communication search

The communication page reads the collector-owned SQLite history through
`GET /v1/services/{name}/traffic-history`. The runtime resolves the configured
service path and releases its mutex before opening the database read-only.
It reuses routerd's `OpenTrafficFlowLogReadOnly` and `TrafficFlowLog.List`;
there is no second writer, copied history, or query process.

Searches accept exact client/peer IPs, protocol, and inclusive RFC3339 start-time
bounds. Equivalent IPv6 spellings are normalized. The default is the newest
100 rows, with a requested maximum of 1000; one extra row determines whether
the result was truncated. A visible notice asks the operator to narrow the
search when necessary. This is a bounded search, not a stable paged export.
The query has the original console's 1500 ms database budget, independently of
network application. Missing or unreadable history returns HTTP 503 instead
of pretending that no communication occurred.

The page shows original endpoints, translated address, classification engine,
DNS/TLS/HTTP names, accounting availability and packet/byte counters. A missing
end observation is not labelled as current connectivity. The existing
`make check-traffic-flow` fixture includes independent HTTP requests against
the actual product while real TLS/UDP traffic populates the existing database.

## Tailscale observation view

The VPN page uses the revision returned by `/v1/services` to read the existing
typed configuration document. Only interface, hostname, advertisement intent,
route acceptance and inbound-blocking policy enter its template. Control server
URLs, authentication files and persisted engine state are not display data.
The selected canonical service name reads `/v1/services/{name}` so a truncated
list observation does not hide peer details. No CLI polling or second store is
introduced.

Membership, addresses, peers, allowed peer prefixes and byte counters come from
the existing Tailscale observation. Its original boot-clock deadline applies to
the whole observation, including peers. Expired data is labelled as the last
observation, with current connectivity unconfirmed. A configured but inactive
service remains unobserved. Known authentication states are shown without an
authentication URL. Advertised local routes are explicitly configuration intent,
not a claim that the control server approved them or that forwarding works.

`go test -race ./internal/protocol/webconsole` checks actual Unix HTTP reads and
escaped HTTP rendering for current, expired, authentication-required and missing
observations. These focused checks use recorded observation inputs and do not
establish real Tailscale forwarding; the existing `check-tailscale` and
`check-tailscale-homert` communication checks own that acceptance.

### Tailscale console communication acceptance

On 2026-10-30, the integrated product at `b32a43c` with release identity
`wsl-console-b32a43c` passed the existing Tailscale communication checks on a
separate Windows machine running Ubuntu under WSL. The normal run completed at
21:11:18–21:13:30 JST (132.4 seconds); the homert advertisement run completed at
21:13:57–21:14:43 JST (45.9 seconds). Both exited successfully. The acceptance
script change was integrated as `305fbc2`; removing an unused helper return in
`735682a` did not change the product or acceptance conditions.

The standard signed product bundle ran as PID 1 in its own mount, network and
process namespaces. The independently checked peers were Tailscale 1.104.2 and
Headscale 0.29.6. An independent LAN client read the actual product console and
matched its observed membership, both address families, peer and configured
advertisements. The console did not expose enrollment keys, their file paths,
control URLs or registration URLs. Whole-product restart retained both the
actual communication and displayed membership. Revocation displayed the need
for reauthentication; reauthentication restored traffic and the observed view.

The homert case advertised all three configured RFC1918 prefixes. The peer's
kernel routes and both-direction LAN TCP/UDP worked without subnet NAT or
manual return routes. Both IP families also passed exit traffic through the
declared WAN NAT. Removing the Tailscale configuration withdrew routes and
connectivity and displayed no configured Tailscale service. Reapplying it
restored routes, communication and the console view. Packet captures backed the
existing communication checks. Physical homert02/03 acceptance, physical WAN
switching and long-duration operation remain separate work.

Use the existing `make check-tailscale` and `make check-tailscale-homert` targets.
With the same standard build, engine and peer prerequisites already present,
run `python3 scripts/tailscale-peer.py` and the same command with `--homert`.
The scripts create and remove their own isolated network, process and mount
namespaces. The recorded outputs were `dist/vpn-accept.log` and
`dist/vpn-homert-accept.log`; one actual HTTP response was retained temporarily
as `dist/vpn-live.html` for layout review.

SHA-256 of the product used by both checks:

| Artifact | SHA-256 |
| --- | --- |
| `routerd` | `dd5a345ced5192c09b0bfcb73e9ab5e4cb33070b340d995f916e77fa852ab678` |
| `routerd-modules` | `61230e7c61ac672f6621d3ca9ff934d0d5162ba2547a8002229624b6862e3eeb` |
| `routerd-logging` | `4be6777af1a9daf85b4a2c4017120949428ebbcdb8ad6fadc8e889591fc6c798` |

### History search communication acceptance on a separate machine

On 2026-10-30 at 21:14–21:16 JST, the standard product build `b32a43c`
(`wsl-console-b32a43c`) passed both existing traffic-flow checks on Ubuntu
26.04 under WSL 2, Linux 6.18.33.1, on a separate Windows computer. The fixture
was `735682a`; each check completed in 22.15 seconds with the race detector.
The three product SHA-256 values are identical to the Tailscale checks above.
The same build also passed the existing management check in 14.41 seconds.

Independent LAN TCP/UDP traffic crossed source NAT, and an independent HTTP
client searched the collector's real stored rows by client address, protocol
and original start time. Built-in and reused nDPI classification both retained
real TLS names and counters. The console returned unavailable, rather than an
empty success, while storage was unusable; actual forwarding continued. After
whole-product restart the original connection was still searchable. After
closure its original end time appeared, and retention removal disappeared
from the HTTP result. The existing UDP archive check also passed.

Reproduce through `make check-traffic-flow` after the standard build. The
recorded output was `dist/wsl-flow-console/`. A later nDPI run captured one real
HTTP response for local layout review using a temporary test-only write; that
write was removed and the fixture restored exactly. These results establish
the saved-history operator path, not full homert natural renewal, HA, hardware
qualification, or the remaining client-inventory and protocol UI features.

Both real HTTP responses were rendered offline in a dedicated browser profile
and inspected at a desktop width of 1280 pixels. The initial existing-browser
inspection tool stopped because it could not identify the current URL; the
offline renderer opened only the saved local responses. A subsequent wording
correction changed Japanese labels/errors only, without changing data access,
protocol handling or the acceptance conditions.
