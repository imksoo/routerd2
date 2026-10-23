# Structured log module

This module replaces the dependency on journald for routerd-owned diagnostics.
It has no `journalctl`, syslog daemon, shell, or external collector dependency.
It is deliberately a bounded diagnostic service: log delivery cannot be a
precondition for a network reconciliation or a worker heartbeat.

## Process integration

Run one optional, supervised `logging` worker with:

```go
logging.Run(ctx, logging.Options{
    RuntimeDir: runtimeDir,
    StateDir: stateDir,
    ReleaseID: releaseID,
    CaptureKernel: platform == "appliance",
}, ready)
```

Every other process creates one shared asynchronous handler:

```go
handler, err := logging.NewHandler(logging.HandlerOptions{
    RuntimeDir: runtimeDir,
    Module: moduleName,
    Generation: generation,
    Fallback: os.Stderr,
})
// Handle err before installing the handler.
slog.SetDefault(slog.New(handler))
// On shutdown: handler.Close(ctxWithDeadline).
```

PID defaults to the current process. `ROUTERD_GENERATION`, exported by the
supervisor, identifies a worker incarnation within the release. Handler clones
created by `With` and `WithGroup` share the same producer identity and queue.
The logging worker itself uses a console handler so that it cannot recursively
send its own failures to itself. Early boot messages, raw process stdout/stderr,
and panic stacks still go to the inherited console; they are not parsed by the
supervisor. In particular, process reaping never waits for a logging pipe EOF.

`CaptureKernel` reads `/dev/kmsg` directly only when explicitly enabled. It uses
an independent, nonblocking read cursor and never clears the kernel ring or
writes to it. Boot identity plus kernel sequence suppresses replayed records
after a logging worker restart. Sequence gaps and ring overruns are visible;
unavailable kernel access does not stop module log ingestion. Original kernel
escapes and monotonic microsecond timestamps are preserved. The UTC timestamp
is an estimate using the current wall-clock offset, so sequence numbers remain
the ordering authority. This implements the Linux
[/dev/kmsg ABI](https://www.kernel.org/doc/Documentation/ABI/testing/dev-kmsg).

## Private API

All operations use `<runtime_dir>/logging.sock`, mode `0600`. This API assumes
the supervisor's private runtime directory; it is not an Internet listener or
an authorization boundary between equally privileged workers.

| Operation | Request | Result |
| --- | --- | --- |
| Ingest | `POST /v1/logs`, `{"entries":[...]}` | Accepted and duplicate counts, server cursor, degraded flag |
| Query | `GET /v1/logs?module=network&level=WARN&limit=100` | Latest matching records, cursor, retention/drop/restart gap indicators |
| Continue | `GET /v1/logs?epoch=<epoch>&after=<sequence>&limit=100` | Records after the cursor; `reset` requests a fresh view after restart |
| Oldest retained memory | `GET /v1/logs?tail=false&limit=100` | First page of the current bounded memory window |
| Status | `GET /v1/status` | Queue, memory, disk, corruption, producer loss, and kernel-source counters |

Queries return copies and release the store lock before writing the response.
They have eight concurrent response slots separate from the sixteen ingest
slots, so slow readers do not retain the lock or exhaust producers' budget.
Unknown/duplicate query arguments, unknown JSON fields, oversized bodies, and
invalid producer identities are rejected. A page contains at most 200 records.

## Retention and durability

Defaults are a 256-entry producer queue, a 256-entry disk queue, a 1,024-record
memory window, four files of at most 4 MiB each, and a one-second sync interval.
Records are limited to 7 KiB before server metadata and 8 KiB on disk; ingest
batches contain at most 32 entries. Limits are explicit and validated.

Files live in `<state_dir>/logs/log.<index>.ndjson`, where zero is the current
file. Files are mode `0600`, their directory is `0700`, and a nonblocking writer
lock prevents concurrent owners. Rotation renames retained generations and
syncs the directory. Startup replays only the newest bounded memory tail,
ignores malformed complete records with corruption counters, and truncates an
incomplete final record only in the current file.

**An ingest acknowledgment means accepted in memory, not fsynced.** Abrupt power
loss or SIGKILL can lose queued or unsynced diagnostics. Log storage is not a
transaction journal or a security audit acknowledgment. Committed router
configuration has a separate durable store and is not persisted through this
logging path.

On a runtime disk failure, ingestion and queries continue in bounded memory,
loss counters increase, and reopening is retried. An unreadable startup history
requires a worker restart after repair because its persisted sequence is not
known. Historical degradation and drop counters remain visible for the worker
lifetime. Producer failures fall back to the console on the delivery goroutine;
an unavailable or blocked console cannot block `Handle`. Shutdown waits only
until its caller-supplied deadline.

The query API searches the bounded memory window, including its replayed tail,
not every retained disk file. Operators can retain the NDJSON files externally;
archive search/export, remote forwarding, OTLP, and legacy traffic/DNS/flow log
queries are separate features and are not implemented by this base module.
State survives a reboot only when `StateDir` is on persistent storage.

## Sensitive content

Common credential, private-key, configuration and command-argument attribute
names are redacted. Only bounded scalar attributes are accepted; config
structures, byte slices, custom JSON marshalers, and `slog.LogValuer` are not
expanded. This is defensive filtering, not proof that arbitrary message text
contains no secrets. Callers must log operation IDs and outcomes, never raw
configuration or credential-bearing command lines. Kernel messages retain the
kernel's content and are exposed only through the same private API.

## Validation

The tests exercise Unix-socket delivery, duplicate detection, independent
reader/ingest budgets, worker outage and restart, real SIGKILL recovery,
rotation and retention, interrupted writes, corruption, writer locking,
disk failures, blocked console/disk writers, bounded close, redaction,
concurrent producers, and visible sequence gaps. Kernel tests use injected
records and errors; they do not read the host's kernel ring.
