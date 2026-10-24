# Daemon dependency inventory — pre-implementation architecture gate

Scope: all functional daemon families and relevant utility dependencies identified in the routerd source tree and routerd2 implementation plan, not just NTP/DHCP. This is a **source-level first pass**; rows marked unverified require upstream package and actual Alpine foreground/kill/restart tests before engine selection is frozen. routerd and routerd2 are different products; no prototype compatibility is required.

Evidence: [routerd commands](https://github.com/imksoo/routerd/tree/main/cmd), [controller packages](https://github.com/imksoo/routerd/tree/main/pkg/controller), [implementation plan](implementation-plan.md), [current parity inventory](parity.md). Source tree enumerated on 2026-10-24. Inventory covers runtime engines, supervisors, service dependencies, and ancillary diagnostics; build-only commands are not runtime requirements.

| Family / representative engine | Integration choice | systemd/journald risk | Other dependencies / ownership conflict | Required gate |
|---|---|---|---|---|
| NTP: chronyd, ntpd/ntpsec | External foreground chronyd candidate | Low, upstream verification pending | clock stepping/slewing, CAP_SYS_TIME, source state, DNS | Ready, log, reload, time correction, no systemd |
| DHCPv4 server: dnsmasq, Kea, ISC dhcpd | Native worker currently; Kea optional | Low to medium, Kea hooks/logging unverified | lease DB, hooks, DNS, sockets, interface ownership | lease recovery, failover, DNS, no shell |
| DHCPv6 server / relay | Native or maintained engine | Unverified | PD, IA_NA, relay sockets, persistent leases | protocol and restart tests |
| DHCPv4/v6 client, PD, RA | Native workers | None for native | address, route, resolver ownership | lease expiry and worker death |
| DNS: dnsmasq, Unbound, CoreDNS, current Go resolver | Native Go worker; external optional | Low to medium | dynamic lease records, DNSSEC, TLS/QUIC, cache | bind, reload, log, crash tests |
| PPPoE: pppd / rp-pppoe | Native PPP controller or supervised pppd | Low | plugins, privileged PPP device, secrets, ip-up/down scripts, route/DNS ownership | foreground, no scripts, reconnection |
| BGP: GoBGP, FRR/bgpd/zebra | Embedded GoBGP preferred | Low for GoBGP; FRR multi-daemon risk | RIB/FIB writer conflict, sockets, watchfrr, BFD | peer/FIB ownership and restart |
| BFD | Native worker | None | timer scheduling, BGP gating | latency, restart, stale fact |
| VRRP: keepalived | Native worker | Low for native | VIP, GARP/NA, track scripts, fencing | crash and split-brain safety |
| IPsec/IKE: strongSwan charon, charon-systemd | Plain charon + VICI candidate | High for charon-systemd, avoid | plugins, XFRM, route/VIP ownership, secrets | foreground, private VICI, kernel conflict |
| WireGuard | Native kernel adapter | None | key storage, netlink, routes, marks | restart/rollback and traffic |
| Tailscale: tailscaled/tsnet | Dedicated adapter, choice pending | Medium | state, control plane, TUN, route/DNS/SSH ownership | subnet/exit/identity, no init dependency |
| DPI: nDPI / classifier | Isolated native/CGo worker | Low for engine; packaging high | libndpi ABI, protocol datasets, licenses, flow input | Alpine musl, capture throughput |
| Packet capture: tcpdump/libpcap | On-demand tool plus bounded capture worker | None | CAP_NET_RAW, BPF, disk quotas, pcapng | filters/export, stop, privilege |
| Firewall log: NFLOG/pflog | Native logging worker | None for Linux native | kernel log stream, queue loss | overload and kernel log restart |
| Health checks: ICMP/TCP/DNS/HTTP | Native worker | None | source route/VRF, failure isolation | WAN-specific probe and timeout |
| conntrack observer/sync: conntrack-tools/conntrackd | Native adapter or managed engine | Low to medium | netfilter netlink, replication transport, session ownership | existing TCP/UDP flow continuity |
| Event daemon, event relay, federation | Native workers | None | durable outbox/cursor, replay, retry | restart and idempotence |
| DHCP fingerprint/ARP/RA observers | Native workers | None | raw sockets, AF_PACKET, event loss | packet and restart tests |
| SAM/mobility, RR/leaf, FOU, cloud provider actions | Native workers plus provider SDK | Medium, cloud guest agents variable | metadata, NIC/route APIs, credentials, cloud-init | no guest-agent assumptions |
| SSH, CLI, Web/API | Native management worker | None | host keys, PAM/NSS/OS Login optional | identity, privilege, reconnect |
| logging: rsyslog/syslog-ng/journald | Native log worker; optional bounded compatibility socket | Medium/high if third-party native journal API | /dev/log, journald socket, sd_notify, rotation, audit | prove API-specific need before bundling |
| process supervision: systemd/OpenRC | routerd PID 1 | High if imported engine assumes D-Bus/systemd units | socket activation, cgroups, watchdog, notify | dependency contract per engine |
| diagnostics: ping/traceroute/ss/ethtool | Native diagnostic API; optional tools | None | raw sockets, namespace/VRF, netlink | source/interface selection |
| editor: vi/vim/busybox vi | Optional packaged tool | None | terminal/PTY, candidate config security | SSH editing without shell escape |
| update/install: apk, tar, decompressor | Build-time apk; signed runtime stager | None | atomic extraction, fsync, trust store | identical-image and version-only swaps |
| cloud boot: cloud-init, waagent, GCP guest agent, OCI/AWS agents | Provider-specific native bootstrap where feasible | Medium/high and provider-specific | metadata, identity, ready signals, route/IP API | boot and recovery on each cloud |
| watchdog, time, sysctl, device/module init | Small boot/runtime workers | Low | kernel devices, permissions, bootloader | reboot and fallback |
| metrics/export: Prometheus/OpenTelemetry collectors | Native exporter or managed collector | Low to medium | backpressure, disk, TLS, credentials | bounded queues and crash |
| IP address discovery / NAT traversal | Tailscale or VPN-specific engine | Unverified | STUN/DERP, firewall marks, identity | multi-NAT and reconnection |

## Mandatory engine evaluation

For each candidate, record exact upstream version, source license (including plugins), Alpine package availability, dynamic library closure, musl compatibility, foreground command, readiness contract, logging API, state files, required privileges/capabilities, kernel mutations, helper processes/scripts, runtime network access, restart and config reload behavior, and real executable test evidence. Mark each as **verified**, **failed**, or **unverified**; absence of a systemd unit requirement is not proof of systemd-free execution.

Decision rules: prefer native integration for state ownership; prefer a supervised external foreground daemon when a mature protocol implementation is costly to reproduce; only implement a limited syslog/notify shim after documenting exact consumers. Never implement a generic systemd/journald clone as an accidental dependency. External binaries and their plugins/libraries are version-pinned inside the single signed firmware image. A chosen engine must not independently mutate routerd-owned routes, addresses, firewall or VIP without an explicit delegation.

## Immediate blocking investigations

1. Run Alpine-based foreground/restart tests for chronyd, pppd, plain charon, tailscaled and Kea (including plugins, hooks and logs).
2. Validate route/address/XFRM ownership under strongSwan, PPPoE and Tailscale.
3. Inspect cloud guest-agent requirements independently for OCI, AWS, Azure and GCP.
4. Freeze manifest role bindings and runtime dependency closure only after these gates; the executable count remains unconstrained.

This document is an **inventory and engineering gate**, not a claim that every upstream package was executed or all transitive dependencies were exhaustively resolved.
