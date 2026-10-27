# ADR 0002: PPPoE uses a pinned foreground pppd image

Date: 2026-10-27. Status: implemented; complete NET-04 qualification remains open.

routerd's `pkg/pppoeclient.LinuxPeer` supplies the operational basis for the
pppd options, authentication and session lifecycle. routerd2 does not implement
a second PPP protocol engine. Its worker owns one foreground pppd process;
pppd owns the PPP unit, IPCP/IPv6CP and negotiated addresses. The network writer
owns finite routes, forwarding and NAT. Route-only intent changes keep the
session. Observations use actual kernel addresses, not log strings.

The selected engine is Alpine 3.24.3 ppp-daemon/ppp-pppoe 2.5.3-r0. Its minimal
regular-file tar image contains pppd, pppoe.so, musl's interpreter/libc, libpcap,
OpenSSL libssl/libcrypto and its legacy provider. A real connection failed with
“Could not load legacy provider” when only DT_NEEDED libraries were included.
The builder and validator now include that actual dlopen dependency.
`make pppoe-engine` builds amd64 or arm64 inputs without executing foreign code.
The inventory names exact APK versions, aports commits and original notices.
The separate corresponding-source tar includes upstream sources, APKBUILDs and
patches checked against their SHA512 values. It is not mounted in the helper.

Manifest v4 binds the PPP role to both image and source artifacts. Publisher,
receiver and supervisor check the image's actual ELF architecture and dependency
closure. Missing legacy.so is rejected before publication or installation,
including a correctly signed bundle reproducing that omission. Native commands
remain static Go ELF. Boot ABI is 1, worker ABI 4 and compiler contract native-25.
Discarded manifest/worker formats have no compatibility readers.

The supervisor pins engine and executable descriptors for the cohort. FD 5
passes the engine to the PPP worker; its helper receives image/config descriptors,
reads the explicit private password file and enters an empty private chroot
before extraction. All loader/plugin/library lookup stays inside that image.
Private /etc/ppp, /run and device nodes supply only this process's state. There
is no shell, host pppd fallback, host hook or root .ppprc path. Passwords do not
enter argv or observations. The creating thread outlives pppd; PDEATHSIG and
bounded child cleanup prevent a second worker from inheriting a live old child.

The appliance uses linux-lts: Alpine linux-virt omits CONFIG_PPPOE. The builder
retains the configured module dependency closure rather than the full general
purpose module tree. Kernel source distribution and physical NIC qualification
remain separate unfinished release gates.

Independent peers are RP-PPPoE 3.15/pppd 2.4.9 and ISC DHCP 4.4.3-P1. On PVE
Linux 6.8, signed PID 1 selection, PAP/CHAP, DNS observation, MTU1454, LAN TCP/UDP,
IPv6CP, PD-to-LAN RA/SLAAC, Renew, worker/session recovery, original deadlines,
stale notification rejection and confirmed rollback are exercised through the
product. Normal DAD is enabled. The peer DHCP server must bind the recreated
PPP interface after session replacement and have an explicit server DUID.

Reproduce with `make check-pppoe`, `make check-pppoe-pd DHCP6_SERVER=/path/to/dhcpd`
and `make check-dual-wan`. The last uses independent PPP and Linux AFTR WANs,
separate application/client kernels, real Health/PBR, large TCP, fragmented UDP,
MTU changes, withdrawal, recovery and worker restarts. Relevant current results
and untested scope are in [the handoff](../pve-codex-handoff.md).

PPP learned DNS now reaches the resolver through typed input with original finite
deadlines. Independent dnsmasq peers verify LAN UDP/TCP queries, changed negotiated
servers, expiry while the peer remains reachable and recovery without DNS worker
replacement. The factory appliance exercises this path with PD and PPP recovery.

Complete resource fields, all path-MTU and
VRF cases, real ISP PPP, container PPP permissions, physical hardware and complete
engine/kernel update/fallback qualification remain unfinished. Building static
Go commands or an OCI image does not close those gates.

Comparison source: [routerd LinuxPeer](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/pppoeclient/client.go).
