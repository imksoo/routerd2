# Firewall packet logging

The network worker installs optional typed `PacketLog` expressions on existing
firewall rules and terminal policies. Group zero keeps Linux kernel logging;
a nonzero group sends NFLOG packets to one `firewall_log` service. The existing
routerd `pkg/nflog` reader handles the kernel messages. No separate daemon,
packet queue, database, or delivery registry is added.

The collector records `firewall packet` through the normal product log handler,
with action, rule prefix, original packet timestamp, source/destination,
ports, protocol, address family, interfaces, and original packet size. It does
not retain packet payload. Local records have their normal worker identity and
log cursor; independently configured syslog/OTLP services can forward them.
Kernel logging is best effort. A stopped or overloaded collector cannot delay
forwarding or change a packet verdict. The reader checks cancellation at least
every second when idle, and closing it releases its NFLOG group.

`PacketLog.Every` is a random one-in-N logging divisor; zero/one records every
match. Logging runs in a separate rule immediately before the unchanged
counter/verdict rule. A sample mismatch cannot cause the packet to bypass its
accept, drop, or reject verdict. `CopyRange` controls copied packet bytes;
zero retains the receiver's copy limit. The prefix reserves room for an
unambiguous action label within Linux's 128-byte NFLOG prefix field.

## Legacy import

`FirewallEventLog` enables an NFLOG reader and converts existing
`FirewallPolicy.logDeny` and `FirewallRule.log` expressions to its group.
Group zero in the old declaration defaults to one. The old deny sampling
applies to implicit policy drops; explicit rule logging remains unsampled.
The current homert configuration uses accept sampling zero, drop sampling one,
copy range 2048, and group one. Accepted-packet sampling that feeds the old DPI
flow classifier is explicitly unsupported; it is not silently converted into
packet history.

The old Linux logger did not consume `sinks` or `log.reject`. Their presence
does not change verdicts or create a new forwarding destination. Its dedicated
SQLite path now maps to the product's existing bounded diagnostic log store.
This feature does **not** establish old firewall-history age retention, DPI,
expired-connection correlation, or metrics/traces export.

## Repeatable communication check

From the repository root, with Go, `sudo`, `tcpdump`, and Linux network
namespaces available:

```
make build RELEASE_ID=firewall-log-check
python3 scripts/daemon-tests.py firewall --test TestDaemonFirewallPacketLogs
```

The fixture uses an independent UDP client and echo endpoint. It checks IPv4
and IPv6 acceptance, silent drop, ICMP rejection, original packet tuples in
local records, restart, and removal without changing packet policy. Static
IPv6 fixture DAD must complete before these logging checks; this is not a
cold-start address-readiness qualification. The harness removes its private
namespace and temporary binaries. Use `--output` only while investigating a
failure, and remove the retained output after integration.

Verified with release `firewall-log-6836afb` on 2026-10-30 using Linux network
namespaces: `TestDaemonFirewallSourceCIDRs` and `TestDaemonFirewallPacketLogs`
passed with group 1, copy range 2048, accept sampling zero and deny sampling one.
The packet-log test completed in 6.04 seconds (7.07 seconds including namespace
setup/teardown). This confirms the stated packet/log path, not complete homert
migration, physical-appliance qualification, or sampled-log rate accuracy.
