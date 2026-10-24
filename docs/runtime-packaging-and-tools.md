# Runtime packaging and tool policy (implementation checkpoint)

routerd2 is unreleased. No compatibility with the prototype two-ELF bundle or its configuration formats is required.

## Separation of responsibilities

- **Firmware image**: one signed transport artifact, containing a manifest and an arbitrary number of declared regular files.
- **Installed runtime**: an immutable, digest-addressed directory with `bin/`, `modules/`, `tools/`, `lib/`, and `share/licenses/` as needed. A module, process and ELF need not map one-to-one.
- **Boot selector**: reads boot configuration, validates the chosen installed runtime, then hands off to its entrypoint. It does not parse the network configuration.
- **Runtime**: loads startup-config, supervises declared workers, owns the networking resource model and its state.
- **Tools**: diagnostic and editor executables are not daemon dependencies and are launched on demand with constrained privileges.

## Implementation decisions

- Keep systemd out of the appliance PID 1 path; the routerd supervisor owns lifecycle and reaping.
- Keep journald optional, not an implicit dependency. The logging module must own bounded storage, structured query, loss accounting and forwarding.
- Implement the router-specific ping/traceroute interface in a diagnostics worker using Linux socket APIs; separate interactive probes from health probes.
- Implement capture through a dedicated, privilege-constrained worker. Prefer maintained packet capture/filter libraries rather than rewriting tcpdump and libpcap. Export interoperable capture files. Allow a packaged tcpdump in `tools/` for advanced troubleshooting.
- Use Linux APIs directly for route, nftables, WireGuard and NIC state; do not shell out to `ip`, `nft`, `wg` or `ethtool` in the normal control path.
- Permit a small packaged editor instead of writing a complete Vim clone. SSH config editing remains candidate -> validate -> diff -> commit confirmed.
- Resolve Alpine APK dependencies at image-build time, not by upgrading packages inside an active runtime. External engines and their licenses are pinned by the signed manifest.

## Required acceptance tests

1. Install an image; repeat the *identical* image installation. Assert idempotence, unchanged active runtime bytes, no new boot attempt, and stable startup-config.
2. Install an image with the same payload but a different signed version identity. Assert distinct immutable runtime roots, boot selection and rollback.
3. Reject a different payload attempting to reuse an installed digest or release identity contrary to the manifest policy.
4. Interrupt extraction at every publication boundary. Verify no incomplete runtime becomes selectable.
5. Tamper with a declared file, inject a symlink or omit a tool/library. Verify the selected runtime fails closed.
6. Confirm that tools are invoked only on demand and cannot change network ownership without the typed management API.
7. Verify startup-config and SSH host identity survive image replacement and rollback.
8. Boot the generated Alpine appliance under QEMU, and separately verify the container profile against the host kernel.

The existing `internal/release` pair-only archive and selector are **not** evidence that these multi-file tests pass. Replace that prototype format directly, without migration shims.
