package resourceprogram

import "testing"

func TestThreeValuedLogic(t *testing.T) {
	values := []Truth{False, True, Indeterminate}
	wantAnd := [][]Truth{{False, False, False}, {False, True, Indeterminate}, {False, Indeterminate, Indeterminate}}
	wantOr := [][]Truth{{False, True, Indeterminate}, {True, True, True}, {Indeterminate, True, Indeterminate}}
	for i, a := range values {
		for j, b := range values {
			if got := All(a, b); got != wantAnd[i][j] {
				t.Fatalf("All(%v,%v)=%v", a, b, got)
			}
			if got := Any(a, b); got != wantOr[i][j] {
				t.Fatalf("Any(%v,%v)=%v", a, b, got)
			}
		}
	}
	if True.Not() != False || False.Not() != True || Indeterminate.Not() != Indeterminate {
		t.Fatal("wrong three-valued negation")
	}
	if All() != Indeterminate || Any() != Indeterminate {
		t.Fatal("empty condition must not grant readiness")
	}
}

func healthFacts(now uint64, fact Fact) FactSnapshot {
	return FactSnapshot{Scope: FactScope{BootID: "boot", ConfigDigest: "config", OperatorRevision: 1}, NowNanos: now, Facts: map[string]Fact{"health/wan/up": fact}}
}

func TestFactUsesOriginalFiniteObservationInterval(t *testing.T) {
	fact := Fact{Value: True, ObservedNanos: 100, ExpiresNanos: 200, WorkerGeneration: 1, ProducerEpoch: "worker"}
	for _, tc := range []struct {
		now  uint64
		want Truth
	}{{99, Indeterminate}, {100, True}, {199, True}, {200, Indeterminate}} {
		if value := healthFacts(tc.now, fact).Value("health/wan/up"); value != tc.want {
			t.Fatal(tc.now, value, tc.want)
		}
	}
	fact.Value = False
	if healthFacts(150, fact).Value("health/wan/up") != False || healthFacts(150, fact).Value("missing") != Indeterminate {
		t.Fatal("observed false and absent observation were conflated")
	}
}
