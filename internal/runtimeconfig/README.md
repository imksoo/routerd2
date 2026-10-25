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
    "logging": "/usr/libexec/routerd-modules",
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
