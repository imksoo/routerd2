# BGP community interoperability

The native `service.bgp` declaration supports sending the standard community
and configured communities to a peer. The integration check reads the route at
an independent FRR peer and exercises forwarding through the learned route.

Run from the repository root with FRR installed and permission to create
isolated Linux network namespaces:

```sh
make check-bgp-frr
```

This runs `TestDaemonBGPWithFRR` with the native resource program and actual
routerd2 processes. Earlier results that depended on the removed configuration
converter are discarded; this native path has not been run in the current
worktree. Production devices are not changed by this check.
