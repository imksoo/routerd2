# Derived assertion evaluator

This package is the pure evaluator used by the live control-worker
`derivedcontroller`. ResourceProgram accepts typed `derived_event` payloads.
The runtime supplies finite current HealthCheck and BFD observations; the
controller atomically saves producer state/output jobs and publishes stable
historical events to the journal. Neither package executes external effects or
grants kernel authority.

`Compile` owns a typed copy of the pinned routerd DerivedEvent spec.
`NewSnapshot` copies a bounded set of current scalar observations. `Advance`
returns proposed serialized state, historical emissions and current status.
State and output jobs must be committed atomically with a version fence; an
uncommitted result must not be published. Output IDs bind the program digest,
evaluation scope, triggering input ID, transition counter and asserted value.
Retrying the same frozen input against the same state produces identical IDs.

Status references require a known value from the current scope and producer,
an original observation time no later than the current BOOTTIME, and a finite
expiry strictly after that time and the current BOOTTIME. Missing, expired,
future, unknown and old-scope values are indeterminate. Zero expiry is unknown
here; it is not the event journal's non-expiring historical-delivery convention.
Persisted asserted=true cannot replace these checks. A replacement producer or
control/source scope cannot inherit the previous assertion's retraction grace;
current status omits its boolean until fresh input establishes it. Indeterminate input
cancels a pending hold, exposes no asserted boolean and emits no retraction.
Historical last-transition timestamps remain historical.

Scopes and producer origins must come from the accepted current-producer
registry. Caller-supplied strings are not authentication. The scope must bind
the boot, source/revision and evaluator incarnation. The caller must serialize
every accepted producer transition with timer evaluation, including flaps that
occur between projections. It must reset the evaluator incarnation after a
missed interval and fence producer authority when committing/publishing output.
This component cannot detect an omitted, unexpired producer transition.

Input groups use three-valued conjunction/disjunction, preserving a known
false conjunction or true disjunction despite unrelated unknown values.
Literal fields and known immediate predicates follow the pinned reference at
`1329ef54f296507eda79bbbc3b853783dd27251e`. Direct comparison covers both
emit_when policies, both retract_when values and emit_initial. The pinned
engine ignores retract_when and retracts on target negation; this component
preserves that behavior rather than introducing a latch policy.
Unlike the legacy empty-string fallback, missing status is explicitly unknown.

Hysteresis holds both transition directions using BOOTTIME. Same-value
renewals preserve the due time. Value reversal, unknown input, lease gaps,
producer replacement and scope changes cancel or restart the pending hold.
The live runtime may attach `WithContinuity` proof after observing every accepted
input transition. It bridges coalesced same-predicate renewals, even when the
previous sampled lease has elapsed, without extending the current input lease.
Without this trusted proof, a lease gap always resets the hold. Predicate truth,
rather than irrelevant scalar text changes, defines this continuity.
Providers can attach a separate `EqualityContinuity` token when nonempty
values change independently of presence. The compiler selects it for a status
reference used by an `equals` predicate, including predicates inside `any_of`.
BFD counts every produced `observedAt` change before mailbox coalescing, so
an unseen A→B→A or a move between two literal witnesses restarts the hold.
Presence-only references retain their normal continuity across periodic
timestamp changes. If a reference is used for both presence and equality,
the stricter equality proof is selected conservatively; provider changes can
restart a hold even while another branch keeps the aggregate truth unchanged.
These tokens never replace producer authority or renew observation deadlines.

Wall-clock jumps cannot complete a hold; its event timestamp is fixed at the
original observation wall time plus the duration. No goroutines or volatile
wall timers run in this package. Restored holds still require a current
snapshot; same-scope backwards BOOTTIME is rejected.

`ObservedNanos` preserves input evidence age across evaluator polls. For an OR
witness, its freshness and expiry come from the same witness, even when a
different witness has a longer lease. `ValidUntilNanos` also bounds current
status at a pending transition deadline. A caller must bound the returned
status further by its own liveness and current authority, especially for a
literal-only program, whose logical proof has no producer expiry. These
metadata fields are not persisted authority or transport credentials.

Limits are 64 top-level inputs, 64 groups/terms per input, 1,024 predicates,
1 MiB compiled source, 1,024 snapshot values and 2 MiB snapshot text. Individual
fields/values are bounded; serialized state is at most 64 KiB. Limits fail
without partial state or emissions. Large-scale performance is not qualified.

Tests cover known assertion/retraction, initial policy, immutable snapshots,
stable retry identity, finite freshness, old scopes, source replacement,
unknown bootstrap, expiry gaps, hold boundaries, wall jumps and coherent
witness age/expiry. An actual SQLite consumer fixture restores a pending
hold, fails its timer checkpoint after outbox insertion, retries atomically,
rejects a stale timer claim, reopens the historical emission and verifies that
the restored asserted value is unknown when its current input has expired.

The original SQLite consumer fixture already has a real input cursor. A second
fixture uses `events.ProducerCheckpoint` / `ProducerTransition` to commit the
initial output in an empty journal and reopen it without inventing an input
cursor or treating its historical output as current authority. These producer
APIs now back the live `derivedcontroller`. Evaluation samples current status;
it is not a lossless journal of every short protocol transition. Continuity
tracking prevents a missed flap from falsely completing a hold. The scheduler
processes bounded batches every 200 ms. Read-only current views require matching
input/producer authority and a five-second evaluator lease, further bounded by
input expiry and the resource's own active condition. GET cannot renew this lease.

`show derived RESOURCE_ID` reads `/v1/derived/{id}`. Inactive/unknown status and
storage failure never fall back to an old asserted value. Health exposes bounded
revision/count/pending/error metadata. Remaining work includes other status
provider kinds, structured fields and DerivedEvent chaining, raw protocol event
producers beyond HealthCheck, effect idempotency/authorization, federation, retention and physical/
performance acceptance. CORE-04 remains in progress. See
[live evidence](../../docs/evidence/2026-10-24-live-derived.md).
