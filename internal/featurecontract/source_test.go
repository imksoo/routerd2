package featurecontract_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/lifecycle"
	"github.com/imksoo/routerd/pkg/resource"
	"github.com/imksoo/routerd/pkg/resourcequery"
	routerstate "github.com/imksoo/routerd/pkg/state"
	"github.com/imksoo/routerd2/internal/featurecontract"
)

// These tests execute the pinned reference's pure query/planner functions.
// They do not load its parser, start its controller, or invoke an effector.
func TestSourceConditionEvidence(t *testing.T) {
	for _, tc := range featurecontract.ConditionCases() {
		t.Run(tc.ID, func(t *testing.T) {
			store := sourceStore{observations: tc.Observations}
			condition := sourceCondition(tc.Condition)
			got := featurecontract.False
			if resourcequery.ResourceWhenMatches(condition, store) {
				got = featurecontract.True
			} else if resourcequery.ResourceWhenIndeterminate(condition, store) {
				got = featurecontract.Unknown
			}
			if got != tc.SourceTruth {
				t.Fatalf("source condition = %s, want %s; native requirement: %s", got, tc.SourceTruth, tc.NativeRequirement)
			}

			// The generic and acquisition filters intentionally disagree for
			// indeterminate evidence. This matters for a BACKUP becoming a
			// DHCP/PD client before its HA role has been observed.
			router := &api.Router{Spec: api.RouterSpec{Resources: []api.Resource{{
				TypeMeta: api.TypeMeta{APIVersion: api.NetAPIVersion, Kind: "Interface"},
				Metadata: api.ObjectMeta{Name: "wan"},
				Spec:     api.InterfaceSpec{When: condition},
			}}}}
			kept := len(resourcequery.FilterRouterByWhen(router, store).Spec.Resources) == 1
			if want := tc.SourceTruth != featurecontract.False; kept != want {
				t.Errorf("generic filter kept resource = %t, want %t", kept, want)
			}
			resolved := len(resourcequery.FilterRouterByResolvedWhen(router, store).Spec.Resources) == 1
			if want := tc.SourceTruth == featurecontract.True; resolved != want {
				t.Errorf("acquisition filter kept resource = %t, want %t", resolved, want)
			}
		})
	}
}

func TestSourceDependencyEvidence(t *testing.T) {
	for _, tc := range featurecontract.DependencyCases() {
		t.Run(tc.ID, func(t *testing.T) {
			store := dependencyStore{}
			if tc.Present {
				status := map[string]any{"phase": tc.Phase}
				if tc.ObservedPhase != "" {
					status["observed"] = map[string]any{"phase": tc.ObservedPhase}
				}
				store[tc.Reference] = status
			}
			dependency := api.ResourceDependencySpec{Resource: tc.Reference, Phase: tc.Expected, Optional: tc.Optional}
			if got := resourcequery.DependencyReady(store, dependency); got != tc.SourceReady {
				t.Fatalf("source readiness = %t, want %t; native requirement: %s", got, tc.SourceReady, tc.NativeRequirement)
			}
		})
	}
	store := dependencyStore{
		"Interface/wan": {"phase": "Ready"},
		"Interface/lan": {"phase": "Pending"},
	}
	if resourcequery.DependenciesReady(store, []api.ResourceDependencySpec{
		{Resource: "Interface/wan", Phase: "Ready"},
		{Resource: "Interface/lan", Phase: "Ready"},
	}) {
		t.Fatal("one ready dependency must not hide another pending dependency")
	}
}

func TestSourceOwnershipEvidence(t *testing.T) {
	for _, tc := range featurecontract.OwnershipCases() {
		t.Run(tc.ID, func(t *testing.T) {
			input := lifecycle.GCPlanInput{
				DesiredArtifacts: sourceArtifacts(tc.Desired),
				LedgerArtifacts:  sourceArtifacts(tc.Ledger),
				HostArtifacts:    sourceArtifacts(tc.Host),
				TargetOwnerIDs:   map[string]bool{},
			}
			for _, owner := range tc.Owners {
				input.TargetOwnerIDs[owner] = true
			}
			plan := lifecycle.PlanArtifactOrphans(input)
			if tc.Targeted {
				plan = lifecycle.PlanDeleteTargetGC(input)
			}
			var removed, forgotten []string
			for _, removal := range plan.ArtifactRemovals {
				removed = append(removed, removal.Artifact.Identity())
			}
			for _, artifact := range plan.LedgerForgets {
				forgotten = append(forgotten, artifact.Identity())
			}
			slices.Sort(removed)
			slices.Sort(forgotten)
			want := slices.Clone(tc.Removed)
			slices.Sort(want)
			if !reflect.DeepEqual(removed, want) {
				t.Errorf("source removals = %v, want %v; native requirement: %s", removed, want, tc.NativeRequirement)
			}
			if !reflect.DeepEqual(forgotten, want) {
				t.Errorf("source ledger forgets = %v, want %v", forgotten, want)
			}
		})
	}
}

var fixtureNow = time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)

type sourceStore struct {
	observations map[string]featurecontract.Observation
}

func (s sourceStore) Get(name string) routerstate.Value {
	observation, ok := s.observations[name]
	if !ok {
		return routerstate.Value{Status: routerstate.StatusUnknown, Since: fixtureNow, UpdatedAt: fixtureNow}
	}
	return routerstate.Value{
		Status:    routerstate.StatusSet,
		Value:     observation.Value,
		Since:     fixtureNow.Add(-observation.TransitionAge),
		UpdatedAt: fixtureNow.Add(-observation.PublicationAge),
	}
}

func (s sourceStore) Age(name string) time.Duration { return fixtureNow.Sub(s.Get(name).Since) }
func (s sourceStore) Now() time.Time                { return fixtureNow }

type dependencyStore map[string]map[string]any

func (s dependencyStore) ObjectStatus(_, kind, name string) map[string]any {
	return s[kind+"/"+name]
}

func sourceCondition(input featurecontract.Condition) api.ResourceWhenSpec {
	var out api.ResourceWhenSpec
	if input.Fact != "" {
		match := api.StateMatchSpec{Equals: input.Equals}
		if input.For != 0 {
			match.For = input.For.String()
		}
		if input.MaxAge != 0 {
			match.MaxAge = input.MaxAge.String()
		}
		out.State = map[string]api.StateMatchSpec{input.Fact: match}
	}
	for _, child := range input.All {
		out.All = append(out.All, sourceCondition(child))
	}
	for _, child := range input.Any {
		out.Any = append(out.Any, sourceCondition(child))
	}
	return out
}

func sourceArtifacts(input []featurecontract.Artifact) []resource.Artifact {
	out := make([]resource.Artifact, 0, len(input))
	for _, artifact := range input {
		out = append(out, resource.Artifact{Kind: artifact.Kind, Name: artifact.Name, Owner: artifact.Owner})
	}
	return out
}
