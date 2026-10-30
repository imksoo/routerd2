# Bounded local management APIs

The control worker serves HTTP/JSON over its private Unix socket. The network
worker has a separate socket and owns native kernel effects. The general local
API request/response limit remains **2 MiB**. A successful mutation must remain
acknowledgeable even when its full observation is larger than that limit.

## Configuration and health

| Endpoint | Response |
| --- | --- |
| `GET /v1/config` | The existing complete `State` for small configurations. An oversized state returns **413**, with the metadata/document alternatives named in the error. |
| `GET /v1/config/metadata` | Revision, request identity, health, confirmation metadata, document digest/size and explicit omission flags. |
| `GET /v1/config/document?revision=N` | Exactly one compact current `config.Document`. `N` is mandatory and positive. A different current revision returns **409**. |
| `POST /v1/config/commit` | Existing complete `State` when it fits; otherwise a successful **200** metadata acknowledgement for the committed candidate. |
| `POST /v1/config/confirm` | The same bounded success rule. The returned current confirmation is absent after success. |
| `GET /v1/health` | Lightweight revision/health/epoch/sequence, confirmation identity and service counts. Healthy is **200**; unhealthy is **503**. |

Metadata uses `config_omitted`, `confirmation_previous_omitted` and
`services_omitted`. Missing document bodies must never be interpreted as an
empty candidate or an empty rollback configuration. `services` in metadata is
a health summary; the service endpoints remain the source for instance detail.
Long diagnostic text has a `last_error_truncated` flag.

`config_digest` is the hexadecimal SHA-256 of `json.Marshal(config.Document)`;
`config_bytes` is the exact length of that compact JSON. This digest is distinct
from `last_request_hash`, which also covers commit options. A reader obtains
metadata, fetches its revision-fenced document, and checks the size and digest.
It retries a revision conflict a bounded number of times instead of mixing
metadata and a different candidate. A successful large commit acknowledgement
can be checked against the caller's candidate without fetching the document
again. The CLI implements these checks and preserves omission information for
rollback/service detail it has not fetched.

Health does not construct the complete control State, encode protocol snapshots,
or copy route issue details. Its field names remain compatible with the boot
watcher: `revision`, `healthy`, `last_error`, `producer_epoch`, `sequence`,
`confirmation`, and `services.{revision,phase,route_issue_count}`. Service health
also carries `instance_count`. The complete record's two documents and service
observations therefore cannot overflow a health response.

## Network summaries

`GET /v1/services/resources` exposes the coordinator's selected resource
projection in ID order, 128 resources per page. Each item has `id` and
`state.{active,ready,when,reason,dependency}`; optional reason/dependency explain
false or indeterminate guards, readiness and prerequisites. Truth values are
0 (indeterminate), 1 (false), 2 (true). These are policy conditions, not an
independent kernel acknowledgement. The page includes runtime `phase`, bounded
`last_error` with `last_error_truncated`, operator `revision`,
`projection_generation`, `total`, `offset`, `observation_id` and optional
`next_offset`. Empty/static programs are supported.

Continuations require `offset=N&observation_id=DIGEST`. The digest covers the
whole sorted state and phase, including readiness changes that leave the
network generation unchanged. A changed observation returns 409; invalid,
unknown or duplicate query parameters return 400. No source payloads,
provenance or protocol snapshots are exposed. `show resources` assembles the
pages before writing output and rejects mixed observations.

The network worker's `GET /v1/snapshot` and control's `GET /v1/network` return
`network.ObservationView`. Small complete observations retain the existing
Snapshot fields. Every response additionally supplies:

- `observed_at`: the observation completion time;
- `projection_id`: a digest of committed/attempted intent, revisions and apply
  status;
- `observation_complete`: false if a kernel read failed or any section is a
  bounded preview;
- `counts`: complete counts for the successfully observed, scoped collections;
- `truncated` and `truncation`: explicit indications of omitted or previewed
  desired state, interfaces, addresses, routes, next hops, policy rules,
  WireGuard devices/peers/allowed IPs, or diagnostics.

Encoded network observations are capped at **256 KiB**, including JSON escaping.
Large summaries show previews and exact observed counts; they do not claim
their collections are complete. Observation errors describe failed reads, so a
count from an incomplete observation does not prove the kernel has no further
entries. Legacy internal `NetworkClient.Snapshot` rejects an incomplete view
instead of silently returning its preview as a complete snapshot.

Interface and default route-table scope follows the current/attempted desired
state and ownership journal. An explicit route table or address interface detail
query can inspect a different scope. Observation never adopts or mutates it.
Route ownership candidates are indexed by normalized destination, table and
metric, then checked with the same exact ownership predicate used by the
effector. This avoids scanning the entire managed FIB for every observed route.

