# Persistent configuration and explicit snapshots

The control worker owns the authoritative `StateDir/config.json` journal.
Normal commits durably save the operator's configuration; the initial boot
configuration file is a seed used only when that journal does not exist.
A corrupt journal is an error and is never replaced with the seed.

The unreleased journal now requires a random lower-case 128-bit `store_id`.
It is created with the first committed record and remains unchanged across
commits, confirmation rollback and worker restarts. Records without this
identity are refused; there is no reader or migration for prototype journals.
Health and configuration metadata expose the same identity.

## Capture a confirmed revision

Read the current revision with `routerd config show`, then explicitly capture it:

```sh
routerd config snapshot --expected-revision 12 --runtime-dir /run/routerd
```

This local command calls `POST /v1/config/snapshots` on the private control Unix
socket with `{"expected_revision":12}`. It returns only `store_id`, `revision`
and `digest`; it does not return the configuration body. Revision 12 is an
example, not a default or a request to capture whichever revision is current.
Unknown, duplicate and incorrectly cased request fields are rejected.
The embedded SSH CLI does not yet expose this command.

Capture runs under the same single-writer lock as commit, confirmation and
rollback. The configuration must be reconciled and confirmed. A stale revision
or unconfirmed commit returns HTTP 409. Capture does not apply the kernel,
change workers, advance the configuration revision, select a runtime or reboot.
Ordinary commits already save the running configuration; snapshot capture is a
separate explicit operation.

The payload is canonical JSON containing format version 1, store identity,
revision and operator configuration. SHA-256 covers that complete byte sequence,
including provenance. No lease, learned route, observed health state, event
cursor or private identity file is copied into the snapshot. Credentials that
are part of operator intent remain sensitive in the snapshot: protect these
files and do not put their contents into public evidence.

Snapshots live in `StateDir/snapshots/<digest>.json`, in owned private 0700
directories with owned 0400 regular payloads. Symlinks, multiple hard links,
special files, unexpected modes, invalid identities, differing provenance and
digest mismatches are refused. Internal resolution accepts a typed reference,
not an arbitrary file path, and requires the same authoritative store identity.

A new payload is written to an exclusive temporary file, synchronized and
published with a no-overwrite rename, then the directory is synchronized before
acknowledgment. Retrying the same confirmed revision verifies the existing
payload and synchronizes the directory again. It never replaces or repairs a
corrupt existing object. A post-publication sync error reports uncertain
durability; retry the same revision rather than inferring that no file exists.

Each payload is bounded to 2 MiB and a store to 128 snapshot-directory entries
(at most 256 MiB of complete payloads). Capacity exhaustion returns HTTP 503,
retains all previous references and permits an idempotent retry of the current
already captured revision. Interrupted temporary files consume this entry
budget too; automatic cleanup/pruning is not implemented.

## Adopt a selected snapshot once

For a fixed factory/container runtime, boot configuration may carry an explicit
`startup` selection containing `generation`, `expected_revision`, and the
`snapshot` reference returned by capture. These are references and transaction
fences, not resource declarations or executable paths. A snapshot belongs to
one existing `StateDir` journal identity. An explicit selection cannot create a
new store, read a foreign store, or substitute the seed for missing/corrupt data.

The sole control writer validates and compiles the snapshot before adopting it
as a **new configuration revision**. The original snapshot revision remains
provenance; it never rewinds the public revision counter. Adoption, its selection
generation, source reference, adoption revision and a stable
`configuration.startup_selected` event are stored in the same atomic journal
record. Existing unacknowledged events are retained. Full outbox capacity rejects
adoption before writing; it does not discard history. Control then reconciles
the newly authoritative intent into the kernel and workers before readiness.

A new generation requires the exact `expected_revision`, matching store identity
and no unconfirmed commit. Earlier generations and reuse of a generation with a
different reference or revision fence are rejected. Repeating the exact consumed
selection returns the current journal, including later normal commits, rather
than importing the snapshot again. It synchronizes the journal directory again
before acknowledgment. An unconfirmed later commit is preserved for the normal
control-restart rollback; the startup replay cannot erase that recovery record.
Ordinary writes cannot forget or alter an already consumed generation.

