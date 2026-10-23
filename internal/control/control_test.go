package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
)

type fakeNetwork struct {
	calls []uint64
	fail  map[uint64]bool
}

func (f *fakeNetwork) Apply(_ context.Context, rev uint64, _ network.Desired) error {
	f.calls = append(f.calls, rev)
	if f.fail[rev] {
		return errors.New("injected netlink failure")
	}
	return nil
}
func (f *fakeNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	return network.Snapshot{}, nil
}
func fixture(t *testing.T) (*Engine, *fakeNetwork) {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "bootstrap.json")
	if err := config.AtomicJSON(p, config.Document{SchemaVersion: 1}); err != nil {
		t.Fatal(err)
	}
	s, c, err := config.Open(filepath.Join(d, "state"), p)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNetwork{fail: map[uint64]bool{}}
	e := New(s, c, f)
	if err = e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func request(e *Engine, id string) CommitRequest {
	return CommitRequest{ExpectedRevision: e.State().Revision, RequestID: id, Config: config.Document{SchemaVersion: 1}}
}

func TestDurableCommitReplayAndConflict(t *testing.T) {
	e, n := fixture(t)
	r := request(e, "one")
	s, err := e.Commit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != 2 || !s.Healthy {
		t.Fatalf("unexpected state %+v", s)
	}
	count := len(n.calls)
	if _, err = e.Commit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(n.calls) != count {
		t.Fatal("duplicate commit touched kernel")
	}
	c, err := e.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e2 := New(e.store, c, n)
	if _, err = e2.Commit(context.Background(), r); err != nil {
		t.Fatalf("durable idempotency: %v", err)
	}
	r.RequestID = "two"
	if _, err = e.Commit(context.Background(), r); !errors.Is(err, ErrConflict) {
		t.Fatalf("want stale revision rejection, got %v", err)
	}
	r = request(e, "one")
	r.Config.Network.Interfaces = []network.Interface{{Name: "eth0"}}
	if _, err = e.Commit(context.Background(), r); !errors.Is(err, ErrConflict) {
		t.Fatalf("request ID reused: %v", err)
	}
}

func TestFailedApplyRestoresPreviousDesired(t *testing.T) {
	e, n := fixture(t)
	n.fail[2] = true
	if _, err := e.Commit(context.Background(), request(e, "fail")); err == nil {
		t.Fatal("expected apply failure")
	}
	s := e.State()
	if s.Revision != 1 || !s.Healthy || s.LastError == "" {
		t.Fatalf("wrong rollback state %+v", s)
	}
	if n.calls[len(n.calls)-1] != 1 {
		t.Fatal("did not reconcile previous desired")
	}
	c, err := e.store.Load()
	if err != nil || c.Revision != 1 {
		t.Fatalf("failed revision persisted: %+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(e.store.Dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("successful rollback left pending record")
	}
}

func TestFailedRollbackStaysDegraded(t *testing.T) {
	e, n := fixture(t)
	n.fail[2] = true
	n.fail[1] = true
	if _, err := e.Commit(context.Background(), request(e, "fail-both")); err == nil {
		t.Fatal("expected failure")
	}
	if e.State().Healthy {
		t.Fatal("failed rollback declared healthy")
	}
	if _, err := os.Stat(filepath.Join(e.store.Dir, "pending.json")); err != nil {
		t.Fatal("lost pending recovery record")
	}
}

func TestCrashRecoveryUsesCommittedNotPending(t *testing.T) {
	e, n := fixture(t)
	pending := e.State().Committed
	pending.Revision = 99
	if err := e.store.Stage(pending); err != nil {
		t.Fatal(err)
	}
	c, err := e.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(e.store, c, n)
	if err = restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n.calls[len(n.calls)-1] != 1 {
		t.Fatal("replayed uncommitted candidate")
	}
	if restarted.State().ProducerEpoch == e.State().ProducerEpoch {
		t.Fatal("restart did not change producer epoch")
	}
}

func TestValidationHasNoSideEffectsAndEventGapsReset(t *testing.T) {
	e, n := fixture(t)
	r := request(e, "bad")
	r.Config.SchemaVersion = 999
	if _, err := e.Commit(context.Background(), r); !errors.Is(err, ErrInvalid) {
		t.Fatalf("validation: %v", err)
	}
	if len(n.calls) != 1 {
		t.Fatal("invalid request touched kernel")
	}
	s := e.State()
	for range 300 {
		e.mu.Lock()
		e.record("test", "")
		e.mu.Unlock()
	}
	if !e.Changes(s.ProducerEpoch, s.Sequence).Reset {
		t.Fatal("event gap not detected")
	}
	now := e.State()
	if e.Changes(now.ProducerEpoch, now.Sequence).Reset {
		t.Fatal("current cursor unexpectedly reset")
	}
	if !e.Changes("old-process", 0).Reset {
		t.Fatal("producer restart not detected")
	}
}

func TestConfirmedCommitExpiresAndBumpsRevision(t *testing.T) {
	e, n := fixture(t)
	r := request(e, "trial")
	r.ConfirmTimeoutSeconds = 60
	s, err := e.Commit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if s.Confirmation == nil {
		t.Fatal("missing persisted confirmation record")
	}
	if _, err = e.Commit(context.Background(), request(e, "overlap")); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping trial: %v", err)
	}
	if err = e.Expire(context.Background(), s.Confirmation.Deadline.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s = e.State()
	if s.Revision != 3 || s.Confirmation != nil || !s.Healthy || n.calls[len(n.calls)-1] != 3 {
		t.Fatalf("bad timeout recovery %+v", s)
	}
	if _, err = e.Confirm(ConfirmRequest{2, "trial"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late confirmation: %v", err)
	}
}

func TestConfirmedCommitSurvivesOnlyAfterConfirmation(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		e, n := fixture(t)
		r := request(e, "trial")
		r.ConfirmTimeoutSeconds = 60
		if _, err := e.Commit(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		if confirm {
			if _, err := e.Confirm(ConfirmRequest{2, "trial"}); err != nil {
				t.Fatal(err)
			}
		}
		c, err := e.store.Load()
		if err != nil {
			t.Fatal(err)
		}
		restarted := New(e.store, c, n)
		if err = restarted.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := uint64(3)
		if confirm {
			want = 2
		}
		if restarted.State().Revision != want {
			t.Fatalf("confirm=%t: revision=%d want=%d", confirm, restarted.State().Revision, want)
		}
	}
}
