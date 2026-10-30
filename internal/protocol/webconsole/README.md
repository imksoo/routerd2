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
history search, correlated client inventory, connections, protocol detail, VPN/SAM pages,
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
