# Signed userland releases

This document describes the **unreleased prototype** format. There are no
deployed users of this format, and backward compatibility is not required.
The signed manifest v3 declares a variable artifact set, its native command
identities, entrypoint, role bindings and embedded runtime format declaration.
Native static ELF and regular data are supported; former v1/v2 bytes are refused.
Appliance boot
executes the verified entrypoint inode and pins each declared worker role.
Image staging, boot selection and explicit startup-configuration selection
remain separate contracts; the latter is still pending. See
[ADR 0001](adr/0001-boot-runtime-startup-config.md).

## Native format declarations

Every native executable contains one complete canonical
`ROUTERD2-RUNTIME-ABI-V1:...:END` record in a readonly allocated ELF section.
The publisher derives `runtime_abi` from those actual input bytes and requires
all native members to agree. There is no layout flag that substitutes a claim.
The receiver checks the signed declaration against each native artifact at
stage and whenever the installed set is reopened. The executing supervisor also
checks its own declaration against the selected signed manifest.

Each reopen still reads and hashes every complete payload and verifies the signed
manifest. Within one Store lifetime, successful ELF/identity/ABI parsing of those
same bytes may be reused under the manifest-digest/artifact-name key. That key
binds every expected command, release, architecture and ABI claim. It never
substitutes for the current payload hash. The memo holds at most 256 entries,
retains no payload bytes, and is discarded when the Store closes or its bound
is reached. Changed signed metadata and changed payload bytes require new checks.

The record describes profile/boot/worker contracts, configuration semantics and
schema reader/writer versions, and named persistent-state reader/writer versions.
It is bounded to 4KiB and 32 sorted unique state components; versions are positive
and at most 65535, and each writer version must be readable by its own runtime.
Scanning is streaming, rejects multiple complete valid records, caps malformed
marker candidates, and bounds readers that stop making progress.
The manifest signature domain is `routerd2 signed extracted runtime manifest v3\x00`.
The release identity record remains V1; its format is independent of manifest
version and the runtime ABI record envelope.

The current declaration has configuration schema 2 and semantics contract
`resource-program-native-12`. Boot ABI is 1 and worker ABI is 3. Explicit
multihop BFD uses committed local sources and routed UDP 4784 endpoint pairs;
single-hop retains its device-bound UDP 3784 scope. VRRP tracking
resolves declared resource IDs, shares finite Health/BFD/BGP observations, and
uses explicit healthy/unhealthy/neutral/unknown decisions. Its wire/configuration
contract replaces the discarded service-only tracking form. DHCP server and DNS
lease projections retain original boot-scoped deadlines without receipt leases. Its named formats
are configuration journal 1, events SQLite 6, network journal 5, release journal
1, snapshot 1 and SSH identity 1. Configuration journal and SSH identity numbers
name codec contracts; they are not extra fields inserted into existing files. A persistent-format or
breaking input-semantics change must update the declaration and qualification.
Tests tie explicit format implementations to these claims without linking the
configuration compiler into the declaration package or independent logger.

Current prepare/boot/direct rollback/fallback require equal format declarations.
Even a successful generic preflight callback cannot authorize a format-changing
migration. Such runtimes can be staged for inspection; activation needs a
qualified configuration/state migration workflow that is not implemented yet.
The pure `runtimeabi.CheckTransition` helper additionally checks trusted observed
versions, both runtimes' writers, required facts and configuration semantics.
An explicit version zero fact means an inspected absent file, never an omitted
observation. The helper neither collects facts nor restores identity, lease,
owner or revocation state.

These are publisher declarations bound to executable bytes, not a proof that
code implements them correctly. Release qualification, actual state inspection,
target compiler/kernel/engine capability checks and revocation policy remain
required. No paired loader or operator migration workflow is activated here.

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
  --layout /build/runtime-layout.json \
  --release-id "$release_id" --sequence 42 --target-arch amd64 \
  --output "$PWD/dist/routerd2-linux-amd64-42.tar"
