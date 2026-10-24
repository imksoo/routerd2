# Native appliance state volume

`storage.Mount` mounts one explicitly configured, already formatted ext4 block
device as the appliance's state directory. It is part of the PID 1 boot path;
there is no mount service, shell command, systemd unit, fstab parser, device
discovery, filesystem formatter, or filesystem repair utility in this package.

```go
volume, err := storage.Mount(ctx, storage.Options{
    Platform:    "appliance",
    Device:      "/dev/vda",
    Target:      "/var/lib/routerd",
    WaitTimeout: 10 * time.Second,
})
if err != nil {
    // Stop startup even when volume.Mounted is true.
    return err
}
```

The caller first mounts the kernel interfaces, including procfs and devtmpfs,
and loads the required block-device and ext4 drivers. It then calls `Mount`
**before** opening the release store, committed configuration, DHCP lease
journal, SSH host key, or persistent log files. Public boot configuration fixes
`Target` to `StateDir` and rejects overlap with the runtime directory. A container
or host installation uses storage provided by its surrounding environment.

## Device and filesystem contract

Only explicit appliance mode, PID 1, and effective UID 0 are accepted. Calls are
serialized with a nonblocking process-wide guard; a concurrent call returns
`ErrBusy`. This checks the selected execution mode, not whether the process is
in the initial PID namespace.

`Device` must be a clean absolute path below `/dev`. Every parent directory is
opened relative to a pinned parent descriptor with `O_NOFOLLOW`; the final node
must be a root-owned block device without other-write permission. The package
does not follow `/dev/disk/by-uuid` symlinks or select among devices with matching
UUIDs. `Status.UUID` only reports the UUID read from the selected device.

The selected descriptor is opened read-only and the ext4 superblock is read
without modifying it. The supported initial profile requires:

- The ext4 magic, extents, a nonzero UUID, and an internal journal inode.
- No external journal device or journal UUID.
- No custom on-disk mount-option string.
- No on-disk defaults that disable barriers or select unordered data writeback.
- A recognized on-disk error policy, used to interpret omitted mountinfo options.

These restrictions prevent one configured filesystem from implicitly opening an
external journal device. They intentionally exclude ext2/ext3 compatibility
mounts and ext4 filesystems with custom stored mount options. This is not an
integrity check: the kernel validates the filesystem, features, and checksums
when mounting it. Provisioning and offline repair remain image/operator tasks.

The new mount uses ext4 with `rw,nosuid,nodev,relatime` and the explicit filesystem
options `errors=remount-ro,data=ordered,barrier=1`. It remains executable because
verified `routerd` and `routerd-modules` release images can live on this volume.
The block descriptor's procfs path is the mount source; a pinned directory
descriptor's procfs path is the target. No user-supplied flags or filesystem
options are passed to the kernel.

An unavailable device is retried only for missing/unavailable-device errors.
The wait defaults to 10 seconds and is configurable up to 5 minutes; cancellation
ends the wait. Permission errors, symlinks, wrong node types, read-only devices,
and unsupported filesystems fail immediately. The wait limit covers device
appearance. It cannot interrupt a hung device read, mount syscall, or fsync; a
platform watchdog is required to recover from blocked kernel I/O.

## Mount ownership and repeat calls

An unmounted target must be empty. Missing components are created with mode 0700
and parent directories are fsynced. Every component must be a real root-owned
directory without group or other write access. `/`, `/proc`, `/sys`, `/dev`, and
the latter three directories' descendants are prohibited. Sticky temporary
directories are not accepted as production ancestors.

The bounded mountinfo parser limits the table to 2 MiB, 4096 mounts, and 32 KiB
per line. It decodes the kernel's pathname escapes and permits future optional
fields. It rejects ambiguous stacked mounts, mounts below the state target,
another mount of the selected device elsewhere in this mount namespace, and
shared/slave propagation on the target's covering mount.

