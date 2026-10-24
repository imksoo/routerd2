# Health queue recovery after abrupt guest power loss

This test-only initializer and independent probe boot the real `routerd` as
appliance PID 1 on three kernel boots, with one private ext4 disk and no NIC.
They are never shipped in a production appliance.

```sh
umask 022
python3 scripts/qemu-health-power.py \
  --image-dir dist/image \
  --output dist/health-power-test
```

The host needs QEMU and `mke2fs`, and the source appliance image must contain its
kernel, initramfs, manifest, and virtio/ext4 dependencies. The runner accepts no
host disk argument. It formats only a newly created, pinned, mode-0600 256 MiB
regular file. There is no host mount, loop device, real block device, or external
network. The guest uses 1 GiB RAM and two vCPUs, matching the existing storage
fixture rather than claiming a minimum-memory profile.

The three phases exercise different durability boundaries:

1. **produce:** Actual loopback TCP probes are accepted into the worker's
   private queue while an SQLite trigger rejects their target publication.
   After at least two passed probes, the probe SIGSTOPs the actual health
   process and reads every accepted queue entry. It verifies that none is in
   the target journal and emits their original IDs and full-content hashes.
   The host SIGKILLs the still-running QEMU process. The guest does not stop
   children, close stores, sync, unmount, or power off at this cut point.
2. **registry:** The product mounts the same unclean disk and reopens its own
   state. The probe verifies every original queue entry, then uses the real
   control transaction to withdraw health and its current Derived observation.
   It retains the independent history EventRule. A new SQLite trigger rejects
   only registry ACKs; removing the target fault lets control publish and
   acknowledge all pending probes, including new probes accepted on this boot.
   The probe verifies content, single publication, positive cursors, an empty
   local queue and a retained unhealthy registration. It rejects an old
   supervisor's history/current messages. The host SIGKILLs QEMU again.
3. **recovered:** A third actual kernel boot mounts the same disk without a
   helper mount or repair. The product must retain the empty local ACK and the
   pending registry, without recreating health or Derived current state. Event
   content and cursors must be unchanged. Removing the registry fault must
   restore healthy control with zero registrations. The retained EventRule must
   consume the recovered history. Only this final phase powers off normally.

Only the first initializer mounts the disk for fixture seeding, before any
worker exists, and it unmounts before execing the product. Later initializers do
not pre-mount, clean, repair, or recover the disk on the product's behalf. This
test uses the explicit factory command pair in `Boot.ModulePath`; signed runtime
selection and fallback remain the separate `qemu-storage.py` gate.

Expected IDs/hashes/cursors leave the guest through UART. The host supplies the
previous witness as `/test/expected.json` in the next test initramfs, not by
writing a witness to the guest's data disk. The archive intentionally changes
to carry that test-only file; each phase records and checks its own immutable
archive hash. Kernel and executable inputs stay pinned. No host fsync is
inserted between the power cuts. The first empty filesystem format is synced
before any observation exists.

The verifier requires complete UART lines, one exact witness per phase, three
distinct canonical boot IDs, the correct original authority, actual PID 1 and
fresh executable inode checks. An orderly exit cannot count as a cut: both cut
phases must end with host SIGKILL and QEMU status -9. Missing/changed/duplicate
events, changed cursors, pending work hidden by a valid-looking witness, and
guest failures cannot pass. There is no automatic retry or filesystem repair.

The output contains the manifest, all serial logs, per-phase test initramfs
archives and the final private disk image. These raw artifacts can contain
fixture state and are not intended for a public commit; publish reviewed,
normalized evidence separately.

This models loss of the guest VM's process/device memory on an otherwise
running host. It does not power off the PVE host, discard host filesystem caches,
qualify a physical storage controller, or prove recovery from every possible
write boundary. Physical/NIC/performance qualification, full version migration,
retention and other implementation-plan work remain separate requirements.
