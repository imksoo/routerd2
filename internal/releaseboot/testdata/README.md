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
   The boot configuration deliberately names a nonexistent `module_path`:
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
8. The probe emits its witness and PASS marker, drains the serial console and
   powers off the guest. The host requires QEMU to exit successfully after
   those markers; a marker without clean guest poweroff does not pass.

The probe waits for supervisor readiness before opening the release store, so
its read-only checks cannot lock the store during initial boot selection.
Temporary contention with health confirmation is retried using the product's
explicit busy response. After re-verifying the executable files it always
leaves an idle interval, so the observer cannot monopolize the store lock on a
slow CPU.

## Outputs and boundaries

The output directory contains a dedicated initramfs, serial log and JSON result
manifest. The manifest records kernel, source-image manifest, initializer,
executable and initramfs hashes, the observed confirmation witness, elapsed time
and QEMU exit status. A failed rebuild removes old passing artifacts before
starting; a renamed or absent PASS marker cannot be interpreted as success.

Keys are created inside the ephemeral guest and are not production trust
material. The release store and configuration state use the guest initramfs.
They do not survive guest shutdown.

This gate proves a same-kernel userland transition and real health confirmation.
It does not test two separate boots using a persistent disk, bootloader fallback,
kernel or root-filesystem updates, data-partition mounting, schema migration,
hardware watchdog recovery or appliance power-loss durability. Release unit
tests separately exercise bounded boot attempts, explicit failure, rollback and
process-crash transaction recovery. A persistent two-boot appliance gate remains
separate work.