## Network detail pages

The control endpoint is `GET /v1/network/{section}`. Its network-worker equivalent
is `GET /v1/observations/{section}`.

| Section | Required filters | Optional section filters | Item details |
| --- | --- | --- | --- |
| `interfaces` | — | — | Interface state with `address_count`, an address preview and `addresses_truncated`. |
| `addresses` | `interface` | — | Complete individual address states, ownership, flags and lifetimes. |
| `routes` | — | `table` | Route state with stable `route_id`, `next_hop_count`, a next-hop preview and `next_hops_truncated`. |
| `route-next-hops` | `route_id` | `table` | Individual next hops of the identified route. |
| `policy-rules` | — | — | Individual policy rule states and ownership. |
| `wireguard` | — | — | Device state and `peer_count`; nonempty peer collections are explicitly omitted with `peers_truncated`. |
| `wireguard-peers` | `interface` | — | Public peer state with `allowed_ip_count`, an allowed-IP preview and `allowed_ips_truncated`. |
| `wireguard-allowed-ips` | `interface`, `public_key` | — | Individual prefixes for one public peer key. |

Every section accepts `offset` (default 0), `limit` (default 64, maximum 256),
and optional `projection_id`. `table=0` or no table selects the referenced table
scope of the summary. Each page contains `section`, `total`, `offset`, `items`,
optional `next_offset`, and the revision/status/observation header. Clients use
the returned next offset; the byte cap can produce fewer items than the requested
limit. One unrepresentable individual item returns **413** instead of silently
dropping it. Unknown, duplicate, empty or unrelated query parameters return
**400**. A supplied projection ID mismatch returns **409**; a missing requested
peer/route returns **404**; a kernel observation failure returns **503**.

The full desired projection uses `GET /v1/network/desired` (worker:
`GET /v1/observations/desired`). Here `offset` and `limit` are byte counts, with a
default/maximum chunk size of **64 KiB**. A `DesiredPage` contains `total_bytes`,
`offset`, optional `next_offset`, and `data_base64`, plus the same observation
header. Reassembly is bounded at **2 MiB** and must keep the first projection ID
for later requests. Decode the complete reassembled JSON only after validating
all offsets, lengths and the final total.

Each detail request performs a fresh kernel observation. The projection ID
fences configuration and pending intent changes; it does **not** freeze DAD,
address/route expiry, WireGuard roaming, handshakes, counters or other kernel
changes. Page totals and ordering can therefore change even with the same ID.
The API provides no atomic all-page snapshot guarantee. Route IDs exclude the
remaining expiration time and ownership bit; they identify routing entries,
not authorization or immutable kernel state.

## Minimal protocol dependencies

AFTR obtains only its source dependency through
`GET /v1/source?interface=NAME&address=NUMERIC_ADDRESS` on the network worker.
`NAME` is the address owner: the WAN for RA/IA_NA, or the selected LAN delegation
for a PD-derived source. DNS and tunnel transport remain on the configured WAN.
`NetworkClient.SourceAddress` forwards that native query; there is no full
Snapshot fallback. The result identifies the requested interface/address,
interface presence/index/up state, and at most one matching address state.

The worker reads that interface's native address state and checks ownership only
for matching source candidates. It does not read unrelated routes, policy rules,
WireGuard devices, firewall state, or the full desired projection. This response
intentionally has no projection digest: AFTR already uses its own configuration
digest and dependency generation. Source use requires an exact matching,
present, UP interface and owned address with positive preferred/valid lifetimes,
without tentative, DAD-failed or deprecated flags. A failed source query
withdraws the dependency and fences stale AFTR results; unrelated large operator
observations cannot withdraw it through a transport-size failure.

All these API changes are pure observation or response shaping. They do not
raise the global message limit, modify foreign state, change WireGuard key
ownership, or introduce shell commands.

## Durable configuration observations

Successful commit, confirmation and confirmed rollback save a stable random
event ID, operation, revision and observation time in `config.json` together
with the authoritative configuration. The pending candidate is never a source
of observations. Rejected candidates and periodic reconciliation remain
transient hints on the existing `/v1/events` endpoint.

The control worker delivers this producer outbox to the private `events`
SQLite WAL/FULL journal. It performs journal I/O outside the configuration lock,
then atomically removes only the acknowledged prefix from the current record.
A crash or failed acknowledgement save retries identical event IDs. Concurrent
commits carry the pending prefix forward. No configuration body, caller request
text or credential is included in an observation.

At most 64 observations may remain pending. A confirmed trial reserves its
confirmation/rollback slot before any effects. Capacity exhaustion rejects new
commits with 503 before staging or kernel changes. `health.event_journal`
reports pending count and delivery error; delivery errors make public health
unhealthy without discarding authoritative configuration or pending history.

