# Native Linux network engine

This package is the first native data-plane implementation. It is not feature
parity with the original routerd resource API. It uses rtnetlink, nfnetlink and
fixed procfs sysctl files directly; it does not execute host commands.

## Current behavior

- Bring existing interfaces up, set an explicit MTU, and add/remove explicitly
  managed IPv4 and IPv6 addresses.
- Manage simple unicast static routes with an optional gateway, table and metric.
- Enable or disable IPv4 and IPv6 forwarding only when requested explicitly.
- Replace only the `inet routerd2` nftables table in one kernel batch. The
  forwarding chain drops invalid connections, accepts established/related
  traffic, and accepts explicitly configured ingress/egress interface pairs.
  Its default forwarding policy is **drop**. Input and output are not filtered.
- Optionally masquerade explicit IPv4 or IPv6 source prefixes on an output
  interface. NAT does not imply a forwarding permission.
- Create a native IPv4-in-IPv6 `ip6tnl` DS-Lite B4 tunnel with static underlay
  endpoints. Addresses, routes, forwarding permission and optional NAT are
  separate declarations. AFTR discovery, DHCP-derived endpoints, multiple-AFTR
  selection and an AFTR implementation are not implemented here yet.

`Firewall: nil` leaves an unowned firewall untouched, but removes a table that
this engine previously created and recorded. A non-nil empty firewall enables
the default-drop forwarding chain. Global forwarding changes affect the whole
network namespace. The caller must ensure this is the intended router namespace.

## Ownership and failures

The state file is an ownership journal, not a configuration backup. Hold its
directory in persistent storage and retain it across process and image upgrades.
The engine locks it against a second instance using the same path.

An existing address or route is not silently adopted, even if it looks identical.
The engine needs both journal ownership and a compatible live object before
removing it. Route protocol 242, the tunnel alias and the nftables ownership
marker are additional checks, not sufficient evidence on their own. Unrelated
routes, addresses and nftables tables are retained. Whole-link deletion refuses
remaining address or route references, including automatically created addresses
that cannot be distinguished safely from externally installed addresses.

For MTU, interface UP and forwarding settings, the journal stores the original
value and the most recently written value. Relinquishing management restores the
original value only when live state is still the last value or already the
original value; conflicting external changes produce an error. This is a
single-writer design: cross-process races that occur between an observation and
a netlink operation cannot be fenced using this journal.

Only the nftables replacement is atomic. Interface, address, route, tunnel and
sysctl operations may partially succeed. After each successful mutation the
journal is synchronized, but a crash between the kernel operation and journal
persistence can leave an unclaimed object. The engine fails conservatively on
such a collision rather than inventing ownership. `ApplyError.Partial` and a
`degraded` snapshot report failed reconciliation; they do not promise that the
previous state has been restored.

The control layer owns revision concurrency. `Apply` accepts equal and lower
revision numbers to repair drift or reconcile the last committed configuration
after a failed candidate. Such a rollback is another reconciliation and can
itself fail. Deleting a primary IPv4 address that could implicitly delete another
address is refused. Tunnel replacement and route changes are not hitless.

## Validation

`go test ./internal/network` uses an in-memory kernel fake and never mutates the
host network. It exercises restart ownership, failed-candidate recovery, lower
revision reconciliation, before-image restoration, conflict detection and
foreign-resource preservation.

`CGO_ENABLED=0 go test -tags integration ./internal/network -v` includes an
isolated native-kernel test. The parent re-executes the test in a fresh user and
network namespace before any mutation; the child verifies its network namespace
differs from the parent. It builds client/router/server veth topology and checks
IPv4/IPv6 forwarding, NAT, reverse-connection filtering, IPv4-in-IPv6 traffic and
cleanup. A restricted runtime skips the native test when it cannot create the
isolation. Set `ROUTERD2_INTEGRATION_REQUIRED=1` in a suitable CI kernel to make
that restriction a failure. A skipped test is **not** kernel or packet-path
verification.
