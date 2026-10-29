# Time service

One Go worker owns one fixed foreground chronyd 4.8 process, its private mount
namespace/root, persistent drift and typed tracking observations. The archive
contains its complete ELF closure, notices and corresponding sources. The
supervisor supplies the pinned archive by descriptor; systemd is not required.
Host/container mode uses `-x`, while appliance mode owns clock discipline.

Client and server source groups share that clock owner. A group declares
`servers`, optional DHCPv4/DHCPv6 `sources_from`, and an optional finite-fact
`when`. Live learned sources replace that group's configured fallback; they do
not replace other always-static groups. A condition removes only its group,
not the LAN listener. This retains routerd's independent chrony fragments and
`resolveServers` selection without a second clock writer. The legacy importer
joins NTPClient/NTPServer declarations and uses declared local DNS listeners
for hostname sources. Conditional server listeners and dynamic access-prefix
imports remain unsupported and are rejected rather than silently changed.

Learned servers retain their original information/lease BOOTTIME deadlines.
Conditional activation has its own original fact deadline. The worker expires
both independently of status/control availability and reloads the source file
through a directly owned Unix command connection. Static configuration is
prepared once. Tracking uses a separate connection; no helper is executed per
query/reload. Only the private reply directory is shared with the engine.

`make check-ntp-dhcp6` exercises independent ISC/chronyd/keepalived peers,
original expiry, reacquisition, role changes and stopped-control expiry.
`make check-ntp-import` additionally uses the product importer and normal apply
path with the public homert-shaped fixture: three static peers, a conditional
learned/fallback group, DNS hostname resolution, the same chronyd across
expiry/role changes, independent LAN NTP and IPv4/IPv6 TCP/UDP with capture.
These namespace checks disable host clock adjustment. `make qemu-ntp` uses the
packaged Alpine/PID 1 image and a deliberately slow virtual RTC to check actual
clock correction, client acceptance, persisted drift and reboot.

Synchronization and client-acceptable time quality are different: tracking
includes frequency uncertainty (`skew_ppm`), root delay and dispersion. A
synchronized flag alone does not establish client acceptance. The standard LAN
client's timeout and quality criteria are retained.

Remaining acceptance includes real DHCPv4-learned NTP, IPv6 LAN access denial,
slew/outage behavior, unsynchronized TLS policy, complete homert integration
and target hardware. Intermittent supervisor-observation expiry and a previous
appliance worker startup timeout are tracked in the handoff; a later passing
run is not their cause fix.
