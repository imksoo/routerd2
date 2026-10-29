# Time service

The Go worker owns one fixed foreground chronyd process, private mount/root,
persistent drift directory and typed tracking observations. The engine archive
contains chronyd 4.8-r7, its complete ELF closure, notices and corresponding
sources. `make chrony-engine` builds it; the host needs no chrony service or
service manager. The supervisor supplies the selected archive by descriptor.

The server/iburst, bindaddress and allow configuration follows routerd's
`pkg/controller/chain/ntp.go`. The single time service avoids competing writers
to the system clock. Host/container mode always supplies `-x`. Appliance mode
allows clock discipline and initial `makestep`; that mode still needs VM and
target-device acceptance.

chronyd's command protocol requires filesystem Unix reply addresses. Queries
enter the running engine's pinned root through the same executable boundary
used for conntrack commands. Responses are decoded through the existing
`github.com/facebook/time/ntp/chrony` package and immediately narrowed to the
time service's typed snapshot. BOOTTIME observation/expiry and clock-adjustment
mode are supplied by the owning worker, not inferred from the wall clock.

`make check-ntp` passed on Linux/amd64 with the actual supervisor, control,
network and time workers, an independent chronyd local-reference peer, a LAN
chronyd `-Q` client and packet capture. It demonstrated upstream synchronization,
stratum/source reporting, finite observations and LAN NTP replies. All peers
ran in disposable namespaces, with host clock adjustment disabled.

Still required: DHCPv6/DHCPv4 learned time sources and expiration, source
priority/fallback, conditional source withdrawal, worker/reboot drift recovery,
access denial, IPv6 peers, real appliance clock step/slew and target hardware.
For current homert intent, `wan-info` is already conditional on the LAN VRRP
master role; its source selection must follow that existing ownership. Do not
create a second clock writer or copy learned addresses into operator intent.
