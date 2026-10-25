# Native DHCPv4 client

`Validate(Config)` is side-effect free. `Run(ctx, config, effects, ready)` opens
an interface-bound Linux packet socket and owns a locked persistent lease file.
`ready` means initialization and the first exchange succeeded, not that a lease
has been acquired. `Effects.Update` is synchronous and must honor its context;
an error stops the worker. The worker itself never applies an address or route.

The protocol imports the wire/lease types from the pinned original routerd
module. The Linux receive transport is a selected source adaptation, with its
commit and complete BSD notice in `NOTICE`. Initial discovery/request broadcasts
are sent as Ethernet IPv4 packets sourced from 0.0.0.0, so no IPv4 route/address
is required. Renewals use the lease source address with IP_PKTINFO. Received raw
packets are checked for fragmentation, length, checksums, BOOTP identity,
transaction, selected server and option bounds.
Kernel `PACKET_AUXDATA` identifies deferred UDP checksums on veth/virtual
interfaces; all other packets have their supplied checksum verified. RFC 3396
option fragments and the DHCP option-overload fields are decoded in wire order.

The implementation handles discovery/selecting, INIT-REBOOT on a matching
persisted MAC/client identity, ACK/NAK, T1 unicast renewal, T2 broadcast rebind,
retransmission and expiry. Gateway, DNS, NTP, domain, MTU and lease timestamps are
published in `Snapshot`. `Active` is the producer's observed intent; consumers
also require its original current-boot lease deadline. A last raw `Active`
can remain visible after ownership expires. Inactive snapshots must not leave
an old address/default route active. Restarted
leases require an ACK before becoming active. Normal stop withdraws effects but
retains the server lease record for the next INIT-REBOOT exchange.

Within a running client, a validated server ACK establishes T1, T2 and expiry
once from `CLOCK_BOOTTIME`. UTC corrections cannot change these deadlines.
Retransmission, transaction timeout and the DHCP `secs` field also use boot
time. An expired lease is withdrawn before processing a queued reply; the old
transaction cannot renew it after a process stall. A clock read failure,
boot identity change, rewind or deadline overflow stops the worker. UTC lease
timestamps remain display and saved INIT-REBOOT hint metadata. A saved hint is
never active until the server supplies a fresh ACK.

Snapshots carry the original boot observation, logical epoch/sequence and
ACK-scoped lease clock. The controller authenticates even the first publication
against the current supervisor generation. Transport retries cannot change the
source observation, lease payload or original deadline. Active facts retain
the ACK age through renewal/rebind phase publications. A final stopped snapshot
cannot become active again in the same worker incarnation.

The network writer persists boot-scoped address lifetimes and computes kernel
relative lifetimes from BOOTTIME, including after journal reopen. UTC
`expiresAt` is display metadata. Control recovery deliberately quiesces previous
dynamic workers and requires a fresh server ACK; saved metadata is only an
inactive INIT-REBOOT hint. Physical suspend/NTP faults, other protocol clock
boundaries and autonomous IPv4 default-route expiry with every controller
stopped remain separate acceptance work.

The control-plane compiler owns `UseRoutes`, `UseDNS`, metric, dependency/when
policy and merging this dynamic state with static declarations. Do not start
multiple DHCP clients on the same interface. The state file is per resource and
must survive worker/image restarts. No DHCPv4 server, relay, classless-static-route
decoder, ARP duplicate-address detection or lease release control API is supplied
by this package yet; those capabilities must not be inferred from a client.

Focused unit tests exercise wire/state behavior with fake packets and time.
`go test -tags integration ./internal/protocol/dhcp4` additionally re-executes
into fresh user/network namespaces, creates private veth peers, and verifies
DHCPv4 acquisition/renewal/expiry with a kernel UDP server, DHCPv6 PD/information/
renewal, and RA solicitation/shutdown withdrawal. It never mutates the host
network. Set `ROUTERD2_INTEGRATION_REQUIRED=1` to fail if isolation is unavailable.
Interoperability with independent vendor DHCP servers remains a separate gate.

`make qemu-dhcp4-clock` requires an existing appliance kernel image in
`IMAGE_DIR` and boots a separate no-NIC guest. Its test-only PID 1 changes actual
`CLOCK_REALTIME` by fixed +24h/-24h offsets and restores the original boot-based
baseline on exit. A per-boot kernel marker and inherited socketpair fence the
clock broker; it is absent from both production binaries. The real supervisor,
control, network and DHCP workers must retain the original lease across UTC
correction and configuration reapply, withdraw on original BOOT expiry, reject
old generations and recover only after fresh ACKs, with TCP/UDP and capture.
Running the positive fault directly on a host is refused. This verifies clock
steps in QEMU, not NTP discipline/slew, physical suspend, or hardware readiness.
