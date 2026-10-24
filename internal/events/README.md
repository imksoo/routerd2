# Durable observations, cursors and consumer work

The control worker uses this storage component for durable configuration
commit/confirm/rollback observations through its authoritative producer outbox.
It is not connected to EventRule/DerivedEvent resources, federation, or a
privileged effect executor yet.
It does not publish protocol health or restore permission to change the kernel.

`Open` owns a private Linux directory through one exclusive process lock and a
SQLite WAL database. The existing pinned pure Go SQLite driver supports static
amd64/arm64 builds. FULL synchronous mode and foreign keys apply to every
connection, including replacement connections. An existing database is inspected
read-only before any write or recovery is allowed. Symlinks, hard links, FIFOs,
foreign database identity and directories writable by other users are rejected.

`Append` records a bounded event with a stable ID. An identical retry returns
the existing cursor; changed content under the same ID fails. Observed time is
explicit, and zero event expiry remains non-expiring. Events are historical
evidence; they are never finite protocol authority leases.

`Cursor` initializes a new consumer at the current journal tip. Existing
consumers resume their saved position. `Drain` calls the handler before saving
the cursor. A failed cursor write repeats the event, so handlers must use the
stable event ID for durable idempotency at the target of an external effect.

`Schedule` atomically advances exactly one next cursor and inserts its outbox
jobs. It cannot skip history or rewind a cursor. `Pending` returns bounded work
pages; `Complete` is idempotent. Completed work remains a tombstone, so restoring
only an older cursor cannot reactivate it. A crash after an external effect but
before `Complete` can repeat that effect: the consumer/key pair must also fence
duplicates in the target. No exactly-once execution claim is made.

`Checkpoint` registers a stateful consumer at the current tip and reads its
cursor, version and JSON state consistently. `Transition` commits a next input
cursor, new state and work together. It may keep the cursor unchanged for a
timer, but still requires the current version. A competing or stale transition
cannot advance state or produce work. A stateful consumer cannot switch to
`Drain`, `Cursor` or `Schedule` and bypass this transaction. Timer deadlines,
rule semantics and authority are the caller's responsibility; a checkpoint is
not a protocol lease. State is at most 64 KiB per consumer and 64 MiB in total
by default; replacing state releases its previous byte charge.
The persisted state is bound to its processed cursor. Restoring only an older
cursor while retaining newer state is rejected instead of counting history twice.
A complete database rollback still requires target-side effect deduplication.

`ProducerCheckpoint` / `ProducerTransition` provide separate versioned producer
state and output jobs without requiring a historical input cursor. This permits
an initial DerivedEvent observation in an empty journal without manufacturing
an input event. Producer and consumer names occupy separate API namespaces;
`ProducerPending` / `ProducerComplete` access only producer work. Jobs are
ordered by their first committed state version and key. An identical stable
key/payload retains its original version and completion tombstone; changed
content under an existing key is rejected. A stale state version cannot commit.
These APIs do not append history, publish outputs or authenticate live sources.
The caller must fence current source authority and use target-side durable
idempotency when publishing or executing output. Configuration's existing
authoritative-file outbox remains separate because it must be atomic with the
configuration file itself.

Defaults cap the journal and outbox at 100,000 rows and 64 MiB of encoded payload
each, plus 1,024 consumer/producer registrations combined. The existing
`MaxConsumers` and `MaxConsumerStateBytes` limits cover both APIs together;
producer jobs also share the outbox row/byte limits. Individual bodies are at most
64 KiB. These are payload/row budgets, not a bound on SQLite's total filesystem
size. Capacity exhaustion refuses growth and preserves unconsumed history and
deduplication tombstones. It still acknowledges existing identical events.
Retention/archiving and capacity alarms outside this filesystem remain required
before this component can serve a long-running production event workload.

Tests use actual SQLite transactions, write-failure triggers, SQLITE_FULL,
concurrent publishers, private path attacks, foreign database preservation,
connection replacement, and SIGKILL followed by WAL recovery. Configuration
producer state/outbox atomicity and worker recovery have separate control tests.
Protocol event producers and QEMU power-loss acceptance remain implementation-plan work.

The database schema is now version 4. Accepted design D-09 deliberately does
not import discarded, unreleased version-1/2/3 prototype state. Unsupported schema
identity is rejected through the read-only probe before SQLite recovery/writes.
This is not a promise that different released versions can share or roll back
this database. A production migration/fallback contract remains DIST-01 work.
