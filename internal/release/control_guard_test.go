//go:build linux

package release_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/release"
)

type guardNetwork struct {
	calls        int
	hook         func()
	failRevision uint64
}

func (n *guardNetwork) Apply(_ context.Context, revision uint64, _ network.Desired) error {
	n.calls++
	if n.hook != nil {
		n.hook()
	}
	if revision == n.failRevision {
		return errors.New("injected apply failure")
	}
	return nil
}

func TestReleaseGuardKeepsConfirmedRollbackAndFailedApplyExclusive(t *testing.T) {
	ctx := context.Background()
	root := release.NewFixtureStore(t)
	active := release.InitializeFixture(t, root)
	next := release.StageFixture(t, root, "fixture-r2", 2)
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	if err := config.AtomicJSON(seed, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	cfg, current, err := config.Open(filepath.Join(dir, "state"), seed)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := cfg.Snapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	n := &guardNetwork{}
	e, err := control.NewWithRelease(cfg, current, n, root, active.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := root.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n.hook = func() {
		if _, err := root.Prepare(ctx, next, state.Generation, release.PrepareOptions{}); !errors.Is(err, release.ErrBusy) {
			t.Fatalf("mutation/restore lost release lock: %v", err)
		}
	}
	req := control.CommitRequest{ExpectedRevision: 1, RequestID: "confirmed-trial", Config: current.Config, ConfirmTimeoutSeconds: 5}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Raw store primitives cannot preflight the live writer. Even if a caller
	// prepares a stale pair, no later rollback may silently overwrite its fence.
	selected := configref.Selection{Generation: 10, ExpectedRevision: 1, Snapshot: baseline}
	pending, err := root.Prepare(ctx, next, state.Generation, release.PrepareOptions{Startup: &selected, KnownGood: &baseline})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Expire(ctx, time.Now().Add(time.Hour)); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("pending pair allowed expiry: %v", err)
	}
	if err := e.Reconcile(ctx); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("pending pair allowed restart rollback: %v", err)
	}
	if _, err := e.Confirm(control.ConfirmRequest{Revision: 2, RequestID: req.RequestID}); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("pending pair allowed config confirmation: %v", err)
	}
	if _, err := root.CancelPending(ctx, pending.Generation); err != nil {
		t.Fatal(err)
	}
	if err := e.Expire(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s := e.State(); s.Revision != 3 || s.Confirmation != nil {
		t.Fatal("rollback failed", s)
	}
	n.failRevision = 4
	req.ExpectedRevision, req.RequestID, req.ConfirmTimeoutSeconds = 3, "failed-apply", 0
	if _, err := e.Commit(ctx, req); err == nil {
		t.Fatal("failed apply succeeded")
	}
	if c, err := cfg.Load(); err != nil || c.Revision != 3 {
		t.Fatal("failed apply changed authoritative journal", c, err)
	}
	if _, err := root.Status(ctx); err != nil {
		t.Fatal("failed apply leaked lock", err)
	}
}
func (*guardNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	return network.Snapshot{}, nil
}

