# Conntrack state during VRRP handoff

One IPv4 VRRP worker can own a bundled conntrackd FTFW peer for its network
namespace. `conntrack_sync` is an explicit VRRP service option:

```json
{
  "interface": "lan0",
  "local_address": "192.0.2.2",
  "peer_address": "192.0.2.3",
  "port": 3780,
  "ignore_ipv4": ["198.51.100.2"]
}
```

Declare `forwarding.conntrack_tcp_liberal: true` in the same program. The
network writer applies and journals that setting, and restores its original
value when the declaration is removed. VRRP depends on the setting, sync
interface and declared local address. There is one replication owner per
namespace. Source configuration, routes, addresses and firewall rules remain
with their existing owners; conntrackd is delegated the replicated conntrack
and expectation entries. Local/peer control addresses and explicit management
addresses are excluded from replication. The FTFW transport itself has no
cryptographic authentication; use a trusted link or an authenticated tunnel.
The sync interface can use IPv4 link-local addresses. There is no global-unicast
requirement; local and peer must be distinct IPv4 unicast endpoints.

The VRRP worker holds the engine through its final ownership withdrawal:

1. On election, commit the peer's external cache, then request a local rescan and
   broadcast (`-c`, `-R`, `-B`). Only then publish the elected
   state that can raise WAN identities and admit the LAN VIP.
2. On demotion, withdraw those resources through the existing control/network
   application acknowledgment, then schedule the conntrackd purge and request
   resynchronization (`-t`, `-n`).
3. Engine exit cancels the VRRP operation, withdraws ownership, and causes the
   supervisor to replace the failed worker. Normal shutdown keeps the engine
   alive until withdrawal completes.

This order adapts routerd's `cmd/routerd-vrrp-vmac` implementation. No second
coordinator, peer registry, shell hook or alternate host executable is used.

## Build and runtime binding

`make conntrack-engine DOCKER='sudo -n docker'` produces
`dist/conntrack-engine.tar` and `dist/conntrack-sources.tar`. The pinned Alpine
3.24.3 package inputs supply exact versions and source checksums. conntrackd
1.4.9 is rebuilt with the small GPL-2.0-or-later completion-result patch. The
libnetfilter_conntrack 1.1.1 copy table is also patched to retain
`ATTR_TIMESTAMP_EVENT`. Its parser accepts that kernel attribute but the
upstream copy table omits it; updating a cached connection would otherwise
abort conntrackd in `nfct_copy(NFCT_CP_META)`. Both patches and the exact
upstream sources accompany the runtime library.
The archive includes the loader/library closure, protocol database and licenses;
the corresponding-source artifact includes upstream sources, APKBUILDs,
patches and build scripts. `CONNTRACK_ARCH` selects the target; a foreign C
build requires a native builder or configured instruction emulation.

For an unsigned development boot bind
`"engines":{"vrrp":"/absolute/path/conntrack-engine.tar"}`. A signed manifest
binds the `vrrp` engine and its corresponding source artifact together. The
supervisor pins it on FD 5. The worker extracts it into an empty, private mount
namespace/chroot. Engine control commands enter that same pinned root; there
is no lookup of host binaries, configuration, hooks or libraries.

Upstream CLI exit zero does not establish a complete kernel commit. The patch
returns conntrack and expectation failure counts only after the actual commits
finish. A missing or failed completion prevents ownership publication. Other
commands request rescan/broadcast/resync; their acknowledgment does not claim
that the peer has completed receiving a snapshot.

## Executed communication and limits

`make check-ha-state DOCKER='sudo -n docker'` runs two signed actual PID 1
process trees on the PVE Linux kernel, with separate Linux TCP/UDP client and
server namespaces. It checks continuous traffic over one TCP socket and one
UDP socket through NAT, both takeover directions, worker/engine replacement,
an actual engine SIGKILL, and configuration removal restoring the original
sysctl. `tcp_loose=0` prevents reconstruction of an unreplicated midstream TCP
flow. A dedicated IPv4 link-local sync link exercises both the configuration
path and actual engine replication. Packet capture checks the single client
SYN and link-local FTFW traffic.
UDP loss is reported separately from mapping preservation and recovery.
Diagnostics, captures, catalogs and private namespaces are deleted afterward.
The same fixture now includes independent BusyBox DHCP clients and the native
[DHCPv4 checkpoint replication](../dhcpserver/README.md#two-node-replication),
including sync-link loss, worker replacement and lease expiry. This does not
authenticate the conntrackd FTFW transport itself.

This is not homert replacement acceptance. Complete DHCP/PD synchronization, authenticated
peer admission, complete partition/gap/old-peer handling, large-state commits,
VRF/zone profiles, ARM64 engine build/execution, factory image integration and hardware/load
qualification remain open. The partial-commit result decoder has a focused
local check; actual kernel-table exhaustion has not been exercised.
