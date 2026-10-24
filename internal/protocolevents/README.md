# Durable protocol observation relay

The HealthCheck worker retains every completed probe's typed event from the
pinned routerd evaluator. It saves that observation before publishing the
corresponding finite current snapshot. Shutdown does not invent a probe event.
Other protocol event producers remain implementation-plan work.

Each generated health instance selects a private, per-service `health-events`
directory under the state root. It survives config digest, worker generation
and supervisor changes. The operator cannot supply this path in HealthCheck
intent. The existing SQLite WAL/FULL store supplies exclusive ownership,
private path checks, atomic producer state and version fences. Only the pending
relay queue occupies producer state: at most 64 events and 64 KiB encoded state.
ACKed entries release this local capacity; permanent journal/output-job
tombstones are not deleted or weakened. Target journal IDs retain deduplication.

An accepted queue entry has a random stable ID, original timestamp/content and
historical producer metadata. A failed send, lost ACK, failed local ACK commit,
worker SIGKILL or control restart retries that same entry. A batch contains at
most 16 observations and requires an explicit matching ACK for every ID. ACK
removes only the sent prefix from the latest checkpoint, retaining concurrent
enqueue. The relay tries every 200 ms with a one-second request deadline.

`POST /v1/protocol-events` is a narrow private control API. It requires the
current supervisor/config/worker generation, a previously registered producer
epoch and a fresh supervisor-ready process. It accepts only that HealthCheck's
probe topics and typed daemon/resource detail. It cannot publish configuration
or arbitrary resource events. Whole-page validation precedes journal writes.
Partial storage failure gives no ACK; retrying the page deduplicates its saved
prefix. Source/runtime cutover serializes with delivery. These checks extend
the existing private-worker trust model; they do not claim OS-level isolation
between workers sharing a UID.

History may originate in an earlier boot/configuration and is marked with its
original producer metadata. Replayed history never registers a current producer,
renews fact freshness or grants permission for kernel effects. A removed or
stopped worker cannot send through this API. Late history is not a current Up
observation or authorization to execute a privileged effect.

The control worker registers each health queue in durable producer state before
starting its worker, including candidate workers. Newly registered queues are
initialized before the registry commit; failed registration starts no worker.
Only these registered service names may be recovered. There is no filesystem
scan or adoption of other stores. The registry holds at most 1,024 names.

After permanent source removal, control observes the fresh supervisor state and
requires that no health worker for that service remains. It then acquires the
queue exclusively with `OpenExisting`, retries one page with its original IDs,
and durably acknowledges the queue. Only an empty queue may lose its registry
entry, while its store lock is still held. Missing/foreign state is an error,
never a newly invented empty queue. Files and history are not deleted.
Eight retiring queues are considered per pass, rotating past busy owners.
The store lock also fences a worker that is still exiting or whose role is
being replaced. Recovery does not hold the runtime mutex during disk IO.

Target or local ACK failure retains the original queue. Registry-removal failure
retains discoverability even after local ACK has completed; retry/restart can
remove an empty registration without publishing again. Rejected candidate
application declares its registered queues against the restored authoritative
source, so their pending retirement remains visible. Current workers for still
declared services remain the sole queue owners.

Control reports per-producer storage failures through bounded
`health.protocol_events.last_error` and `State.protocol_event_error`. One
producer's success cannot clear another's failure. Health is unhealthy and
trial confirmation is blocked until recovery. Configuration removal prunes the
active producer error, while retirement remains unhealthy until drainage and
registry ACK complete. `health.protocol_events` adds `registered_relays` and
`retiring_relays`. A successful commit can return healthy=false while its old
history is still recovering; this does not undo the committed source.
Transient unauthorized/not-yet-registered sends do not
create storage-failure alarms. The worker logs changed delivery errors and
recovery independently of the durable transport.

Local write/capacity failure rejects durable acceptance and stops the health
worker before publishing the unpersisted result as a current fact. Previously
accepted entries remain intact. Probe execution and durable acceptance are not
one hardware transaction: a crash before enqueue commits has no recorded probe.
The global journal still has its finite row/byte capacity; retention/archive,
capacity alarms, scale/latency and abrupt QEMU power-loss acceptance remain
required for long-running production use. See the
[initial relay evidence](../../docs/evidence/2026-10-24-health-protocol-events.md) and
[retirement evidence](../../docs/evidence/2026-10-24-health-retirement.md).