An existing mount is reused only if it is the selected device's whole ext4
filesystem, with root `/`, writable/executable state, and `nosuid,nodev`.
Read-only, no-exec, disabled barriers, disabled journal replay, unordered data
writeback, and incompatible error policies are rejected. ext4 omits some
options that match its stored defaults, so the raw superblock default is also
checked: absence of `errors=` is accepted only when the stored behavior is
remount-read-only. `data=journal` is also accepted on an existing mount because
it provides data journaling. Existing mounts are never silently remounted to
change their policy.

Printed source paths are not used as identity. The block node's `st_rdev`, the
mountinfo device number, the target descriptor's `st_dev` and `fstatfs` result,
and its `/proc/self/fdinfo` mount ID must agree. After a new mount, the package
reopens the target through a fresh no-follow traversal: the old descriptor
still refers to the covered directory. It repeats topology and descriptor
checks before reporting success.

The mounted root must have UID/GID 0 and must not grant group or other write
access. Its mode is changed to 0700, clearing special permission bits, and the
directory is fsynced. Contents are not recursively changed or trusted by this
operation; each state owner retains its own validation and durable-write rules.

## Failure and durability boundaries

`Status.Mounted` becomes true immediately after a successful mount syscall. If
a subsequent ownership, topology, permission, or fsync check fails, the error
is returned with that partial status and startup must stop. The package does
not unmount on error: a changed mountpoint could contain somebody else's mount.
`AlreadyMounted` is true only when a matching existing mount has been verified.

Cancellation is checked before the mount syscall. After it succeeds, the
package finishes verification, mode restriction, and fsync before reporting a
cancellation. It does not abandon a goroutine blocked inside a mount syscall.

An ext4 mount can replay its journal; that is part of mounting the explicitly
selected filesystem, not a formatting or repair action. Directory fsync does
not replace the fsync/rename/parent-fsync sequences required by release,
configuration, key, and lease writers. Durability still depends on the storage
stack honoring flushes. This package provides neither a hardware power-loss
guarantee nor a hardware anti-rollback counter.

Exclusivity checks cover the current mount namespace. Provisioning must ensure
that another host/namespace does not concurrently write the same volume. This
package does not perform cloud attachment, partition discovery, LVM/RAID setup,
encryption unlock, filesystem resizing, kernel/rootfs updates, or schema
migration.

## Verification

```sh
go test -race ./internal/storage
```

Unit tests use an inert mount backend, a synthetic clock, constructed mount
tables/superblocks, ordinary temporary files, and read-only procfs metadata.
**They never mount a host filesystem or create a block device.** They exercise
device timing, process guards, conflicting mounts, source/target descriptor
identity, effective filesystem policy, repeated calls, mount failures,
post-mount failures, and cancellation after a successful mount.

The separate `scripts/qemu-storage.py` fixture exercises actual PID 1 startup
with a private regular-file virtio disk inside a no-NIC guest, then a second
kernel boot of that disk. Its own manifest and serial witnesses report whether
persistent config, SSH identity, and unconfirmed-release fallback passed. That
gate is distinct from these unit tests; it formats only its newly created test
image and never mounts it on the host.

## Primary references

- [Linux mount system call](https://man7.org/linux/man-pages/man2/mount.2.html)
- [Mountinfo identities, roots, propagation, and escaping](https://man7.org/linux/man-pages/man5/proc_pid_mountinfo.5.html)
- [Descriptor mount IDs](https://man7.org/linux/man-pages/man5/proc_pid_fdinfo.5.html)
- [ext4 mount options and journal behavior](https://www.kernel.org/doc/html/latest/admin-guide/ext4.html)
- [ext4 superblock layout and stored defaults](https://www.kernel.org/doc/html/latest/filesystems/ext4/super.html)
- [Kernel ext4 option reporting](https://kernel.googlesource.com/pub/scm/linux/kernel/git/stable/linux/+/refs/tags/v6.12.38/fs/ext4/super.c)
