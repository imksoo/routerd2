package resourceprogram

import (
	"strings"
	"sync"
	"testing"
)

var testScope = FactScope{BootID: "boot-a", ConfigDigest: "sha256:abc", OperatorRevision: 7, ProjectionGeneration: 3}
var testEpoch = ProducerEpoch{Epoch: 5, WorkerGeneration: 2}

func newTestStore(t *testing.T) *FactStore {
	t.Helper()
	s, err := NewFactStore(testScope)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateProducer(testScope, "health/wan", testEpoch); err != nil {
		t.Fatal(err)
	}
	return s
}

func observation(sequence, start, end uint64, value bool) ProtocolFact {
	return ProtocolFact{Key: "health/wan/up", Producer: "health/wan", Scope: testScope, Epoch: testEpoch, Sequence: sequence, ObservedNanos: start, ExpiresNanos: end, Value: value}
}

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

func TestFactExpirationAndObservedFalse(t *testing.T) {
	s := newTestStore(t)
	if s.FactValue("health/wan/up", 100) != Indeterminate {
		t.Fatal("absent fact became true or false")
	}
	if err := s.Publish(observation(1, 100, 200, true), 100); err != nil {
		t.Fatal(err)
	}
	if got := s.FactValue("health/wan/up", 199); got != True {
		t.Fatalf("valid health fact = %v", got)
	}
	if got := s.FactValue("health/wan/up", 200); got != Indeterminate {
		t.Fatalf("expired fact = %v", got)
	}
	if err := s.Publish(observation(2, 201, 250, false), 201); err != nil {
		t.Fatal(err)
	}
	if got := s.FactValue("health/wan/up", 220); got != False {
		t.Fatalf("observed down must be false, got %v", got)
	}
}

func TestFactOutOfOrderCannotResurrectUp(t *testing.T) {
	s := newTestStore(t)
	if err := s.Publish(observation(2, 100, 200, false), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(observation(1, 100, 200, true), 101); err == nil {
		t.Fatal("out-of-order Up accepted")
	}
	if err := s.Publish(observation(2, 100, 200, true), 101); err == nil {
		t.Fatal("replayed sequence accepted")
	}
	if s.FactValue("health/wan/up", 150) != False {
		t.Fatal("replay overwrote observed Down")
	}
	if err := s.Publish(observation(3, 102, 202, true), 102); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 150) != True {
		t.Fatal("newer Up was incorrectly dropped")
	}
}

func TestWorkerRestartAndRevokeRejectOldGeneration(t *testing.T) {
	s := newTestStore(t)
	if err := s.Publish(observation(1, 100, 400, true), 100); err != nil {
		t.Fatal(err)
	}
	newEpoch := ProducerEpoch{Epoch: testEpoch.Epoch + 1, WorkerGeneration: 1}
	if err := s.ActivateProducer(testScope, "health/wan", newEpoch); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 101) != Indeterminate {
		t.Fatal("old Up survived worker restart")
	}
	if err := s.Publish(observation(2, 100, 400, true), 101); err == nil {
		t.Fatal("previous producer published after restart")
	}
	if err := s.ActivateProducer(testScope, "health/wan", testEpoch); err == nil {
		t.Fatal("older producer activation accepted")
	}
	fresh := observation(1, 101, 400, false)
	fresh.Epoch = newEpoch
	if err := s.Publish(fresh, 101); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeProducer(testScope, "health/wan", newEpoch); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 102) != Indeterminate {
		t.Fatal("fact survived producer revocation")
	}
	if err := s.ActivateProducer(testScope, "health/wan", newEpoch); err == nil {
		t.Fatal("revoked epoch was reactivated")
	}
	if err := s.Publish(fresh, 102); err == nil {
		t.Fatal("revoked producer published again")
	}
}

func TestProjectionChangeRejectsDelayedNotification(t *testing.T) {
	s := newTestStore(t)
	if err := s.Publish(observation(1, 100, 400, true), 100); err != nil {
		t.Fatal(err)
	}
	next := testScope
	next.ProjectionGeneration++
	if err := s.ReplaceFactScope(next); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 101) != Indeterminate {
		t.Fatal("old Up survived projection change")
	}
	if err := s.Publish(observation(2, 100, 400, true), 101); err == nil {
		t.Fatal("old projection notification accepted")
	}
	if err := s.ActivateProducer(testScope, "health/wan", testEpoch); err == nil {
		t.Fatal("old projection producer accepted")
	}
	if err := s.ActivateProducer(next, "health/wan", testEpoch); err != nil {
		t.Fatal(err)
	}
	fresh := observation(1, 101, 400, false)
	fresh.Scope = next
	if err := s.Publish(fresh, 101); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 150) != False {
		t.Fatal("new projection observed false not visible")
	}
	boot := next
	boot.BootID = "boot-b"
	if err := s.ReplaceFactScope(boot); err != nil {
		t.Fatal(err)
	}
	if s.FactValue("health/wan/up", 151) != Indeterminate {
		t.Fatal("old Up/Down survived boot change")
	}
}

func TestFactOwnershipAndInvalidRecords(t *testing.T) {
	s := newTestStore(t)
	bad := []ProtocolFact{
		func() ProtocolFact { f := observation(1, 100, 200, true); f.Key = ""; return f }(),
		observation(0, 100, 200, true),
		observation(1, 100, 0, true),
		observation(1, 200, 300, true),
		observation(1, 100, 100, true),
		func() ProtocolFact { f := observation(1, 100, 200, true); f.Scope.BootID = "other"; return f }(),
	}
	for i, fact := range bad {
		if err := s.Publish(fact, 100); err == nil {
			t.Fatalf("invalid fact %d accepted", i)
		}
	}
	if err := s.Publish(observation(1, 100, 200, true), 100); err != nil {
		t.Fatalf("rejected facts advanced sequence: %v", err)
	}
	other := ProducerEpoch{Epoch: 1, WorkerGeneration: 1}
	if err := s.ActivateProducer(testScope, "foreign", other); err != nil {
		t.Fatal(err)
	}
	foreign := ProtocolFact{Key: "health/wan/up", Producer: "foreign", Scope: testScope, Epoch: other, Sequence: 1, ObservedNanos: 101, ExpiresNanos: 200, Value: false}
	if err := s.Publish(foreign, 101); err == nil || !strings.Contains(err.Error(), "owned") {
		t.Fatalf("foreign producer displaced active fact: %v", err)
	}
	if s.FactValue("health/wan/up", 150) != True {
		t.Fatal("foreign producer stole a fact")
	}
}

func TestConcurrentFactReadsAndUpdates(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := uint64(1); i <= 100; i++ {
			if err := s.Publish(observation(i, 100+i, 1000+i, i%2 == 0), 100+i); err != nil {
				t.Errorf("publish %d: %v", i, err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := uint64(1); i <= 100; i++ {
			_ = s.FactValue("health/wan/up", 100+i)
		}
	}()
	wg.Wait()
}
