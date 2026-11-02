# Native ingress forwarding

The native ingress path combines Health observations, conditional DNAT,
hairpin source NAT, explicit rejection when all backends are unavailable, and
VRRP priority tracking. It selects the first healthy backend in declaration
order. One unhealthy backend leaves forwarding available through the next
healthy backend and lowers the tracked priority; full recovery restores it.

Run the real-process check with independent HTTPS backends and a Linux client:

```sh
make check-ingress
```

`TestDaemonIngressHTTPS` checks Host/SNI/path, backend status and transition
thresholds, ordered forwarding and recovery, scoped hairpin NAT, all-backend
rejection, and observed VRRP priorities. The test builds a native
`ResourceProgram` directly; it does not invoke a configuration converter.

The earlier acceptance log used an imported configuration and is discarded as
evidence for this native setup. The current worktree has not run this
integration check. It requires Linux network namespace and network
administration capabilities; those capabilities are absent in the current
host. This procedure does not qualify a physical router or the complete
homert configuration.
