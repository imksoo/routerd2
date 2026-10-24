# Historical event patterns

This is a pure evaluator. ResourceProgram accepts typed `event_rule` resources;
`internal/eventcontroller` connects them to the control worker's durable journal
and publishes historical outputs. This package itself performs no I/O, executes
no jobs, mutates no kernel state and grants no protocol authority.

`Compile` owns a typed copy of the pinned routerd EventRule spec. `Advance`
evaluates one durable input and returns serialized state plus stable emissions.
`Tick` evaluates due timers without goroutines, using persisted due times. A
caller must atomically commit both with the exact `events.Checkpoint` cursor
and version, including for a timer that does not advance the input cursor.
The evaluator's returned state is a proposal until that commit succeeds.

The implemented operators are all_of, any_of, sequence, window, absence,
throttle, debounce and count. Defaults, inclusive window/interval boundaries,
strict sequence reset, correlation fields, missing correlation policy and
attribute interpolation follow the pinned reference. Immediate outputs are
compared directly with `github.com/imksoo/routerd/pkg/eventrule` at
`1329ef54f296507eda79bbbc3b853783dd27251e`, excluding its processing timestamp
and bus cursor. The old engine's Rate field has no effect; this is not a new
rate limiter implementation. all_of/sequence retain the reference's exact
topic matching; other applicable operators use its `bus.MatchTopic` function.

Native output identities bind the rule digest, correlation, input ID and fire
counter. Immediate observations use the input event time. Absence/debounce
deadlines and output times use observation time plus duration, rather than the
reference's volatile wall-clock timer. The caller serializes input and timer
claims. Source changes reset old state and timers. EventRule-produced inputs
are ignored to prevent the reference's feedback loop. Expiry is a delivery
bound: zero stays non-expiring; equality is still valid. None of this is an
authority check for executing an external action.

Template expansion is one pass. Input text cannot introduce a second template
expression, and each rendered attribute is bounded at 4 KiB before writing.
This removes the reference's Go map-order dependence for nested expressions.

Inputs and individual outputs are at most 32 KiB, state at most 64 KiB, and
each Tick returns at most 64 emissions. Correlation count is capped at the
reference's 4,096 keys with deterministic eviction; the byte budget may be
reached first. Capacity returns no partial state/emissions, so the caller must
retain its input cursor and expose backpressure. Full-size legacy inputs and
4,096-key workloads are not qualified by the small functional fixtures.

Tests cover all eight operators with deterministic clocks, timer restoration,
expiry, source replacement, retry identity, input ownership, output batching,
capacity and the pinned immediate-output comparison. An actual SQLite test
combines the evaluator with the durable consumer store, fails a checkpoint
write after outbox insertion, retries without recounting or changing identity,
and reopens the committed state and work. This is not a privileged effect test.

The live controller registers new lifetimes at the journal tip, serializes
state/cursor/output commits and publishes stable IDs through the same journal.
Resource removal, definition changes and conditional activation gaps fence old
timers/output jobs. Unchanged unconditional definitions preserve aggregation
across control restart and unrelated source revisions. Explicitly conditioned
rules reacquire current observations; unrelated logging readiness is not a
gate for unconditional historical consumers. Timer batches run before queued
observations strictly later than their original deadlines; an observation at
the deadline wins. Control calls the scheduler every 200 ms, with bounded pages
and rotating rule order, rather than a promise of exact delivery latency.

Remaining work includes protocol event producers beyond configuration events,
DerivedEvent live integration, general typed status references, privileged
effect authorization/idempotency, federation, scale qualification and
capacity/retention operations. CORE-04 remains in progress.
