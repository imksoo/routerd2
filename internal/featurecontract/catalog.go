// Package featurecontract records selected behavior of the pinned routerd
// functional reference. It contains test inputs, not a runtime configuration
// format or a compatibility layer. Native implementations can reuse the cases
// while deliberately choosing stronger freshness and ownership requirements.
package featurecontract

import "time"

const SourceCommit = "1329ef54f296507eda79bbbc3b853783dd27251e"

// Truth keeps absence of evidence separate from evidence of a false predicate.
type Truth string

const (
	True    Truth = "true"
	False   Truth = "false"
	Unknown Truth = "unknown"
)

// Condition is a small test vocabulary. It intentionally supports only the
// conjunction, disjunction and equality operations needed for the first slice.
// It is never decoded from a router's configuration.
type Condition struct {
	Fact   string
	Equals string
	For    time.Duration
	MaxAge time.Duration
	All    []Condition
	Any    []Condition
}

// Observation separates transition age from publication age. The reference's
// generic when evaluator uses TransitionAge for both for and maxAge; the native
// engine must not infer publication freshness from transition age.
type Observation struct {
	Value          string
	TransitionAge  time.Duration
	PublicationAge time.Duration
}

type ConditionCase struct {
	ID           string
	Condition    Condition
	Observations map[string]Observation
	SourceTruth  Truth
	// NativeRequirement explains which property to retain or strengthen. A
	// source result is evidence, not a requirement to reproduce its weaknesses.
	NativeRequirement string
}

// ConditionCases returns independent fixture values so one test cannot mutate
// observations used by another test. No case reads a clock or touches the host.
func ConditionCases() []ConditionCase {
	role := Condition{Fact: "ha.role", Equals: "master"}
	health := Condition{Fact: "link.health", Equals: "up"}
	values := func(roleValue, healthValue string) map[string]Observation {
		out := map[string]Observation{}
		if roleValue != "" {
			out["ha.role"] = Observation{Value: roleValue, TransitionAge: time.Minute, PublicationAge: time.Second}
		}
		if healthValue != "" {
			out["link.health"] = Observation{Value: healthValue, TransitionAge: time.Minute, PublicationAge: time.Second}
		}
		return out
	}
	timed := func(stable, published time.Duration) map[string]Observation {
		return map[string]Observation{"ha.role": {Value: "master", TransitionAge: stable, PublicationAge: published}}
	}
	return []ConditionCase{
		{ID: "unconditional", SourceTruth: True, NativeRequirement: "A resource without conditions is eligible."},
		{ID: "known-match", Condition: role, Observations: values("master", ""), SourceTruth: True, NativeRequirement: "Fresh known matching evidence permits activation."},
		{ID: "known-mismatch", Condition: role, Observations: values("backup", ""), SourceTruth: False, NativeRequirement: "Known nonmatching evidence is false, not unknown."},
		{ID: "explicit-unknown", Condition: role, Observations: values("unknown", ""), SourceTruth: Unknown, NativeRequirement: "Unknown evidence cannot authorize new acquisition or activation."},
		{ID: "missing-observation", Condition: role, Observations: values("", ""), SourceTruth: False, NativeRequirement: "Strengthen the reference: missing evidence is unknown, distinct from a known mismatch."},
		{ID: "all-false-dominates-unknown", Condition: Condition{All: []Condition{role, health}}, Observations: values("backup", "unknown"), SourceTruth: False, NativeRequirement: "A known false conjunct makes the conjunction false."},
		{ID: "all-true-and-unknown", Condition: Condition{All: []Condition{role, health}}, Observations: values("master", "unknown"), SourceTruth: Unknown, NativeRequirement: "An unresolved conjunct prevents a true result."},
		{ID: "any-true-dominates-unknown", Condition: Condition{Any: []Condition{role, health}}, Observations: values("master", "unknown"), SourceTruth: True, NativeRequirement: "A true disjunct suffices; unrelated unknown evidence does not veto it."},
		{ID: "any-false-and-unknown", Condition: Condition{Any: []Condition{role, health}}, Observations: values("backup", "unknown"), SourceTruth: Unknown, NativeRequirement: "A false disjunct cannot resolve an unknown alternative."},
		{ID: "for-before-boundary", Condition: Condition{Fact: "ha.role", Equals: "master", For: 30 * time.Second}, Observations: timed(30*time.Second-time.Nanosecond, time.Second), SourceTruth: False, NativeRequirement: "Do not activate before the entire stability interval has elapsed."},
		{ID: "for-at-boundary", Condition: Condition{Fact: "ha.role", Equals: "master", For: 30 * time.Second}, Observations: timed(30*time.Second, time.Second), SourceTruth: True, NativeRequirement: "The stability boundary is inclusive and must trigger evaluation without another event."},
		{ID: "max-age-at-boundary", Condition: Condition{Fact: "ha.role", Equals: "master", MaxAge: 30 * time.Second}, Observations: timed(30*time.Second, 30*time.Second), SourceTruth: True, NativeRequirement: "Define the freshness boundary explicitly; the source accepts equality."},
		{ID: "max-age-after-boundary", Condition: Condition{Fact: "ha.role", Equals: "master", MaxAge: 30 * time.Second}, Observations: timed(30*time.Second+time.Nanosecond, 30*time.Second+time.Nanosecond), SourceTruth: False, NativeRequirement: "Expired evidence loses authority even without a new module publication; native status should distinguish it from false."},
		{ID: "old-value-recently-republished", Condition: Condition{Fact: "ha.role", Equals: "master", MaxAge: 30 * time.Second}, Observations: timed(time.Hour, time.Second), SourceTruth: False, NativeRequirement: "Strengthen the reference: maxAge measures freshness, while for measures unchanged evidence duration."},
		{ID: "recent-transition-old-publication", Condition: Condition{Fact: "ha.role", Equals: "master", MaxAge: 30 * time.Second}, Observations: timed(time.Second, time.Hour), SourceTruth: True, NativeRequirement: "Reject inconsistent or stale publication metadata; a recent transition alone is not fresh evidence."},
		{ID: "persisted-match-has-no-generic-boot-guard", Condition: role, Observations: timed(time.Hour, time.Hour), SourceTruth: True, NativeRequirement: "Strengthen the reference: persisted status from a previous boot cannot authorize activation before a live observation."},
	}
}

