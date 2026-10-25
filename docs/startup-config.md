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

## Remaining signed-runtime boot integration

The one-time adoption path is implemented for boot with a fixed runtime. Using
`startup` together with the signed `release` selector is currently rejected:
preparing and confirming a runtime/configuration pair, reserving compatible
fallback references, fencing changes during a trial and binding the adoption to
the release attempt generation remain pending. Do not interpret fixed-runtime
startup as acceptance of that coupled boot-control transaction.

The capture operation does not select startup by itself; there is no online
snapshot-restore endpoint or SSH `prepare` implementation yet. Selection intent,
boot attempts, confirmation and fallback must be integrated following
[ADR 0001](adr/0001-boot-runtime-startup-config.md). Named stores, reference-aware
pruning, write-time power cuts and actual ENOSPC/fsync-failure acceptance remain.
These prerequisites do not complete CORE-06, DIST-01 or physical deployment
qualification.
