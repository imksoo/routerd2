# Durable observations, cursors and consumer work

The control worker uses this storage component for durable configuration
commit/confirm/rollback observations through its authoritative producer outbox.
EventRule resources also consume this journal and publish historical results
through their atomic state/cursor/outbox. DerivedEvent resources publish sampled
assertion transitions through their atomic producer state/outbox. Federation
and a privileged effect executor are not connected yet. Raw HealthCheck probe
events arrive through the source-fenced private relay; other raw protocol event
producers remain open work. Historical output never restores kernel authority.

`Open` owns a private Linux directory through one exclusive process lock and a
SQLite WAL database. The existing pinned pure Go SQLite driver supports static
amd64/arm64 builds. FULL synchronous mode and foreign keys apply to every
connection, including replacement connections. An existing database is inspected
read-only before any write or recovery is allowed. Symlinks, hard links, FIFOs,
foreign database identity and directories writable by other users are rejected.
`OpenExisting` additionally refuses to create a missing directory/database;
retirement recovery cannot disguise lost state as an empty store. `ErrOwned`
distinguishes a live exclusive owner from corruption/path errors.

`InspectFormat(ctx)` queries the already-owned live connection for application
ID, schema version, journal mode and the complete non-internal schema catalog in
one SELECT/read snapshot. It checks the database and lock inode identities
before and after reading, so a missing/replaced pathname cannot return cached
success from an old connection. It returns only bounded format metadata and a
catalog SHA-256, not event bodies, producer state or consumer cursors. It does not
register work, checkpoint/compact, migrate, recover, initialize or open a second
writable store. Reading SQLite may still perform its usual WAL shared-memory
bookkeeping; this is not a claim that every filesystem byte remains unchanged.

Operational open authenticates all required table/index definitions through its
read-only probe before opening SQLite read/write, in addition to header checks.
It retains its existing behavior with ancillary objects used to inject durable
write failures in recovery tests. Update qualification is stricter:
`InspectFormat` requires the whole catalog to match, and rejects additional
tables/views/triggers as well as altered/missing required objects. This distinction
is not an operator extension/migration feature. Internal `sqlite_` objects are
excluded by a literal prefix, not SQL LIKE's wildcard underscore. The metadata
read is limited to 64 objects, 8 KiB per DDL and 64 KiB total schema text. These
bounds do not qualify all stored rows or guarantee bounded SQLite memory/CPU.
Schema 6's catalog digest is frozen in a separate test to catch DDL changes that
forget to change the format contract and its ABI declaration.

The private control endpoint `GET /v1/event-store/format` performs this fresh
inspection outside the configuration mutex. It accepts no query/path options,
returns 503 for unavailable/closed/incompatible state and never substitutes the
health cache. It identifies the persistent format, not an atomic snapshot of
configuration, events, identity, other workers or protocol authority. The signed
PID 1 preparation fixture combines the current six declared state-family
observations for both candidate and fallback format checks. It still cancels the
pair before boot; operator paired loading/health and other state policies remain
unfinished.

`Append` records a bounded event with a stable ID. An identical retry returns
the existing cursor; changed content under the same ID fails. Observed time is
explicit, and zero event expiry remains non-expiring. Events are historical
evidence; they are never finite protocol authority leases.
Optional bounded `detail` preserves daemon/resource references, severity,
reason and message for historical rule correlation. Its daemon kind must match
the envelope's source. Missing detail keeps configuration-event envelopes
readable; source is adapted as daemon name/kind. There is no public append API,
and this metadata is not producer authentication or a live protocol lease.

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
`LookupProducer` reads an existing registration without allocating one, allowing
an unused optional component to discover retained state without registry writes.

Defaults cap hot event bodies and the outbox at 100,000 rows and 64 MiB of encoded payload
each, plus 1,024 consumer/producer registrations combined. The existing
`MaxConsumers` and `MaxConsumerStateBytes` limits cover both APIs together;
producer jobs also share the outbox row/byte limits. Individual bodies are at most
64 KiB. These are payload/row budgets, not a bound on SQLite's total filesystem
size. Capacity exhaustion refuses growth and preserves unconsumed history and
deduplication tombstones. It still acknowledges existing identical events.
`Capacity` reads transactional usage counters, cursor bounds, file allocation and
filesystem availability without scanning payloads. Logical budget pressure is
visible at 80%/100%; low available filesystem space is also reported. Completed
jobs retain their hot budget charge until `CompactCompleted` moves their
identity to the separately bounded tombstone budget.
`CompactCompleted` atomically replaces only completed consumer/producer work
with kind/name/key, original cursor/state version and normalized payload SHA256.
The hot row/byte charge is released, and freed SQLite pages can be reused.
Pending jobs, state/version/cursors and event bodies remain untouched. Stable
retries consult retained identities; changed content or consumer source cursor
conflicts. Completion remains idempotent. Batches share consumer/producer work;
failed writes preserve the complete original transaction. Default retained
identity limits are one million records/256MiB of encoded identity bytes.
No identities expire, and full identity capacity refuses reclamation rather
than forgetting deduplication. Physical filesystem limits still apply.
`ArchiveEvents` moves at most a bounded batch of hot bodies to gzip storage in
the same WAL/FULL transaction, after independently checking the round trip/SHA outside the writer transaction
and rechecking the original body at commit.
It retains each original event row/ID/cursor, so unconsumed history, pending work
and timers keep valid source references. List and retry reads verify bounded
decompression, complete framing, CRC, original length, SHA and event identity.
Repeated archive reads use a four-MiB-payload/4096-entry LRU cache. Every hit
still compares the current SQL compressed bytes, size and digest. Corruption
invalidates the hit; eviction and Close discard only the decoded cache. No
producer authority, cursor or other durable state is cached here.
Hot payload pages/row budgets can be reused without skipping any observation.
The compressed tier has a separate one-million-row/256MiB encoded budget; all
history retains a one-million-row/1GiB original-byte budget. No IDs expire and
no external durability, database file shrink or unlimited retention is claimed.
Capacity reports `events` (hot), `archived_events` and `history` separately;
first/tip/cursors and export snapshots always describe complete logical history.
`ExportHistory` copies an immutable prefix through a captured tip in bounded
pages without holding a store lock across output I/O. `VerifyHistory` checks
frames, complete event content, ordered cursors, unique IDs, byte/count totals
and SHA-256. This does not delete source rows, advance consumers or renew leases.
See [operator commands and the archive contract](../../docs/event-storage.md).
External durable offload, expiry/lifetime policy, restore/integrity qualification
and capacity alarms outside this filesystem remain required before a
long-running production event workload.

Tests use actual SQLite transactions, write-failure triggers, SQLITE_FULL,
concurrent publishers, private path attacks, foreign database preservation,
connection replacement, and SIGKILL followed by WAL recovery. Configuration
producer state/outbox atomicity and worker recovery have separate control tests.
The live Health relay recovery fixture is in
[health-power-test](../../images/health-power-test/README.md).
Other protocol event producers, exhaustive cut points and physical power-loss
acceptance remain implementation-plan work.

The database schema is now version 6. Accepted design D-09 deliberately does
not import discarded, unreleased version-1/2/3/4/5 prototype state. Unsupported schema
identity is rejected through the read-only probe before SQLite recovery/writes.
This is not a promise that different released versions can share or roll back
this database. A production migration/fallback contract remains DIST-01 work.
