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
`resolveServers` selection without a second clock writer.

Learned servers retain their original information/lease BOOTTIME deadlines.
Conditional activation has its own original fact deadline. The worker expires
both independently of status/control availability and reloads the source file
through a directly owned Unix command connection. Static configuration is
prepared once. Tracking uses a separate connection; no helper is executed per
query/reload. Only the private reply directory is shared with the engine.

`make check-ntp-dhcp6` exercises independent ISC/chronyd/keepalived peers,
original expiry, reacquisition, role changes and stopped-control expiry.
These namespace checks disable host clock adjustment. `make qemu-ntp` uses the
packaged Alpine/PID 1 image and a deliberately slow virtual RTC to check actual
clock correction, client acceptance, persisted drift and reboot.

Synchronization and client-acceptable time quality are different: tracking
includes frequency uncertainty (`skew_ppm`), root delay and dispersion. A
synchronized flag alone does not establish client acceptance. The standard LAN
client's timeout and quality criteria are retained.

Chronyd saves frequency and uncertainty on each clock update (`driftfile
interval 0`). A 60-second write throttle left an untrained estimate on disk
after the live clock had calibrated; an actual KVM power cut then restored
1000000 ppm uncertainty and the independent client rejected NTP until a new
reference arrived. This uses chronyd's own update and atomic rename, with no
second state writer or polling loop. It does not change source polling or
claim that synchronization alone establishes time quality.

The learned-source peer run also resolves homert's two configured fallback
hostnames through an independent dnsmasq process and captures those queries and
the resulting NTP traffic. It does not exercise homert's conditional DNS/DoH
service itself. Remaining acceptance includes real DHCPv4-learned NTP, IPv6 LAN
access denial, long-outage holdover/slew, unsynchronized TLS policy, complete
homert integration and target hardware. A previous appliance boot with ordinary
link-local DAD repeatedly restarted RA/DHCPv6 workers; repeat that boot without
NoDAD remains an acceptance item.

The native single-router `make check-uplink UPLINK_NTP_OUTAGE=1` blocks all router-originated
UDP/123 upstream requests after real synchronization. It confirms actual drops,
LAN client acceptance during holdover, IPv4/IPv6 DNS and HTTPS, then a newer
chronyd reference on the same worker after restoring traffic. Polling and client
quality limits retain their normal settings. Capture includes actual upstream
requests/replies and LAN responses. This short outage check leaves long-outage
quality and appliance clock discipline for their own acceptance.
