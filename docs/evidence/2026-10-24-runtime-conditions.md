# Runtime conditions checkpoint — 2026-10-24

Base: e4be8804fe70a3769523d188a8af1e86b26b5e77. Go 1.27.3, Linux amd64.

Implemented: source when/ready_when; immutable whole-projection fact snapshots;
dependency/owner closure; finite supervisor and accepted protocol observations;
CLOCK_BOOTTIME deadlines, generation/revision fences; serialized stop/network/start;
confirmation readiness; successful network projection generation in status/health;
prefix-scoped CLI completion to avoid recursive conditions exhausting its budget.

Local checks: resourceprogram/runtimeplan/config/control/cli/cmd-routerd tests,
race detector on these packages, go vet ./..., CGO_ENABLED=0 command builds.
The test fixtures exercise the real repository types and coordinator with in-memory
network/supervisor boundaries. They do not prove kernel packet forwarding.

Full go test ./... was attempted. Unix socket creation is denied by this execution
environment (operation not permitted), preventing localapi, logging, supervisor,
legacy runtime boundaries, BGP and DNS daemon tests; DHCP server runtime readiness
also timed out. The full suite is NOT PASS. A CLI completion regression discovered
by that run was fixed and the CLI suite rerun successfully.

Not complete: CORE-03/04 overall, hold/maxAge, durable events, standalone Health
probes, PBR acceptance, real daemon/packet/kernel integration, M2–M7. No parity
classification is promoted by this checkpoint. GitHub CI is not a blocking gate.
