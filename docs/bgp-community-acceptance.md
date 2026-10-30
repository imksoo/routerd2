# Imported BGP identity community

The captured pve-rt-06 configuration uses `BGPRouter/lan` with
`communities.send: standard` and `communities.set.out: ["64512:46248"]`.
The importer previously rejected `standard`, although both implementations
already advertise the configured standard community.

The old effective behavior is retained: routerd's
`pkg/controller/bgp/controller.go` uses `SendCommunity: 3`, including when the
source says `standard`. The existing routerd2 worker does the same. Import
acceptance does not introduce a new attribute filter or discard extended
communities. The migration report discloses this behavior. Community import
transformations and unsupported send modes remain rejected.

## Acceptance

From a dedicated checkout, with independent FRR and disposable network
namespaces:

```sh
umask 022
make build RELEASE_ID=pve-bgp-community-6836afb
python3 scripts/daemon-tests.py bgp-frr --test TestDaemonBGPWithFRR
go test ./internal/legacyconfig
```

Recorded on 2026-10-30 at 15:30 JST, based on `6836afb` plus this change:

- The product migrates the legacy declaration and starts its normal BGP worker.
- FRR's received route contains `64512:46248` before and after a complete
  product daemon restart. The assertion reads FRR's route view, rather than
  the product's configured intent or local routing information base.
- A LAN client exchanges TCP and UDP with the independent peer through the
  route learned from FRR. Packet captures cover the LAN and BGP interfaces.
- `TestDaemonBGPWithFRR` passed in 6.92 seconds; the existing legacy importer
  package checks also passed.

Product binary SHA-256:

- `routerd`: `bd00278bd39a093c66b697915bdd52619e2e51747d3ba9b10f30d42e9cb85f20`
- `routerd-modules`: `e07779675879c56e048653a126a9297741a16cf4f5aac3a6103f8a5fc89bbf04`
- `routerd-logging`: `be91c9f54659737803c666c188dcc1d13b524d8b943a8b182e5fba2d7bba2bfc`

This acceptance covers the configured BGP identity attribute and existing
forwarding/restart path. It does not establish complete pve-rt migration,
SAM enrollment, or k8s IngressService support. Production devices were not
changed. Detailed transient logs and captures are not repository artifacts.