```

Sequence `42` is an example: allocate a strictly increasing publisher counter
for subsequent releases. The identity is an opaque label, not a semantic-version
comparison. Use the arm64 builds and `--target-arch arm64` for that target.

All input/output paths must be clean absolute paths. The output directory must
already exist. The publisher writes a temporary file, validates and signs the
complete artifact set, synchronizes the file, then publishes with a no-overwrite rename
and synchronizes the directory. It refuses to replace an existing bundle. A
post-publication reporting or directory-sync failure explicitly identifies the
published path; do not infer that no file exists from a nonzero exit alone.

The `trust` output is a public JSON map from full key IDs to Ed25519 public keys.
Install that file through the image's explicit local trust configuration. A
bundle cannot install its own trust key or change the trusted-key policy.
The `pack` result contains the manifest digest, target, sequence and artifact
hashes/sizes, with no private key material.

Copy and edit [runtime-layout.json](../examples/runtime-layout.json) with the
clean absolute source file paths for the chosen architecture. A layout has
explicit `boot_abi`, `worker_abi`, entrypoint, artifact kind/command/source and
role bindings. Source paths never enter the signed manifest. Multiple roles
may share a native artifact; separate artifacts may use the same native command.
No arbitrary argv or host package install hooks are accepted.

The uncompressed USTAR archive starts with `manifest.json` and `manifest.sig`,
then contains every declared artifact in manifest order. Names are bounded
single relative filenames in this initial layout, without links or directories.
The limits are 64 artifacts, 64 roles, 64KiB manifest, 1GiB per artifact and 4GiB
aggregate payload. Receiver artifact policy defaults to 512MiB per artifact.
Native files are static project Go executables; external engines, dynamic
loaders/libraries/plugins and nested artifact layouts need further work and
are refused by this format. Regular data may contain notices or inventory;
this is not a claim of complete source/SBOM/license distribution qualification.
Distribute all required exact-build license and source materials.

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
explicit `platform: appliance`. The signed entrypoint and role catalog replace the factory `workers` catalog
for this boot. A direct factory boot supplies an explicit role-to-executable
`workers` object. The old `module_path` field is refused; worker paths and argv
cannot be supplied through dynamic configuration or the worker reconcile API.

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

`make release-built-test` verifies the actual amd64/arm64 three-command catalogs and
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

Appliance boot opens the existing selector through `release.OpenForBoot` so
verification of a damaged pending payload does not prevent reaching its
verified fallback. Normal operational opens still reject damaged references.
The boot transaction checks the complete journal and all noncandidate payloads
before recording failure and selecting the baseline. A previously executed
trial additionally requires its signed ABI metadata to authenticate and match
the fallback. Missing/corrupt journal, unusable baseline, lost trial metadata
or unqualified format changes are refused without selecting an executable.
Neither an arbitrary old runtime nor the factory executable can replace those
checks. A pending update without a baseline records failure and returns no
image. A separate storage fixture mode, `--corrupt-trial`, changes the signed
trial payload before its second boot while another attempt remains; the normal
attempt-limit gate remains separate. Configuration adoption and all-state
migration are not activated by this recovery path.

## Runtime/configuration journal primitives (not an activated boot flow)

The internal release store can prepare an explicit `Startup` selection with a
`KnownGood` configuration reference. It publishes the candidate runtime, exact
configuration intent and conditional fallback in one current-generation switch.
Configuration store identity and candidate/recovery generation reservations
survive cancellation and failure. The release store validates reference shape
and transaction coherence; the coordinator must verify the snapshots, target
compatibility and live configuration revision before invoking it.

`BeginBoot` keeps the same intent across exec handoff and bounded attempts.
`FailBoot` selects the previous runtime and its prepared conditional recovery
together. A paired trial cannot use runtime-only confirmation:
`ConfirmConfiguration` requires the exact adoption receipt and unchanged actual
configuration revision. It is a caller-observed assertion, not an internal
health probe. `PrepareRollback` queues a new paired trial for the known previous
runtime, keeping both high watermarks; direct confirmed-pair rollback is refused.

These are library/state-machine primitives. There are no corresponding CLI or
SSH prepare flags yet, and the current appliance command explicitly refuses a
selected paired intent rather than booting its runtime without its configuration.
The coordinated loader, profile/kernel/state compatibility checks and full
runtime/configuration health confirmation remain to be wired and accepted.
Existing runtime-only prototype gates do not qualify this flow.

## Configuration writer fence for a selected signed runtime

Signed appliance boot passes private root/trust-file/manifest-digest context to
the control worker. The supervisor validates and copies it once, overrides any
inherited or caller-provided environment entry, and clears it for other workers.
The worker opens the protected trust policy and release store independently.
Before opening/adopting configuration it checks the exact selected runtime and
startup intent under the release lock; a bound store identity must already match.
This context is not an operator boot-config field or an authorization credential.

Control holds its own writer mutex, then the release process mutex and file lock,
through stage, kernel/protocol apply, journal save, and any failed-apply restoration.
Confirmed rollback, restart reconciliation and configuration confirmation use the
same lock order. A competing release transaction returns busy (configuration HTTP
503); a stale runtime, foreign store or unconsumed startup returns HTTP 409.
There is no unlocked check-then-apply window. The release callback does not call
back into control or recursively acquire its own store.

A pending paired intent freezes new commits and changes to an unconfirmed record,
both before boot and during a trial. Exact durable request replay remains an
acknowledgment without repeating apply. Canceling the candidate or confirming the
exact adopted pair unfreezes writes; cancellation retains authority reservations.
Reapplying an already confirmed configuration remains possible during prepare.
Runtime-only pending releases retain their existing configuration behavior.

This is the writer side of coordination. The raw release store still cannot
preflight live configuration revision, unconfirmed state, snapshots or target
compatibility. Preparing a stale raw intent is therefore not a supported operator
workflow. Paired signed boot remains explicitly refused until the coordinator,
loader, health confirmation and fallback policy are connected and accepted.

## Checked preparation before publication

`PrepareChecked` and `PrepareRollbackChecked` extend the internal primitives
with a required preflight callback. They hold the release mutex and file lock
across reading the current state, verifying the actual target descriptors,
validating the pair's reference/generation bounds, running preflight and
publishing the candidate. Rollback supplies the actual previous runtime to the
callback. A malformed pair is rejected before invoking preflight.

The callback receives an independent copy of the current state and borrowed
target descriptors. Input startup/known-good references are frozen before it
runs; modifying callback metadata cannot change the published pair. Callbacks
must not recursively acquire the same store or retain/close borrowed descriptors.
Failure or cancellation before publication leaves both watermarks and selector
unchanged; a canceled context is rechecked after the callback returns.

This provides a place for the coordinator to verify the live configuration and
actual immutable snapshots while its release lock excludes configuration writes.
It does not supply the coordinator's target ABI, capability, state or revocation
policy. The raw preparation methods remain lower-level primitives, and there
are no new operator CLI/SSH flags or activated paired loader in this change.
Resolving a snapshot with the running compiler is not proof that the target
runtime supports it.

The configuration store now exposes `InspectStartup` for the configuration side
of preflight. It shares adoption's checks and returns actual journal/snapshot
observations without writing or syncing. Both candidate and conditional fallback
inputs must be inspected while writers are excluded, and preparation must reject
an already consumed candidate rather than interpreting replay as new authority.
The signed PID 1 fixture inspects both inputs before preparation and cancels the
pair before boot. This does not activate the paired loader or inspect target
compiler behavior and other persistent state. See [startup configuration](startup-config.md).

The network writer now exposes bounded actual journal metadata through its
private `GET /v1/journal` endpoint and `network.InspectJournal`. They share the
startup reader's strict format/ownership checks, never renew stored deadlines or
apply kernel state, and do not treat an unreadable journal as cached success.
The signed boot fixture compares worker and direct disk observations under the
preparation lock. This contributes a network format observation; it is not a
complete state inventory, an atomic observation of other workers, or evidence
that saved lease/ownership facts are fresh. See the [network journal contract](../internal/network/README.md).

`management.InspectHostKey` adds a protected read-only observation of SSH host
identity codec/presence and public fingerprint. Explicit key initialization uses
atomic no-replace publication, and existing keys are never repaired or replaced
on parse failure. The signed fixture observes absence with SSH disabled; the
two-boot storage fixture inspects the actual persistent management key and SSH
peer. This is not key rotation/revocation or complete state qualification. See
the [identity contract](../internal/management/README.md).

The event store's `InspectFormat` and private `GET /v1/event-store/format` endpoint
inspect the actual owned SQLite header/catalog without registration, migration
or a second initializing store. Required DDL is authenticated before writable
open; update qualification requires an exact catalog, including rejection of
extra triggers/views. Its schema digest describes format, not event contents or
lease freshness. The signed preparation fixture now checks observations for all
six currently declared state families against both candidate and fallback input
formats; it still cancels before boot. Protocol/engine-specific state, complete
target policy and the operator paired workflow are separate unfinished work.
See the [event store contract](../internal/events/README.md).