`GET /v1/event-store` performs fresh read-only capacity inspection (no query
parameters); `GET /v1/health` returns `event_journal.storage`, a bounded cache
updated about once per second. Warning pressure remains healthy; exhaustion or query
failure sets `State.event_store_error` and prevents trial confirmation. Confirm
refreshes storage before taking the configuration lock, so it does not trust an
old healthy cache. Expired trial rollback still restores its authoritative
source and retains an undelivered observation in the reserved file outbox.
Health polling does not query SQLite. The background capacity query and fresh
inspection do not hold the configuration lock across storage I/O.
The same background monitor reclaims a maximum of 64 completed job payloads per
pass into bounded durable SHA256 identities, sharing consumer and producer work.
`compaction_error` persists across successful read-only inspections until a
maintenance pass succeeds. It degrades health and blocks confirmation without
changing source/cursors or dropping pending work. Completed-job reclamation recovers hot job capacity
and reusable SQLite pages.
It also moves up to sixteen hot event bodies per pass into bounded gzip storage
inside the same WAL/FULL database transaction. Logical IDs/cursors/history and
pending/timer references remain readable. `archive_error` survives successful
read-only inspection until a successful maintenance pass; errors degrade health
and prevent confirmation. `events`, `archived_events` and `history` report hot,
compressed and complete logical budgets. History export uses the complete budget.
See [capacity and immutable export](../../docs/event-storage.md); these do not
reclaim journal space or guarantee an externally durable alarm.

`GET /v1/observations?after_cursor=N&limit=L` reads persistent, append-only
history independently of the control worker epoch. `after_cursor` is required;
limit defaults to 16 and must be 1..16. Duplicate/unknown fields return 400,
a cursor beyond the tip returns 409, and storage errors return 503. The page
contains records, next_cursor and more. Each event is at most 64 KiB, keeping
responses below the API byte limit. This is read-only history, not an effect
execution or consumer registration API. Journal retention/reclamation and externally durable alarms,
other raw protocol event producers and federation remain open work. HealthCheck
probe events use the narrow durable relay described below. DerivedEvent
assertion transitions are also connected through their producer state/outbox.

## Historical protocol input

`POST /v1/protocol-events` accepts up to 16 stable-ID HealthCheck probe events
for a durably registered queue, using `{resource, events}`. Historical delivery
is independent of current worker generation, readiness and current observations.
Only that HealthCheck's passed/failed/timeout topics and typed resource detail
are accepted. Validation covers the entire page before any append; success
returns `accepted_ids` in exactly the page order. A partially stored page is
retried whole and deduplicates by ID. This is not a generic public append API.

The worker saves pending observations before sending current facts and retains
their original time/content/producer metadata across restart and source changes.
History can predate current authority, but delivery cannot register a producer,
renew facts or execute effects. `health.protocol_events.last_error` and
`State.protocol_event_error` report bounded per-producer storage failures.
Failures make health unhealthy and prevent trial confirmation; another
producer's success cannot clear them. Authoritative config remains committed.
Before worker creation, control initializes and durably registers that health
queue's generated ownership. Registration failure rejects the candidate before
starting its worker. Failed candidate application keeps its registered queues
visible against the restored source.

Permanent removal starts control-owned retirement recovery. Only registered,
no-longer-declared queues whose health worker is absent from a fresh supervisor
observation are considered. An exclusive store lock is also mandatory. Recovery
retries original historical IDs, commits the local ACK, and removes registration
only after the queue is empty. Missing/foreign stores are errors; there is no
filesystem discovery/deletion or current-fact restoration.
`health.protocol_events.{registered_relays,retiring_relays}` reports bounded
counts. Pending retirement and failed recovery make health unhealthy and block
confirmation until local and registry ACKs complete. Commit can succeed while
reporting this unhealthy status. The loop rotates eight queues per pass and
processes one bounded publication page per queue.
Still-declared conditional-inactive health queues are recovered under the
runtime's module reconciliation guard, without holding its current-fact mutex.
A selected health binding or a remaining health process blocks recovery.
If every candidate is selected, explicit denials require no runtime operation
guard or supervisor IO, avoiding competition with normal freshness renewal.
An empty declared queue keeps registration for the next activation.
`stopped_pending_relays` counts discovered stopped queues still awaiting durable
delivery; they remain unhealthy between pages. Only an exclusively emptied
queue clears that worker's prior delivery alarm. No other producer's alarm is
cleared. Source/revision and unknown Derived inputs remain unchanged. The ownership
wait and supervisor query release control source authority, so current history
publication is not queued behind those waits. Recovery only tries the source
lock while owning runtime reconciliation; a busy source defers that pass to
avoid lock inversion. It reloads the registry/source under both guards, and an
unobserved name cannot clear pending work.
See [relay durability and limits](../protocolevents/README.md).