Explicit adoption also requires an owned private 0700 state directory and an
owned 0600, singly linked, bounded regular `config.json`. Journal and snapshot
reads are anchored to the same open directory. Symlinks, shared or foreign
journals and special files are rejected; a FIFO cannot block startup inspection.
This check applies to explicit startup selection, including replay, rather than
claiming that every normal journal read/write already has this protection.

The supervisor validates and copies the selection once, passes its bounded JSON
only to the control worker and clears the reserved selection environment entry
for all other workers. Inherited environment and `Options.Env` cannot inject a
selection. Worker restart reuses that fixed selection. The lightweight
`internal/configref` types/decoder do not depend on the resource compiler, kernel
engines or worker runtime. This does not yet provide a separate small bootstrap.

Publication writes/synchronizes a complete temporary record, atomically renames
it and synchronizes its directory. A write/sync failure leaves startup failed
with uncertain durability. Retry the same selection: the journal will contain
the complete previous or adopted record and a consumed generation is not applied
twice. No compensating write silently rewinds adoption after publication.

## Conditional fallback to a prepared known-good configuration

A recovery selection contains a `recovery` fence with `failed_generation` and
`failed_snapshot`, in addition to its own new generation, baseline
`expected_revision` and known-good `snapshot`. It permits exactly two states:

- Before candidate adoption, the journal is still at the prepared baseline
  revision, has not consumed the failed generation, and its operator document
  exactly matches the verified known-good snapshot.
- After candidate adoption, the journal contains that exact failed generation,
  expected revision and source reference, with no later public revision or
  unconfirmed commit. Its durable adoption receipt authorizes recovery; reading
  the failed candidate snapshot again is unnecessary.

Anything else is a conflict, including a later operator commit. This is not an
open-ended allowed revision range. Both branches adopt the known-good document
as a new public revision. The receipt's `recovered` flag records whether the
failed candidate had actually been consumed; its adoption revision is checked
against that branch. Repeating the exact recovery selection preserves later
commits and its original receipt. Normal Save/Stage cannot change that receipt.
The supervisor deep-copies the nested fence before fixing the control-only env.

The known-good snapshot is always verified. A missing/corrupt failed candidate
payload cannot block post-adoption recovery when its exact durable receipt is
present. Existing identity files, keys, revocation policy, lease authority and
event history are not imported from either snapshot.

## Read-only inspection before preparing an update

The internal `Store.InspectStartup` method runs the same identity, generation,
revision, confirmation, outbox-capacity, snapshot digest/provenance and compiler
checks as adoption, without writing a record, event or selection, creating a
directory, or performing a durability sync. Its result contains the actual
current record, verified snapshot and recovery branch. A consumed selection
returns `AlreadyAdopted` with the current record and no snapshot payload; later
commits and confirmation recovery remain authoritative even if the old input
snapshot is gone.

Inspection is an observation, not a reusable authorization token. The caller
must exclude independent configuration writers while checking both candidate
and fallback inputs and publishing its release decision. Adoption inspects again
before writing. `PrepareChecked` provides the release lock, and release-bound
control writers participate in that lock; the inspection method itself does not
acquire it. These are internal APIs, not a new operator prepare/restore command.

The current compiler validates both source documents. This does not establish
that an incoming executable can compile them, inspect the other persistent state
families, authorize format migration or qualify the target kernel/engines.

## Remaining signed-runtime boot integration

The one-time adoption path is implemented for boot with a fixed runtime. Using
`startup` together with the signed `release` selector is currently rejected:
internal paired prepare/confirm records, reserved fallback references and the
release-bound control writer fence exist, but their operator workflow, coordinated
loader, exact tuple health/confirmation and fallback acceptance remain pending.
Do not interpret fixed-runtime startup or read-only inspection as acceptance of
that coupled boot-control transaction.

The capture operation does not select startup by itself; there is no online
snapshot-restore endpoint or SSH `prepare` implementation yet. Selection intent,
boot attempts, confirmation and fallback must be integrated following
[ADR 0001](adr/0001-boot-runtime-startup-config.md). Named stores, reference-aware
pruning, write-time power cuts and actual ENOSPC/fsync-failure acceptance remain.
These prerequisites do not complete CORE-06, DIST-01 or physical deployment
qualification.
