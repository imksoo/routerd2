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

## Remaining boot integration

Snapshot capture and verification are implemented. Selecting a snapshot at boot,
adopting it as a new authoritative journal revision, binding it to a runtime
selection/attempt generation, and retaining a verified runtime/configuration
fallback pair are still pending. Current boot continues to load the committed
journal. A snapshot reference is not yet an accepted boot input.

Implement these operations as one explicit boot-control contract following
[ADR 0001](adr/0001-boot-runtime-startup-config.md); never create an independent
mutable YAML authority or silently overwrite later commits on each restart.
This prerequisite does not complete CORE-06, DIST-01 or physical deployment
qualification.
