# Persistent state and signed fallback fixture

This test-only image boots the real routerd command pair as PID 1 twice, with
one ext4 data volume and no external network interface. The guest initializer
and its independent probe are never installed in a production image.

```sh
python3 scripts/qemu-storage.py \
  --go go \
  --image-dir dist/image \
  --output dist/storage-test
```

The source image must contain its `vmlinuz`, `initramfs.cpio.gz` and
`manifest.json`, including `virtio_blk`, `ext4`, `jbd2`, `mbcache` and `crc16`
modules. Dependencies are loaded through routerd's native module loader. The
host needs QEMU and `mke2fs`; it needs no loop device, host filesystem mount,
privileged container, or real block device.

## What the two boots prove

On the first boot, the test initializer mounts the dedicated disk only to
provision two freshly built and signed executable pairs, an initial routing
configuration, a fixture client key, and a random state sentinel. It initializes
the baseline and queues the trial with one allowed attempt. The signing key is
ephemeral, and all test keys belong to the fixture.

The initializer closes the release store, syncs and **unmounts** the volume,
verifies that its witness has disappeared and the underlying mountpoint is
empty, and execs the actual factory `routerd`. Production startup must load the
storage modules and mount `Boot.StateVolume` before it can open the signed
release store. It then preserves PID 1 while selecting the trial pair.

An independent guest process verifies the actual supervisor and all four core
workers against the verified release inodes. It authenticates to the embedded
SSH server on guest loopback, edits the candidate configuration, and commits
`resources[interface/lo].interface.alias = "persistent-two-boot"`. It checks committed
revision 2 and the actual kernel alias, records hashes of the committed config,
SSH host key and state sentinel, and powers off while the release trial is
still pending. The health confirmation window is deliberately longer than the
fixture, so the test does not simulate a successful confirmation.

The second boot uses a new kernel instance and the **same disk**. Before the
actual routerd starts, the initializer verifies that the journal still contains
the first boot's unconfirmed attempt. It again unmounts before product startup.
The new kernel boot ID causes the production release selector to exhaust the
single attempt and select the baseline automatically. The probe checks:

- The first and second boot IDs differ, and the failure record identifies the
  first boot and the trial release.
- The baseline is the actual PID 1 and every core worker uses its paired,
  verified `routerd-modules` inode.
- The release journal records `boot-rollback`, has no pending attempt, and
  retains its high watermark.
- Committed revision 2 survives byte-for-byte, is served over a new authenticated
  SSH connection, and is projected onto the fresh kernel's loopback interface.
- The SSH host private key hash and public-key fingerprint remain unchanged.
- The independent state sentinel survives the disk remount.
- The confirmed configuration snapshot captured through the actual control API
  resolves byte-for-byte after the second kernel boot. Its store identity,
  revision and complete payload digest match the first-boot witness; the host
  gate rejects missing, malformed or differing snapshot provenance.
- The state directory is the dedicated writable ext4 device with `nosuid` and
  `nodev`, while allowing execution of the verified release pair.

## Host isolation and evidence

The runner creates a 256 MiB, mode-0600 regular file with `O_EXCL` and
`O_NOFOLLOW` inside a private temporary directory. Only that pinned descriptor
is passed to `mke2fs` and QEMU; the runner accepts no user-selected disk device.
It rechecks the descriptor's type, size, owner, permissions and link count before
each boot. Live kernel, initramfs, disk and serial output remain private until
the guests stop. The initramfs is read through gzip's CRC trailer before boot,
and the runner verifies unchanged kernel and initramfs hashes after each boot.

Each phase must emit its exact PASS marker and one structured execution witness,
then shut down with QEMU exit status zero. Missing, skipped or renamed checks do
not pass. The host compares both witnesses before marking the complete fixture
successful. No automatic retry or filesystem repair changes a failed result.

The output directory contains `manifest.json`, both serial logs, the test
initramfs and the completed `data.ext4` image. The manifest records executable,
kernel, archive and disk hashes, the two witnesses and each QEMU result. A failed
rebuild clears previous passing artifacts before starting.

This gate verifies persistent state and userland fallback across two actual
kernel boots. It does not install a bootloader, update the kernel/rootfs, create
a cloud image, or certify recovery from arbitrary power loss during a disk
write. Those are separate image and storage lifecycle contracts.
