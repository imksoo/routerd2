# Durable observations, cursors and consumer work

This package is a storage component. It is not connected to the daemon,
EventRule/DerivedEvent resources, federation, or a privileged effect executor yet.
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

Defaults cap the journal and outbox at 100,000 rows and 64 MiB of encoded payload
each, plus 1,024 consumer registrations. Individual event/job bodies are at most
64 KiB. These are payload/row budgets, not a bound on SQLite's total filesystem
size. Capacity exhaustion refuses growth and preserves unconsumed history and
deduplication tombstones. It still acknowledges existing identical events.
Retention/archiving and capacity alarms outside this filesystem remain required
before this component can serve a long-running production event workload.

Tests use actual SQLite transactions, write-failure triggers, SQLITE_FULL,
concurrent publishers, private path attacks, foreign database preservation,
connection replacement, and SIGKILL followed by WAL recovery. Daemon wiring,
producer state/outbox atomicity, worker recovery and QEMU power-loss acceptance
remain separate implementation-plan work.
