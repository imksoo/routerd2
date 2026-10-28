# Appliance boot settings

The boot document is separate from transactional router configuration. Paths,
platform selection, kernel drivers, release selection, and SSH listeners take
effect at process startup.

This describes the current unreleased boot schema. The accepted next architecture separates
image extraction, selection of a versioned runtime directory, and loading its
startup configuration; see [ADR 0001](../../docs/adr/0001-boot-runtime-startup-config.md).
The current `config` field seeds an empty config store. An existing committed
record remains authoritative; the new explicit startup-revision selection is not implemented by this schema yet. The worker catalog is now explicit;
signed runtime selection replaces it with the verified manifest role bindings.
This prototype has no deployed users: its boot and serialized configuration
formats may be replaced directly, without a compatibility reader or migration.

## Persistent state volume

An appliance may mount one explicitly provisioned ext4 block device at its
`state_dir` before it opens configuration records, SSH host keys, or a signed
release store:

```json
{
  "platform": "appliance",
  "config": "/etc/routerd/config.json",
  "workers": {
    "logging": "/usr/libexec/routerd-logging",
    "network": "/usr/libexec/routerd-modules",
    "control": "/usr/libexec/routerd-modules",
    "management": "/usr/libexec/routerd-modules"
  },
  "runtime_dir": "/run/routerd",
  "state_dir": "/var/lib/routerd",
  "kernel_modules": ["virtio_pci", "virtio_blk", "ext4"],
  "state_volume": {"device": "/dev/vda", "wait_timeout_seconds": 10},
  "ssh": {"user": "admin"}
}
```

This example declares core workers only. Add protocol role bindings explicitly
for a factory profile that uses them. The decoder does not merge factory
worker defaults into a serialized catalog. `module_path` is no longer accepted.

This is a boot-settings example, not a disk provisioning command. The device
must already contain a supported ext4 filesystem. Include the actual machine's
storage drivers and the network drivers required by its router configuration.
Omitting `state_volume` preserves the existing directory-based behavior; in an
initramfs-only image that directory remains volatile unless another boot stage
has provided persistent storage.

Startup validates the document, mounts basic kernel interfaces, loads the
explicit kernel modules, mounts and verifies the state volume, and then opens
the release store or starts the supervisor. The same sequence runs after a
selected release preserves PID 1 through `exec`; existing matching mounts are
verified and reused. A volume error stops startup. It never switches silently
to a fresh directory on the initramfs.

`state_volume` accepts only `device` and `wait_timeout_seconds`. The wait is
zero for the 10-second default, or 1–300 seconds for an explicit bound on device
appearance. The device is a clean absolute path below `/dev`; symlink device
aliases are not followed. The target is always `state_dir`, which must be a
clean absolute non-root path outside `/dev`, `/proc`, and `/sys`. Neither
`runtime_dir` nor `state_dir` may contain the other. Host and container modes
reject this setting; the container runtime supplies their mounts.

The volume uses ext4 with an internal journal, `rw,nosuid,nodev`, executable
release files, ordered journaling, barriers, and `errors=remount-ro`. Mounting
requires the actual appliance PID 1 and root. There is no disk discovery,
formatting, repair, generic mount-option passthrough, or automatic unmount.
See [storage](../storage/README.md) for ownership checks and the boundary around
kernel I/O that requires a platform watchdog.

When `release` is enabled, its `root` and `trust_file` may refer to paths on this
volume. Provision the signed baseline, trust file, and any initial configuration
needed there before boot. Keep the initial boot document and storage driver
files accessible before the volume is mounted and after a selected userland
execs. This mounts persistent userland state; it does not implement a bootloader
or kernel/rootfs A/B selection.

The [two-boot integration fixture](../../images/storage-test/README.md) verifies
the real production mount sequence, persisted SSH edits and host keys, and
automatic fallback from an unconfirmed signed userland attempt.

## Explicit startup snapshot

A fixed-runtime boot may include a `startup` object with `generation`,
`expected_revision`, and `snapshot` (`store_id`, `revision`, `digest`).
The control worker adopts it once into the existing authoritative journal,
then reconciles that new revision. Replaying the consumed selection preserves
later commits and normal unconfirmed-restart rollback. Missing/corrupt stores
are not initialized from the seed when an explicit selection is present.

The selection is a bounded reference, not a ResourceProgram or executable path.
A `startup` object together with `release` is rejected until selection and
fallback for the runtime/configuration pair are implemented atomically. See
[the startup contract](../../docs/startup-config.md).

## Engine binding

A factory catalog declaring `pppoe` must also declare
`"engines":{"pppoe":"/usr/lib/routerd/pppoe-engine.tar"}`.
`make pppoe-engine` supplies that pinned archive and its corresponding source
archive. The bare host default catalog does not advertise PPPoE.
Signed release selection replaces both worker and engine bindings from the
verified manifest and passes their pinned descriptors to workers. The worker
ABI is 5: FD 3 carries lifecycle IPC, FD 4 the native image and FD 5 that
role's engine image. A restart keeps the same engine inode.

A VRRP service using `conntrack_sync` binds its own `vrrp` engine archive.
`make conntrack-engine` supplies the runtime and corresponding sources.
Plain VRRP does not start this engine. See the
[handoff contract](../protocol/conntracksync/README.md).

A `tailscale` role also requires its `tailscale` engine archive. The engine-free
host default omits this role. `make tailscale-engine` builds the fixed static
daemon, full notices and matching source artifact; signed catalogs bind both.
Its TUN/table52 ownership, explicit FW/NAT and private resolver setup are
specified in [the Tailscale contract](../protocol/tailscale/README.md).

The factory image also binds `dhcp_server` to its signed/bundled
`/usr/lib/routerd/dnsmasq-engine.tar` and corresponding sources. The generic
native host catalog omits this role; declare both worker and engine explicitly
when enabling it. `make dnsmasq-engine` builds its amd64/arm64 runtime and source
archives. DNS, DHCPv6/RA and VRRP election retain their Go implementations.
