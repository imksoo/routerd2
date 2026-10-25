# Real PID 1 signed-release gate

This integration fixture boots the actual routerd command pair in a dedicated
QEMU guest. It checks descriptor-based replacement of PID 1, matching worker
executables and automatic health confirmation of a signed userland trial.

The guest has no external network interface, shared disk, forwarded host port
or host filesystem mount. QEMU uses TCG and requires no KVM device. The fixture
and its management probe are test-only programs and are never included in
production images.

## Run

Provide an appliance image directory containing a matching `vmlinuz`,
`initramfs.cpio.gz` and `manifest.json`. The runner copies only regular kernel
module files from that initramfs; it builds all userland executables from the
current repository source.

```sh
python3 internal/releaseboot/testdata/qemu-release-boot.py \
  --go go --image-dir dist/image --output dist/release-boot-test
```

The host needs Python 3.8 or newer, the project's Go toolchain and
`qemu-system-x86_64`. The source image must contain the drivers needed by the
normal empty router configuration: `nf_tables`, `nft_ct`, `nft_chain_nat`,
`nft_masq` and `ip6_tunnel`, including their dependencies.

The runner builds Linux amd64 static commands with baseline CPU settings. It
sets two distinct embedded release identities, `release-gate-baseline` and
`release-gate-trial`, in both members of each pair. The fixture helper is a
separate initializer and probe, excluded from the ordinary production build.

## What must happen to pass

1. The test initializer starts as global PID 1, verifies a dedicated kernel
   command-line flag and mounts the same basic kernel interfaces as routerd.
   It rejects any guest NIC. Loopback and the kernel's unused `ip6tnl0` fallback
   tunnel created by `ip6_tunnel` are permitted; the latter must have the
   expected tunnel hardware type and no device attachment.
2. It generates a temporary Ed25519 key inside the guest, signs and stages both
   actual executable pairs, initializes the baseline and prepares sequence 2
   as the next trial.
3. It starts an independent observer process and execs the verified baseline
   routerd descriptor while retaining PID 1.
4. The product's appliance boot code reads the real kernel boot ID, selects the
   pending trial and execs its verified routerd descriptor. Re-entry uses the
   same boot attempt. It pins the matching modules descriptor.
5. The real supervisor starts logging, network, control and management workers.
   The boot configuration deliberately names a nonexistent `workers` catalog:
   startup succeeds only if the signed-release integration replaces it with
   the verified descriptor.
6. Product health monitoring observes the required ready process tree and a
   healthy committed configuration continuously for two seconds, then confirms
   the exact boot ID, token and manifest digest.
7. The independent probe verifies supervisor PID 1 and release identity, all
   four workers' readiness and identity, and each running `/proc/PID/exe` inode
   against the selected stored ELF. It verifies healthy control, matching
   service/config revisions, active trial, previous baseline, no pending
   attempt, retained high watermark, `operation=confirm` and unchanged kernel
   boot ID.
8. After observing confirmation, the probe makes a real control commit and
   snapshot. Checked preparation rejects a stale live revision and a missing
   actual snapshot without consuming authority. During the successful preflight,
   the actual previous runtime's descriptor must be open, live store/revision
   must match, and a competing real control commit must return HTTP 503 while
   the release file lock is held. It prepares a paired rollback intent through
   these internal primitives
   while PID 1 and the selected workers continue running. Exact request replay
   must retain revision 2, a new commit must return HTTP 409 without changing
   the authoritative journal, and cancellation must retain store identity and
   the reserved configuration generation 11. A new commit must then reach
   revision 3. This tests the live writer fence without activating paired boot.
9. The probe emits both its execution and commit-fence witnesses and PASS marker,
   drains the serial console and
   powers off the guest. The host requires QEMU to exit successfully after
   those markers; a marker without clean guest poweroff does not pass.

The probe waits for supervisor readiness before opening the release store, so
its read-only checks cannot lock the store during initial boot selection.
Temporary contention with health confirmation is retried using the product's
explicit busy response. After re-verifying the executable files it always
leaves an idle interval, so the observer cannot monopolize the store lock on a
slow CPU. Once confirmation has been witnessed, a fence failure ends the test;
the probe does not restart the transaction or retry mutations until they pass.
Unix API observations have a five-second bound. Full artifact re-verification
uses the remaining existing 125-second probe budget, instead of inheriting the
short RPC deadline. Both stages retain the global deadline and include their
stage name on error. The fixture's deadline-contract unit/Race/vet checks are
explicit Makefile targets because Go's `./...` pattern excludes `testdata`.

## Outputs and boundaries

The output directory contains a dedicated initramfs, serial log and JSON result
manifest. The host requires both witnesses before accepting PASS. The manifest
records kernel, source-image manifest, initializer,
executable and initramfs hashes, the observed confirmation witness, elapsed time
and QEMU exit status. A failed rebuild removes old passing artifacts before
starting; a renamed or absent PASS marker cannot be interpreted as success.

Keys are created inside the ephemeral guest and are not production trust
material. The release store and configuration state use the guest initramfs.
They do not survive guest shutdown.

This gate proves a same-kernel userland transition, real health confirmation and
the running control worker's paired-prepare commit fence/replay/cancel behavior.
The raw paired intent is canceled before any boot. It does not prove a paired
runtime/configuration loader, compatible target preflight or paired trial health.
The snapshot is resolved using the running compiler; target ABI/capability/state
qualification is still a separate requirement.
It does not test two separate boots using a persistent disk, bootloader fallback,
kernel or root-filesystem updates, data-partition mounting, schema migration,
hardware watchdog recovery or appliance power-loss durability. Release unit
tests separately exercise bounded boot attempts, explicit failure, rollback and
process-crash transaction recovery. A persistent two-boot appliance gate remains
separate work.
