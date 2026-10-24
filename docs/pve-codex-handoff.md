# routerd2: PVE VM Codex handoff — 2026-10-24

## Objective and authority

Continue the entire implementation plan in `docs/implementation-plan.md` and
`docs/implementation-plan.json`. The user authorized development and GitHub
operations, including main publication. Work in the local edit/test loop. Do not
wait for GitHub Actions between implementation changes. Do not mark work packages
complete merely because a library, mock test or compiled binary exists.

Use a disposable Linux VM on PVE, not the PVE host or a production router. Keep
management access independent of the test namespaces. Do not apply the example
configuration to production WAN interfaces. Production cutover, cloud costs and
disk initialization remain outside this isolated development task.

## Current checkpoint

Base before this work: `e4be8804fe70a3769523d188a8af1e86b26b5e77`.
First local commit: `80d641a` (resource conditions/runtime reconciliation).
The follow-up commit containing this handoff adds:

- Source `when` / `ready_when`, immutable whole-projection fact snapshots,
  dependency/owner subtree gating, finite trusted protocol observations.
- Generation/revision/boot/epoch fencing, CLOCK_BOOTTIME expiry, confirmation
  readiness and a separate successful network projection generation.
- Stop changed workers before changing their network, start only after ACK;
  retry if facts change during stop, and restart quiesced matching bindings.
- Native Health worker: scoped ICMP/TCP/DNS/HTTP, threshold/history, finite
  observations, shutdown withdrawal, supervisor/worker registration.
- Read-only scoped Netlink lookup through the network worker for best-effort
  probe evidence. No shell command dependency.
- `examples/health-pbr.yaml`: independent probe path, guarded primary route,
  persistent backup route and source PBR rule.
- CLI recursive-condition completion fix; timezone-independent route/address
  deadline validation (negative Unix timestamps previously passed in JST).

Health runtime fixtures: `internal/runtimeplan/health_pbr_test.go` and
`resource_conditions_test.go`. Protocol tests: `internal/protocol/health`.

## Evidence and remaining environment limits

The cloud workspace used real repository Go 1.27.3. Relevant package tests and
race detector, whole-repository vet and static command builds passed. Scoped
TCP, HTTP and DNS probes passed against real loopback servers. Network writer
unit tests passed after fixing the timezone-dependent deadline bug.

A full `go test ./...` was attempted. The environment rejects Unix sockets with
`operation not permitted`: localapi, logging, supervisor, legacy runtime boundary,
BGP and DNS daemon tests cannot complete. DHCP server readiness also timed out.
That full run is NOT PASS. The kernel, netns, real supervisor and two-WAN packet
acceptance are not completed here. Raw ICMP integration was also attempted and failed at socket creation with
`operation not permitted`; rerun it on the VM. Do not relabel these limits as successful skips.

## First VM actions

1. Update a clean checkout, inspect local changes before pull, and read AGENTS.md
   if one exists. Pin and record the exact tested commit (`git rev-parse HEAD`).
2. Use Linux amd64 and Go 1.27.3 as in go.mod/CI. Install compiler and normal build
   dependencies. Verify Unix sockets, network namespaces, CAP_NET_ADMIN and
   CAP_NET_RAW work inside this disposable VM.
3. Run local checks and preserve logs:

```sh
go version
make check
go test -race -tags integration ./internal/protocol/health -run '^TestScoped' -count=1 -v
make build-linux
```

`make check` includes format, full tests, dependency tests, Python tool tests,
race and vet. Diagnose failures; do not weaken tests to get a pass. On sockets
or privilege failure, fix the VM execution profile instead of adding skips.

4. Run the existing required QEMU/kernel gates locally as appropriate:

```sh
make image IMAGE_CONFIG=qemu
make qemu-network
make qemu-protocols
```

These require the repository's image toolchain/Docker/QEMU as documented in
`images/README.md`. They are local tests, not a reason to wait for GitHub Actions.
They do NOT yet contain a new end-to-end two-WAN Health/PBR acceptance gate.

## First missing acceptance gate to implement

Add a required isolated Linux/netns or QEMU integration test with independent
WAN-A/WAN-B peers and a LAN client. Run actual routerd supervisor, control,
network and health workers, not only memoryNetwork/memoryManager.

- Adapt `examples/health-pbr.yaml` to the lab's addresses and probe servers.
- Capture probe packets: interface/source/mark must match WAN-A; cutting WAN-A
  must not let probes succeed through WAN-B.
- Confirm LAN TCP/UDP uses the primary while eligible, falls back after the
  configured unhealthy threshold, and returns after the healthy threshold.
- Verify probe host route and policy rule survive application-route changes;
  unrelated/foreign objects remain untouched.
- Kill/restart health/control workers, delay old-generation reports, expire
  facts, and exercise confirmed rollback. Check fail-closed behavior and no
  stale Up resurrection. Track operator revision separately from projection.
- Record explicit pass markers, commit and artifacts; failed/omitted cases are
  not acceptance. Add this suite to local test targets and nonblocking CI.

## Continue remaining plan

CORE-03: hold/maxAge, general status references, complete readiness/lifecycle
contracts and resource status views. CORE-04: durable journal/event/cursor/outbox
contracts (separate from transient facts). NET-01: dynamic targetSource and
legacy import, required packet evidence. NET-02: full EgressRoutePolicy
priority/mark/hash/hysteresis and ownership contracts. Then continue M2–M7 from
the plan; HA/SAM/cloud/distribution acceptance is not completed by this change.
Do not promote parity.json without corresponding contract and execution evidence.