type DependencyCase struct {
	ID                string
	Reference         string
	Expected          string
	Optional          bool
	Present           bool
	Phase             string
	ObservedPhase     string
	SourceReady       bool
	NativeRequirement string
}

func DependencyCases() []DependencyCase {
	return []DependencyCase{
		{ID: "required-ready", Reference: "Interface/wan", Expected: "Ready", Present: true, Phase: "Ready", SourceReady: true, NativeRequirement: "A successful current-generation dependency enables its dependent."},
		{ID: "required-pending", Reference: "Interface/wan", Expected: "Ready", Present: true, Phase: "Pending", NativeRequirement: "Declared intent or pending application is not readiness."},
		{ID: "required-absent", Reference: "Interface/wan", Expected: "Ready", NativeRequirement: "Missing required readiness does not enable a dependent."},
		{ID: "optional-absent", Reference: "Interface/wan", Expected: "Ready", Optional: true, SourceReady: true, NativeRequirement: "If optional dependencies are offered, absence must have explicit semantics."},
		{ID: "optional-present-wrong-phase", Reference: "Interface/wan", Expected: "Ready", Optional: true, Present: true, Phase: "Failed", NativeRequirement: "Optional does not turn an observed failure into readiness."},
		{ID: "observed-phase-overrides-top-level", Reference: "Interface/wan", Expected: "Ready", Present: true, Phase: "Pending", ObservedPhase: "Ready", SourceReady: true, NativeRequirement: "Native readiness must carry successful apply generation and boot evidence instead of trusting an unqualified retained phase."},
	}
}

// Artifact describes identity and owner only. Source tests translate it to the
// reference's pure GC planner; no teardown executor is constructed.
type Artifact struct {
	Kind  string
	Name  string
	Owner string
}

func (a Artifact) ID() string { return a.Kind + "/" + a.Name }

type OwnershipCase struct {
	ID                string
	Targeted          bool
	Desired           []Artifact
	Ledger            []Artifact
	Host              []Artifact
	Owners            []string
	Removed           []string
	NativeRequirement string
}

func OwnershipCases() []OwnershipCase {
	const ownerA = "net.routerd.net/v1alpha1/VirtualAddress/lan-a"
	const ownerB = "net.routerd.net/v1alpha1/VirtualAddress/lan-b"
	a := Artifact{Kind: "net.ipv6.address", Name: "lan0:2001:db8:1::1/64", Owner: ownerA}
	b := Artifact{Kind: "net.ipv6.address", Name: "lan0:2001:db8:1::2/64", Owner: ownerB}
	unmanaged := Artifact{Kind: "net.ipv6.address", Name: "lan0:2001:db8:1::3/64"}
	return []OwnershipCase{
		{ID: "host-observation-is-not-ownership", Host: []Artifact{unmanaged}, NativeRequirement: "Observed foreign kernel objects must survive reconciliation and teardown."},
		{ID: "desired-owned-object-survives", Desired: []Artifact{a}, Ledger: []Artifact{a}, Host: []Artifact{a}, NativeRequirement: "Do not remove an owned object still desired by the program."},
		{ID: "only-orphaned-owned-object-removed", Desired: []Artifact{b}, Ledger: []Artifact{a, b}, Host: []Artifact{a, b, unmanaged}, Removed: []string{a.ID()}, NativeRequirement: "Remove only previously owned objects which are no longer desired; retain other owners and foreign objects."},
		{ID: "missing-host-object-is-not-deleted", Ledger: []Artifact{a}, NativeRequirement: "A vanished object requires no destructive host operation."},
		{ID: "target-delete-is-exact-owner", Targeted: true, Ledger: []Artifact{a, b}, Host: []Artifact{a, b, unmanaged}, Owners: []string{ownerA}, Removed: []string{a.ID()}, NativeRequirement: "Deleting or gating one resource must not withdraw another owner's address or route."},
		{ID: "empty-delete-target-is-no-op", Targeted: true, Ledger: []Artifact{a, b}, NativeRequirement: "An absent target set must never become a wildcard deletion."},
	}
}
