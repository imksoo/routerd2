# Native DHCPv6 PD/information worker

`Validate(Config)` is side-effect free. `Run(ctx, config, effects, ready)` uses the
pinned original `pkg/pdclient` state machine, an interface-bound UDP/546 socket,
and a locked/synchronized snapshot journal. The link must already be up with a
usable link-local IPv6 address. Socket initialization reports worker readiness;
it does not assert a delegated prefix exists.

An explicit DUID is used as configured. Otherwise the persistent DUID is reused,
or a DUID-LL is generated once from the Ethernet address and persisted before
transmission. The compiler supplies an IAID. Default prefix request is /60.
The adapter validates DUID, IAID, selected server, prefix and lifetimes, rejects
failed DHCP status options, and adds Request retries and elapsed-time handling
around the existing Solicit/Renew/Rebind engine. PD and information transactions
are tracked separately. Prefix expiry/valid-lifetime zero emits withdrawal;
unexpired prior prefixes remain individually timestamped for renumbering.

`Snapshot` embeds the original typed snapshot and adds `Active` and
`InformationOnly`. The compiler owns LAN subnet derivation, address lifetimes,
RA changes and all kernel mutations. It must use expiry timestamps, including
`PreviousPrefixes`, rather than refresh lifetimes from the original duration on
every reconcile. On worker stop, the callback clears current and previous prefix
effects while the disk journal retains recoverable lease identity. Callback
errors fail the worker.

`DHCPv6Information` on an interface that also uses PD should compile into that
same worker. Do not run competing UDP/546 clients. AFTR, DNS, SNTP and search
domains are requested. Information-request asks for option 32 and honors the
negotiated Information Refresh Time (24-hour default, 600-second floor, explicit
infinity). `InformationRefreshAt` and `InformationStale` expose refresh state;
refresh is not a prefix lease expiry. Previously learned information remains
available while refresh retries are pending, and the compiler decides its stale
data policy. IA_NA address acquisition, DHCPv6 server/relay, multiple IA_PD instances,
multiple prefixes in one IA, rapid commit and full server-selection policy are
not implemented by this adapter. The old engine is an IA_PD subset; importing it
does not establish full DHCPv6 feature parity.

Tests cover packet retransmission bookkeeping, identity/lifetime validation,
failure-status handling, persistence and expired restore without live sockets.
The isolated native test in `../dhcp4/kernel_integration_test.go` exercises a
real UDP/546 client against a fixture UDP/547 server, including information
refresh and renewal. Independent server interoperability and delegated-prefix
propagation through the complete control-plane compiler remain separate gates.
