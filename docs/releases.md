# Signed userland releases

routerd2 updates the two runtime executables as one signed release:
`routerd` and `routerd-modules`. The current appliance boot path can select a
pending pair, execute the verified routerd inode as PID 1, pin the matching
modules inode, and confirm the trial after continuous process/configuration
readiness. An explicit failure selects the last confirmed pair; unconfirmed
boots have a bounded retry policy.

This updates userland only. Kernel, root filesystem, bootloader, persistent
data-partition provisioning and configuration-schema migration are separate
work. Containers use a pinned OCI image, not this appliance boot selector.

## Build and sign on the publisher

The Linux-only `cmd/routerd-bundle` program is a build-time publisher. It is
not installed in the appliance or container and does not add another runtime
executable. It uses the same manifest, ELF and archive checks as the receiving
release store and never executes the input binaries.

Provide an Ed25519 signing key as one unencrypted PKCS#8 `PRIVATE KEY` PEM
file. The key must be owned by the effective publisher UID, readable only by
that owner, have exactly one hard link, and reside behind no symlink path
components. Do not place the key in the repository or release artifacts.
The tool does not automatically generate or install production trust keys.

Build both targets with the exact intended identity:

```sh
release_id="$(git rev-parse HEAD)"
make build-linux RELEASE_ID="$release_id"
make release-built-test RELEASE_ID="$release_id"

go run ./cmd/routerd-bundle trust \
  --private-key-file /secure/routerd-publisher.pem > /secure/release-keys.json

go run ./cmd/routerd-bundle pack \
  --private-key-file /secure/routerd-publisher.pem \
  --routerd "$PWD/dist/bin/linux-amd64/routerd" \
  --modules "$PWD/dist/bin/linux-amd64/routerd-modules" \
  --release-id "$release_id" --sequence 42 --target-arch amd64 \
  --output "$PWD/dist/routerd2-linux-amd64-42.tar"
```

Sequence `42` is an example: allocate a strictly increasing publisher counter
for subsequent releases. The identity is an opaque label, not a semantic-version
comparison. Use the arm64 builds and `--target-arch arm64` for that target.

All input/output paths must be clean absolute paths. The output directory must
already exist. The publisher writes a temporary file, validates and signs the
complete pair, synchronizes the file, then publishes with a no-overwrite rename
and synchronizes the directory. It refuses to replace an existing bundle. A
post-publication reporting or directory-sync failure explicitly identifies the
published path; do not infer that no file exists from a nonzero exit alone.

The `trust` output is a public JSON map from full key IDs to Ed25519 public keys.
Install that file through the image's explicit local trust configuration. A
bundle cannot install its own trust key or change the trusted-key policy.
The `pack` result contains the manifest digest, target, sequence and artifact
hashes/sizes, with no private key material.

The bounded update archive contains exactly four regular files:
`manifest.json`, `manifest.sig`, `routerd` and `routerd-modules`. Distribute the
project LICENSE and the exact build's `dist/licenses` alongside it. This archive
format does not replace the license/source materials needed for a full Linux
image distribution.

## Install a baseline and queue an update

The appliance needs a provisioned persistent filesystem at the chosen store
location. A boot document can use [`state_volume`](../internal/runtimeconfig/README.md)
to mount an existing ext4 device before PID 1 opens the release store. The default
direct-boot initramfs has no volume configured and remains volatile. Neither the
image builder nor routerd formats or provisions a data disk. An installer stages
and explicitly initializes its known-good baseline using the local release
commands. Merely initializing it does not prove that the baseline boots.

```sh
routerd release stage \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --file /var/lib/routerd/incoming/routerd2-linux-amd64-42.tar

routerd release status \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64

routerd release prepare \
  --root /var/lib/routerd/releases \
  --trust-file /etc/routerd/release-keys.json --target-arch amd64 \
  --digest "<manifest-digest-returned-by-stage>" \
  --expected-generation 3 --max-attempts 1
```

Use the actual current generation from `status`, not the example value `3`.
`prepare` requires a newer publisher sequence and reserves enough journal
capacity for its allowed boot attempts and confirmation/fallback. A capacity
failure leaves selection unchanged and calls for explicit cleanup with
`release prune`. Commands, initial baseline setup and rollback semantics are
described in [releasecli](../internal/releasecli/README.md).

The incoming local bundle must be provisioned by the operator or image-builder.
The current embedded SSH CLI does not provide a bundle-upload/SFTP subsystem.
Preparing a release changes next-boot selection, not the running processes,
and does not itself reboot. The local commands are available as routerd command
invocations; they are not arbitrary shell commands through the SSH CLI.

Enable signed boot selection in the appliance's existing boot JSON:

```json
{
  "release": {
    "root": "/var/lib/routerd/releases",
    "trust_file": "/etc/routerd/release-keys.json",
    "ready_timeout_seconds": 120,
    "stable_seconds": 10
  }
}
```

This is the `release` fragment, not a complete boot configuration. It requires
explicit `platform: appliance`. The selected modules descriptor replaces the
ordinary `module_path` for this boot. Normal appliance boots without the
fragment retain their fixed image-provided pair.

## Health and failure boundaries

Health confirmation checks actual supervisor PID 1, selected release identity,
all required workers, stable process generations, committed configuration
revision, no outstanding confirmed-config transaction, and no unresolved route
projection issues. A restart or revision change resets the stability window.
This proves local process/configuration readiness, not ISP or Internet reachability.

Signature/payload revalidation before confirmation has the remaining overall
trial budget, allowing for slower CPUs and storage. The store checks cancellation
again before beginning the durable confirmation. A journal commit already in
progress cannot be rolled back simply because its caller's deadline expires.
Failure recording gets a separate bounded verification budget; a failed durable
write is reported explicitly rather than treated as a successful rollback.

The kernel boot ID, trial token and manifest digest fence every confirmation.
Re-entering the same boot through descriptor execution does not consume another
attempt. The store's active selection is durable boot metadata and is not, by
itself, proof of what a running process currently executes.

An independent hardware/hypervisor watchdog is needed to recover from a PID 1
hang before the monitor starts, a stalled kernel/module load or blocked storage
I/O. The current image does not install such a watchdog. A/B userland metadata
alone cannot reset an unresponsive machine.

## Verification

`make release-built-test` verifies both actual amd64/arm64 command pairs and
the offline publisher-to-runtime bundle path. Unit/race tests cover malformed
archives, signatures, ELF identity, generation/token fencing, capacity, pruning,
key permissions, cancellation, concurrent publication and process crashes at
journal/staging boundaries.

`make qemu-release-boot` boots a separate no-NIC guest, stages signed baseline
and trial binaries, and exercises the product's real PID 1 descriptor handoff.
An independent probe checks each worker's executing inode, local health and the
final confirmation record before clean guest poweroff. The fixture retains
hashes and serial evidence under `dist/release-boot-test`.

That gate uses a temporary in-memory store. The separate
[`qemu-storage` gate](../images/storage-test/README.md) exercises an actual ext4
disk across two kernel boots, persisted SSH edits/host keys and automatic fallback
from an unconfirmed signed userland attempt to its baseline. Neither gate proves
bootloader fallback, physical power-loss durability or kernel/rootfs switching.
See the [release store contract](../internal/release/README.md) and
[PID 1 gate](../internal/releaseboot/testdata/README.md) for exact boundaries.