## Current DerivedEvent observations

`GET /v1/derived/{id}` reads one declared resource's finite current observation.
Resource IDs containing `/` are supported. The response includes ID, lifetime,
checkpoint version, state, original `observed_nanos`, `valid_until_nanos` and
publication pending status. `show derived RESOURCE_ID` uses this endpoint in
operational and candidate CLI sessions; it never reads historical assertions as
a fallback. Missing resources return 404; persistence/evaluation errors return
503 without a partial current view.

The control worker accepts typed `derived_event` specs. Initially supported
references are HealthCheck scalar fields, keyed by ResourceProgram resource ID,
not service name. Missing/inactive/stale providers are unknown. Own `when` and
dependency conditions fence activation immediately; old source/worker/control
authority cannot grant current asserted status. Views also require a live
evaluator lease of at most five seconds and cannot renew it themselves.

Producer state and output jobs commit atomically before historical publication.
Stable IDs make retries after failed completion ACKs idempotent in this journal.
Unchanged unconditional definitions survive control restart without re-emitting
their initial value; fresh current input is still mandatory. Definition/removal/
activation changes fence older jobs. Evaluation samples current predicates;
accepted between-poll transitions are tracked for hold continuity, not replayed
as a lossless protocol event stream.

`health.derived` contains revision, active count, pending flag and bounded error.
Errors also set `State.derived_error`, make health unhealthy and prevent trial
confirmation. Committed source remains authoritative when post-save registry
work fails. An unused DerivedEvent component performs no registry writes.
Other providers, chaining, retention, effects and federation remain open work.

## Durable EventRule resources

The control worker accepts ResourceProgram `event_rule` payloads using the
pinned typed pattern/emit spec. A new activation registers at the current
journal tip; historical input is not replayed into a newly created rule.
State, exact next input cursor and output jobs commit atomically. Timer updates
also require the current checkpoint version. Output publication uses stable
identities in the observation journal, so retrying after a failed completion
ACK cannot insert a duplicate record. A completed job retains its tombstone.
This is historical publication, not an external-effect exactly-once promise.

Committed source changes and rule processing share the configuration authority
lock. The lifecycle registry persists each activation before processing.
Definition changes, removal/re-addition and conditional activation gaps obtain
new consumers and cannot publish old timers/jobs. Conditions are evaluated from
fresh finite snapshots; the runtime observes accepted flaps between controller
polls and fences producer replacement/lease gaps. Conditional lifetimes reset
after a new control incarnation. Unchanged unconditional definitions retain
their aggregate across restart and unrelated source revisions, including while
the logging worker is unavailable. A revision gap in the lifecycle registry
conservatively starts new lifetimes because an inactive interval may be missing.

The delivery loop runs every 200 ms. Each pass visits at most sixteen rules,
rotating the start, with four input records and eight output publications per
visited rule, and at most 64 due timer emissions per transaction. Original
observation deadlines order timers before later queued inputs; an input at the
deadline wins. These bounds are not throughput or latency qualification.

`GET /v1/health` includes `event_rules.{revision,rules,last_error}`. Each bounded
rule entry exposes id, lifetime, cursor, version and a `pending` boolean for
queued output work (not future timers). Errors make public health unhealthy;
the authoritative configuration remains saved and processing retries. A
post-commit lifecycle save failure must not report that the committed source
was rolled back. At most 128 live rule declarations are accepted before any
effects; durable registrations/tombstones still share store capacity limits.
Retention/archive/capacity operations remain required for long-running use.

## Restoring an explicitly saved configuration

`POST /v1/config/restore` accepts a `RestoreRequest` containing the immutable
`snapshot` reference, `expected_revision`, `request_id` and optional
`confirm_timeout_seconds`. `Store.ResolveSnapshot` verifies the saved content
and its store identity. `Engine.Restore` then passes only its configuration to
`Engine.Commit`; it creates a new current revision instead of replaying the old
revision, runtime observations, release selection or journal. Resolution does
not hold the writer lock. The normal commit path still owns revision conflicts,
idempotent replay, release preparation fencing, network application, persistence
and failure recovery. A confirmed restore uses the ordinary confirmation and
automatic rollback path. The HTTP acknowledgment has the same bounded complete
or explicit-omission form as a normal commit.

This follows the old routerctl rollback behavior of reading saved intent and
submitting it through the existing apply owner. It restores one verified local
configuration snapshot; it does not restore an entire appliance or import a
snapshot from another configuration store.