func TestActualReleaseStoreFencesControlCommitPrepareTrialAndConfirmation(t *testing.T) {
	ctx := context.Background()
	root := release.NewFixtureStore(t)
	active := release.InitializeFixture(t, root)
	next := release.StageFixture(t, root, "fixture-r2", 2)
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.json")
	if err := config.AtomicJSON(seed, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	cfg, current, err := config.Open(filepath.Join(dir, "state"), seed)
	if err != nil {
		t.Fatal(err)
	}
	n := &guardNetwork{}
	e, err := control.NewWithRelease(cfg, current, n, root, active.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := root.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The competing prepare runs while the real commit is in network apply,
	// before its journal save. A status check followed by an unlocked apply
	// would incorrectly let this prepare succeed.
	n.hook = func() {
		if _, err := root.Prepare(ctx, next, state.Generation, release.PrepareOptions{}); !errors.Is(err, release.ErrBusy) {
			t.Fatalf("prepare entered commit: %v", err)
		}
	}
	req := control.CommitRequest{ExpectedRevision: 1, RequestID: "first", Config: current.Config}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	n.hook = nil
	baseline, err := cfg.Snapshot(2)
	if err != nil {
		t.Fatal(err)
	}
	selected := configref.Selection{Generation: 10, ExpectedRevision: 2, Snapshot: baseline}
	pending, err := root.Prepare(ctx, next, state.Generation, release.PrepareOptions{Startup: &selected, KnownGood: &baseline})
	if err != nil {
		t.Fatal(err)
	}
	calls := n.calls
	if _, err := e.Commit(ctx, req); err != nil || n.calls != calls {
		t.Fatal("durable duplicate repeated apply or was refused", err)
	}
	req.ExpectedRevision, req.RequestID = 2, "blocked"
	if _, err := e.Commit(ctx, req); !errors.Is(err, control.ErrConflict) || n.calls != calls {
		t.Fatal("prepared pair allowed configuration mutation", err)
	}
	if saved, err := cfg.Load(); err != nil || saved.Revision != 2 {
		t.Fatal("blocked commit changed journal", saved, err)
	}
	if err := root.WithState(ctx, func(release.State) error {
		r := httptest.NewRequest(http.MethodPost, "/v1/config/commit", strings.NewReader(`{"expected_revision":2,"request_id":"busy","config":{"schema_version":2}}`))
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("busy HTTP status %d: %s", w.Code, w.Body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.CancelPending(ctx, pending.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatalf("cancel did not unfreeze commits: %v", err)
	}
	// Cancellation reserves the publisher sequence permanently. The next
	// trial uses a fresh publication rather than reusing the canceled image.
	next = release.StageFixture(t, root, "fixture-r3", 3)
	baseline, err = cfg.Snapshot(3)
	if err != nil {
		t.Fatal(err)
	}
	selected = configref.Selection{Generation: 20, ExpectedRevision: 3, Snapshot: baseline}
	state, err = root.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.Prepare(ctx, next, state.Generation, release.PrepareOptions{Startup: &selected, KnownGood: &baseline}); err != nil {
		t.Fatal(err)
	}
	boot, err := root.BeginBoot(ctx, "guard-kernel")
	if err != nil {
		t.Fatal(err)
	}
	boot.Image.Close()
	current, err = cfg.AdoptStartup(selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.NewWithRelease(cfg, current, n, root, active.Digest); !errors.Is(err, control.ErrConflict) {
		t.Fatal("old runtime accepted new selection", err)
	}
	wrong := current
	wrong.StoreID = strings.Repeat("f", 32)
	if _, err := control.NewWithRelease(cfg, wrong, n, root, next.Digest); !errors.Is(err, control.ErrConflict) {
		t.Fatal("foreign configuration store accepted", err)
	}
	wrong = current
	wrong.Startup = nil
	if _, err := control.NewWithRelease(cfg, wrong, n, root, next.Digest); !errors.Is(err, control.ErrConflict) {
		t.Fatal("unconsumed startup accepted", err)
	}
	e, err = control.NewWithRelease(cfg, current, n, root, next.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	req.ExpectedRevision, req.RequestID = current.Revision, "trial"
	if _, err := e.Commit(ctx, req); !errors.Is(err, control.ErrConflict) {
		t.Fatal("trial allowed commit", err)
	}
	if _, err := root.ConfirmConfiguration(ctx, "guard-kernel", boot.State.Attempt.Token, next.Digest, release.ConfigurationConfirmation{Adoption: *current.Startup, Revision: current.Revision}); err != nil {
		t.Fatal(err)
	}
	if s, err := e.Commit(ctx, req); err != nil || s.Revision != 5 {
		t.Fatal("confirmed pair did not unfreeze", s, err)
	}
}
