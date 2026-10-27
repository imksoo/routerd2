# Signed userland releases

The release store installs a signed, versioned runtime with a variable set of
static native executables and regular data artifacts. Its manifest binds roles
to native artifacts and names the supervisor entrypoint. Consumers receive all
verified descriptors together, without re-resolving a mutable selector.

This package performs local verification, storage and selection. The appliance
boot integration executes the selected descriptor and uses the actual
supervisor and control health to confirm a trial. The local management CLI
queues changes; a successful prepare or rollback does not mean that the running
process tree has already changed.

`WithState` holds the process mutex and interprocess file lock across a callback,
after verifying the current state and referenced runtimes. It does not publish
callback changes. The configuration writer uses it for selection checks and its
complete apply/save/restore transaction; a separate prepare cannot enter midway.
Callbacks must not recursively call this store. Contention returns `ErrBusy`,
and callback errors/canceled acquisition release or avoid both locks.
Private boot authority and pending-pair commit fencing are described in
[the writer contract](../../docs/releases.md#configuration-writer-fence-for-a-selected-signed-runtime).
This does not activate paired boot or add snapshot/compatibility preflight to
the raw store primitives.

`PrepareChecked` and `PrepareRollbackChecked` require a preflight callback under
that same lock, after target verification and pure pair validation, before any
publication. Its current-state view is a deep copy; caller-owned references are
frozen first. The target descriptors are borrowed only during the call. Errors
or a canceled context leave the selector and watermarks unchanged. These checked
methods provide the transaction boundary for live/snapshot/target-policy checks;
they do not implement the policy, an operator workflow or paired boot by themselves.

## Bundle and trust contract

An uncompressed USTAR bundle contains canonical `manifest.json`, raw 64-byte
`manifest.sig`, then exactly the declared regular artifacts in manifest order.
Manifest v4 signs release ID, publisher sequence, target, key ID, boot/worker
ABI, the native `runtime_abi` declaration, entrypoint, role catalog, artifact
kinds, native Go command identities, SHA-256 and sizes. The domain is
`routerd2 signed extracted runtime manifest v4\x00`. Discarded manifest versions are refused.
Native ABI records are extracted from readonly ELF data without execution;
all native members must agree and match the signed manifest. There is no
publisher layout override for the declaration. See the
[format declaration contract](../../docs/releases.md#native-format-declarations).
Every reopen rehashes complete payloads; only successful structural parsing is
memoized within the Store, under the signed manifest digest and artifact name,
with a 256-entry bound and no cached payload bytes. A changed signature claim
cannot borrow another manifest's inspection, and changed bytes fail the hash
before any memo lookup. Closing the Store discards the memo.

The first extracted layout accepts bounded single filenames (no links or
nested paths), static native Go ELF and non-executable regular data. Limits are
64 artifacts, 64 roles, 64KiB manifest, 1GiB per artifact and 4GiB total payload.
The receiving store defaults to a 512MiB per-artifact policy. Boot ABI 1 and worker ABI 4 are accepted. Native executables remain static.
The PPPoE role requires a signed regular-file engine image and a distinct
corresponding-source artifact in `engines`. The image contains its exact pppd,
plugin, interpreter, libraries, OpenSSL provider, notices and package inventory.
Publisher and receiver check the actual ELF closure and target architecture.
The supervisor pins the image descriptor and passes it as worker FD 5; the
engine helper extracts it into an empty private chroot. Host engine lookup is
not supported. Other external-engine profiles and kernel capability contracts
remain unfinished.

`WriteBundle` takes an explicit layout and map of already opened files, verifies
native command/ABI identity, signs metadata and rechecks bytes while writing. Its
caller publishes only after fsync/rename/directory fsync. The offline publisher
uses `--layout`; see [releases](../../docs/releases.md). Private signing keys
never enter the runtime or artifact set.

Prepare, boot and fallback compare selected declarations under the store lock.
Until a qualified migration workflow exists, a format-changing runtime can be
staged but cannot be activated or selected by direct rollback. Callback success
is not a migration authorization. Format ranges and publisher claims do not
substitute for actual state, kernel/engine or revocation qualification.

`ParseTrust` and `LoadTrustFile` accept a JSON object mapping full key IDs to
canonical padded base64 Ed25519 public keys. The trust file may be formatted for
humans; duplicate keys, trailing JSON, null values, mismatched IDs, noncanonical
base64 and oversized input are rejected. It contains public keys only.
The file and its ancestors must be owned by root or the current effective UID
and must not be writable by other users; a root-owned sticky ancestor such as
`/tmp` is allowed. The file must be regular, have a single hard link and have no
symlink components. A public-key file with mode 0644 is accepted.

The signer cannot add trusted keys through a bundle. Keep verification keys for
every active, pending and previous release during key rotation: removing a key
that authenticates one of those references makes state verification fail.

## Verification before installation

The signature, manifest bounds and target are checked before executable bytes
are admitted. The archive parser accepts a small fixed USTAR subset. It rejects
absolute or unexpected paths, links, devices, directories, PAX and GNU
extensions, sparse entries, duplicate entries, malformed headers, inconsistent
sizes, nonzero padding, extra files and truncated or excessive trailers.

Installed executables must satisfy all of these checks:

- The size and SHA-256 digest match the signed manifest.
- They are static, little-endian, 64-bit Linux ET_EXEC files for amd64 or arm64.
  Interpreters, dynamic sections, writable executable segments and malformed or
  excessive header tables are rejected.
- Go build metadata identifies the matching
  `github.com/imksoo/routerd2/cmd/routerd` or
  `github.com/imksoo/routerd2/cmd/routerd-modules` command, with
  `CGO_ENABLED=0` and the signed target. The supported CPU baseline is
  `GOAMD64=v1` or `GOARM64=v8.0`.
- A read-only allocated `.rodata` section contains exactly one
  `ROUTERD2-RELEASE-V1:<release-id>:END` record matching the manifest.
  The executable's runtime ReleaseID is derived from that same record.

The current format deliberately accepts the project's static executable build
profile. PIE, dynamic linking and marker-free development builds are not
silently accepted. ELF validation does not establish that signed code is free
of vulnerabilities; trust in the publisher remains necessary.

The manifest is limited to 64 KiB. Each executable defaults to a 512 MiB limit,
with an explicit policy ceiling of 1 GiB. The store defaults to 32 installed
release directories and 256 journal generations. Capacity failures are
explicit; the store never silently evicts a fallback release.
Before queuing an update, `Prepare` requires enough free journal slots for
preparation, every allowed boot attempt and final confirmation or fallback.
If those slots are unavailable, it leaves selection and the high watermark
unchanged so that old generations can be pruned first. Repeated crashes may
still accumulate incomplete transactions; their removal remains explicit.

## Durable layout

`Open(Options)` requires a clean absolute private root without symlink
components. Directory-relative file operations retain anchored descriptors.
Private directories, exclusive temporary files, no-follow opens and hard-link
checks prevent path substitution within the local storage contract.

`releases/<manifest-sha256>/` contains an immutable manifest, signature and complete artifact set
of executable files. A journal generation contains `state.json` and its
`active`, `pending` and `previous` links. The generation directory name binds the
state checksum and a random nonce. One atomic `current` symlink replacement
publishes the complete generation, including all three links.

Each transaction syncs its files and directories before publishing and syncs
the parent directory after rename. Cross-process nonblocking flock and an
in-process guard reject concurrent operations with `ErrBusy`.

Reading the selected state verifies its canonical JSON, checksum, links and
referenced runtime sets. Corrupt state, missing payloads, revoked signing
keys and invalid link targets fail closed. The store does not search older
generations and guess which one an operator intended. A missing selector in an
otherwise nonempty journal also fails closed.

After publication, a directory-sync failure is `ErrUncertain`: the operation
may already be visible, and the caller must read status before deciding what to
do next. Successful fsync depends on filesystem and storage-device guarantees;
process-crash tests are not a claim of hardware power-loss certification.

`Verify` and `BeginBoot` return an `Image` with every declared artifact pinned open.
Launchers must use those descriptors, rather than independently resolving role
paths through a selector that may change. Installed payloads are never modified
in place by this package. A privileged process outside this contract can still
alter the filesystem; there is no TPM-backed rollback counter or protection
against an attacker who controls root or replaces the entire disk.

## Selection and boot state machine

The release ID is a display identity. Only the signed integer `Sequence` is used
for the update policy. `HighWatermark` advances on preparation and survives
cancellation, failure and explicit rollback.

| Operation | Preconditions and result |
| --- | --- |
| `Stage` / `StageFile` | Verify and install a runtime; selection is unchanged. Repeating a valid staged bundle is idempotent, including at store capacity. |
| `Lookup` / `Verify` | Re-verify an installed manifest digest or complete reference. |
| `Initialize` | An empty store only: register an explicitly installer-approved baseline. This declares prior knowledge; it does not boot or health-test the binary. |
| `Prepare` | Match the expected journal generation, have no outstanding trial and provide a sequence above the durable high watermark; queue the pending runtime. |
| `BeginBoot` | Choose and pin all runtime artifacts. A queued trial records the old active release as fallback and creates a boot attempt token. |
| `OpenForBoot` | Open an existing selector and finish `BeginBoot` before exposing operational store access, so a damaged pending payload cannot prevent verified fallback. Never initialize a missing boot selector. |
| `Confirm` | Match the current boot ID, token and selected manifest digest; clear the pending attempt and retain the previous confirmed release. |
| `CancelPending` | Match the generation and cancel an untried pending runtime. An already running trial cannot use this shortcut. |
| `FailBoot` | Match the active boot ID and token, record the failure and select the previously confirmed release. |
| `Rollback` | Match the generation and give a reason; explicitly select a confirmed previous release. A failed trial never becomes a fallback. |
| `Prune` | Match the generation; remove old journal generations, incomplete transactions and only explicitly named unused releases. Current active, pending and previous references are protected. |

Calling `BeginBoot` again with the same kernel boot ID returns the same attempt
and token without consuming another retry. A different boot ID consumes another
attempt, bounded by `MaxAttempts` (default 1, maximum 5). Exhaustion selects the
confirmed fallback. If there is no fallback, the failure is recorded and
`ErrNoFallback` is returned with no image; it is never reported as a successful
rollback. Corruption is not treated as an excuse to bypass verification.

Appliance startup uses `OpenForBoot` rather than ordinary `Open` followed by
selection. It first checks the canonical, checksummed journal, generation and
exact reference symlinks under the store lock. Every noncandidate reference
must still pass its signature, complete payload hashes and native checks.
If only the pending payload fails, one journal transaction rejects it and
returns the fully verified baseline. An untried candidate preserves the current
startup selection; a damaged trial uses its existing conditional fallback and
requires the candidate's signed ABI metadata to remain authentic and equal to
the fallback ABI. Recovery preserves runtime/configuration watermarks and stores
the rejected reference as failure history, never as a future rollback target.
The actual configuration adoption remains a separate coordinated-loader task.

Ordinary `Open`, status and management operations remain strict. A corrupted
selector/journal/link, an unverifiable baseline or incompatible/lost signed
trial ABI is refused without changing the selector. There is no scan for an
arbitrary older release and no factory-image shortcut. Damage to an already
confirmed active runtime is also refused; this automatic recovery policy is
limited to a pending/trial update. Before publication, cancellation or an I/O
error leaves the old selector; after publication an unconfirmed directory sync
returns `ErrUncertain`. Failed initial installation records `ErrNoFallback`
without returning any executable. No boot call silently initializes a journal.

All CLI state changes that use optimistic concurrency require the exact
`expected-generation` from status. Health confirmation instead uses the
unforgeable-per-attempt token together with boot ID and active digest. These
tokens fence stale local callers; they do not make an untrusted root user safe.

`BeginBoot` is appropriate only once the kernel boot ID is available. The
appliance integration preserves PID 1 while execing the selected routerd file
descriptor, pins the declared role executables, and runs a bounded health trial.
Only the actual required process tree plus a healthy committed configuration
can confirm that trial. The release library itself has no health timer,
hardware watchdog, payload execution or reboot mechanism.

Confirmation re-verifies the selected files, which may take longer than a short
IPC request on slow storage. The boot monitor gives it the remaining overall
trial deadline. `Confirm` checks the context again after verification and
before starting its journal commit. Once that commit starts, blocking
filesystem sync and rename operations cannot be undone by context cancellation.
Resetting a kernel or device stuck in I/O requires the separate watchdog/reset
mechanism.

## Scope of the extracted runtime bundle

This format updates userland code. It does not update or mount a persistent
root filesystem, select a kernel, configure a bootloader, create disk
partitions, coordinate fleet rollouts or migrate configuration and state
schemas. A disk-backed release store must already be available to the boot
integration. Initramfs-only storage does not survive reboot.

Recovery after a hang before the health monitor starts requires a separate
reset mechanism, for example an appliance watchdog or hypervisor policy.
Repeated crash tests cover transaction publication boundaries; a complete
power-failure appliance qualification needs persistent media and the actual
boot/reset chain.

Each code update must preserve the stored configuration/state schemas needed by
its fallback, or ship a separately designed reversible migration. A valid
signature and matching executable identity do not prove schema compatibility.

Signed regular data may carry notices and inventory. Still distribute the
project LICENSE and the dependency notices and licenses for the exact build in
the accompanying release or image materials. The repository's distribution
license inventory remains part of producing a redistributable release.

## Validation

Unit and race tests cover canonical signatures, archive attacks, ELF metadata,
identity and bounds, stale callers, exact boot-attempt idempotence, bounded
retries, no-fallback failures, downgrade policy, explicit rollback, concurrency,
capacity, pruning and corrupt or missing state.

Process-crash fixtures exit abruptly without running defers at seven journal
publication boundaries and five bundle staging boundaries. They reopen the
store, verify the selected generation, and clean incomplete transactions.

`TestBuiltRuntimeCatalog` additionally checks the actual project build's six
ELFs (amd64 and arm64 native catalogs), writes signed bundles, stages them, looks them up,
initializes the store and pins the selected catalog. It never executes a foreign
architecture payload.

```sh
go test -race ./internal/release ./internal/releasecli

ROUTERD2_INTEGRATION_REQUIRED=1 \
ROUTERD2_RELEASE_BUILT_DIR="$PWD/dist/bin" \
ROUTERD2_RELEASE_BUILT_ID="<exact-built-release-id>" \
go test -race -tags integration -run '^TestBuiltRuntimeCatalog$' \
  ./internal/release
```

The real-PID-1 trial gate is documented under
`internal/releaseboot/testdata/README.md`. It uses a VM with no external network
interface; its scope is userland selection and health confirmation.

## Sources and license

- [Go Ed25519](https://pkg.go.dev/crypto/ed25519)
- [Go build information reader](https://pkg.go.dev/debug/buildinfo)
- [Linux rename semantics](https://www.man7.org/linux/man-pages/man2/rename.2.html)
- [Linux fsync semantics](https://www.man7.org/linux/man-pages/man2/fsync.2.html)

This is a new BSD-3-Clause implementation. See NOTICE and the repository LICENSE.
