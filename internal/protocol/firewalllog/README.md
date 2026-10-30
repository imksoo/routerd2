# Firewall packet logging

The network worker installs optional typed `PacketLog` expressions on existing
firewall rules and terminal policies. Group zero keeps Linux kernel logging;
a nonzero group sends NFLOG packets to one `firewall_log` service. The existing
routerd `pkg/nflog` reader handles the kernel messages. The collector also owns its packet-history database; no separate daemon or
delivery registry is added.

The collector records `firewall packet` through the normal product log handler,
with action, rule prefix, original packet timestamp, source/destination,
ports, protocol, TCP flags, address family, interfaces, and original packet size. It does
not retain packet payload. Local records have their normal worker identity and
log cursor; independently configured syslog/OTLP services can forward them.
The service compiler assigns `state/services/<service>/firewall-events.db`;
configuration cannot redirect that file to a shared or legacy database. The
existing routerd `pkg/logstore.FirewallLog` keeps the original packet timestamp
and decoded fields. It does not copy the legacy file or retain packet payload.
A single history loop inserts batches and expires them. Its 512-entry queue
keeps disk and archive waits off the NFLOG reader; full queues and failed
batches produce warnings and lose those history records. Forwarding continues.
Shutdown stops packet ingestion first and drains queued history within a
2-second write budget.

An optional age retention policy uses the existing `pkg/logstore.ApplyRetention`
implementation. The default sweep interval is 24 hours. Every expired row is
submitted to configured UDP syslog destinations before deletion; a reported
send error keeps the rows. UDP submission is not receiver acknowledgement.
Partial exports may be repeated after a later failure. Retention and insertion
share this service's history loop, so insertion cannot add an old packet between
export and deletion. Maintenance can fill the bounded queue; this is operational
history, not a lossless audit archive.

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
does not change verdicts or create a new forwarding destination. The legacy path maps to the service-owned history database, while normal
packet diagnostics still use the existing product log owner.
`LogRetention.signals: [firewallEvents]` preserves the configured age, daily
sweep, vacuum setting, and supported UDP pre-delete archive destinations.
The legacy OTLP LogSink sent nothing; it is diagnosed and skipped rather than
counted as an archive destination. Telemetry remains independently configured.
DPI, expired-connection correlation, and metrics/traces export are not implied.
Other retention datasets remain explicit unsupported-import diagnostics until
they have their own history owners.

## Repeatable communication check

From the repository root, with Go, `sudo`, `tcpdump`, and Linux network
namespaces available:

```
make build RELEASE_ID=firewall-history-check
python3 scripts/daemon-tests.py firewall --test TestDaemonFirewallPacketLogs
```

The fixture uses an independent UDP client and echo endpoint. It checks IPv4
and IPv6 acceptance, silent drop, ICMP rejection, original packet tuples in
local records, persisted history across restart, age expiry, and removal without
changing packet policy. An independent UDP receiver checks an expired row's ID,
original timestamp, and tuple. Newer rows remain. The fixture sets an 8-second
age and changes only the sweep interval from daily to one second, while both IP
families keep exercising allow, drop, and reject. Import coverage separately
checks the actual 7-day policy without waiting seven days. Static
IPv6 fixture DAD must complete before these logging checks; this is not a
cold-start address-readiness qualification. The harness removes its private
namespace and temporary binaries. Use `--output` only while investigating a
failure, and remove the retained output after integration.

Verified with release `firewall-history-2c6c232-final` on 2026-10-30 using Linux network
namespaces: `TestDaemonFirewallPacketLogs` passed with group 1, copy range 2048,
accept sampling zero and deny sampling one. An archive-only unreachable route produced a real send failure: expired rows
remained while two communication rounds continued in both families and recorded
new packets. Restoring that route allowed pre-delete UDP receipt, expiry and
newer-row retention. Actual TCP SYN flags survived in both families. Restart
persistence and logging removal also passed. The test took 14.22 seconds
(15.25 seconds including namespace setup/teardown). The independent endpoint uses static fixture addresses
and disables its own IPv6 DAD; product IPv6 DAD completion is observed normally.
This confirms the stated packet/history path, not automatic address acquisition,
complete homert migration, physical-appliance qualification, disk-failure recovery,
or sampled-log rate accuracy.
