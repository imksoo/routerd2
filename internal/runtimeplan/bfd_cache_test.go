package runtimeplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type blockedBFDNetwork struct {
	Network
	once             sync.Once
	entered, release chan struct{}
	failure          error
}

func (n *blockedBFDNetwork) Apply(ctx context.Context, revision uint64, desired network.Desired) error {
	wait := false
	n.once.Do(func() { wait = true; close(n.entered) })
	if wait {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-n.release:
		}
		if n.failure != nil {
			return n.failure
		}
	}
	return n.Network.Apply(ctx, revision, desired)
}

func blockBFDNetwork(t *testing.T, r *Runtime, backend Network, failure error) (*blockedBFDNetwork, func()) {
	t.Helper()
	blocked := &blockedBFDNetwork{Network: backend, entered: make(chan struct{}), release: make(chan struct{}), failure: failure}
	r.mu.Lock()
	r.network, r.appliedHash = blocked, ""
	r.mu.Unlock()
	var release sync.Once
	finish := func() { release.Do(func() { close(blocked.release) }) }
	t.Cleanup(finish)
	return blocked, finish
}

func fastBFDCall(t *testing.T, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("BFD observation waited for the unrelated network writer")
	}
}

func nextBFDUpdate(t *testing.T, r *Runtime, doc config.Native, sequence uint64, coalesced bool) serviceconfig.Update {
	t.Helper()
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	value := observedBFD(t, doc.Services[0].BFDConfig(), clock)
	value.Peers[0].Received = sequence + 1
	if coalesced {
		value.Peers[0].Received += 2
		value.Peers[0].Transitions, value.Peers[0].UpTransitions, value.Peers[0].DownTransitions = 4, 2, 1
	}
	return update(t, r, "service.edge", sequence, value)
}

func TestBFDCacheUpdatesAndQueriesRemainIndependentOfBlockedNetworkWriter(t *testing.T) {
	doc := bfdBGPDocument()
	r, native, _ := fixture(t, doc)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := r.Accept(ctx, nextBFDUpdate(t, r, doc, 1, false)); err != nil {
		t.Fatal(err)
	}
	bgp := observedBGPRoutes()
	if err := r.Accept(ctx, update(t, r, "service.router", 1, bgp)); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	view, err := r.BFDProjection("service.router", digest, 1)
	if err != nil {
		t.Fatal(err)
	}
	bgp.BFD = map[string]bgpconfig.BFDAcknowledgement{"192.0.2.2": proofOf(view.Peers["192.0.2.2"])}
	if err := r.Accept(ctx, update(t, r, "service.router", 2, bgp)); err != nil || len(native.applied.Routes) != 1 {
		t.Fatal("initial acknowledged path missing", err)
	}
	publication := nextBFDUpdate(t, r, doc, 2, true)
	blocked, release := blockBFDNetwork(t, r, native, nil)
	projectionDone := make(chan error, 1)
	go func() { projectionDone <- r.reconcileDynamic(ctx) }()
	select {
	case <-blocked.entered:
	case <-ctx.Done():
		t.Fatal("network writer did not start")
	}
	fastBFDCall(t, func() error {
		call, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		return r.Accept(call, publication)
	})
	fastBFDCall(t, func() error {
		view, err := r.BFDProjection("service.router", digest, 1)
		if err != nil {
			return err
		}
		grant := view.Peers["192.0.2.2"]
		if !grant.Up || grant.Sequence != 2 || grant.DownTransitions != 1 {
			return errors.New("independent health query lost the latest coalesced failure")
		}
		return nil
	})
	fastBFDCall(t, func() error {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/services/service.edge", nil))
		var status InstanceStatus
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			return err
		}
		if w.Code != http.StatusOK || status.Sequence != 2 || status.BFD == nil || len(status.BFD.HealthyPeers) != 1 {
			return errors.New("individual BFD status blocked or lost cached facts")
		}
		return nil
	})
	release()
	if err := <-projectionDone; err != nil {
		t.Fatal(err)
	}
	// The existing single writer finishes its point-in-time operation. Its next
	// reconciliation must use the newer failure counter, never the older copy
	// it held before that operation, and must withdraw the pre-reset proof.
	if err := r.reconcileDynamic(ctx); err != nil || len(native.applied.Routes) != 0 {
		t.Fatal("writer did not consume the latest cached BFD fact", native.applied.Routes, err)
	}
	for _, instance := range r.State().Instances {
		if instance.Name == "service.edge" && instance.Sequence != 2 {
			t.Fatal("full status failed to merge cached BFD state")
		}
	}
}

func TestBFDCacheRecipientGenerationBecomesVisibleOnlyAfterSuccessfulACK(t *testing.T) {
	doc := bfdBGPDocument()
	r, native, _ := fixture(t, doc)
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := r.Accept(ctx, nextBFDUpdate(t, r, doc, 1, false)); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	bgp := observedBGPRoutes()
	publication := update(t, r, "service.router", 1, bgp)
	for _, iteration := range []struct {
		generation uint64
		fail       bool
	}{{1, false}, {2, true}, {2, false}} {
		publication.Generation = iteration.generation
		publication.ProducerEpoch = strings.Repeat(string(rune('a'+iteration.generation)), 32)
		var failure error
		if iteration.fail {
			failure = errors.New("injected kernel apply failure")
		}
		blocked, release := blockBFDNetwork(t, r, native, failure)
		done := make(chan error, 1)
		go func(u serviceconfig.Update) { done <- r.Accept(ctx, u) }(publication)
		select {
		case <-blocked.entered:
		case <-ctx.Done():
			t.Fatal("recipient apply did not start")
		}
		fastBFDCall(t, func() error {
			_, err := r.BFDProjection("service.router", digest, iteration.generation)
			if !errors.Is(err, ErrStale) {
				return errors.New("pending recipient generation obtained a health projection")
			}
			return nil
		})
		release()
		err := <-done
		if iteration.fail {
			if err == nil {
				t.Fatal("failed recipient publication was acknowledged")
			}
			if _, err := r.BFDProjection("service.router", digest, 2); !errors.Is(err, ErrStale) {
				t.Fatal("failed generation retained a health capability", err)
			}
			if _, err := r.BFDProjection("service.router", digest, 1); err != nil {
				t.Fatal("rollback lost the previous acknowledged recipient", err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if view, err := r.BFDProjection("service.router", digest, iteration.generation); err != nil || !view.Peers["192.0.2.2"].Up {
				t.Fatal("successful recipient ACK did not authorize current health", view, err)
			}
		}
	}
	if _, err := r.BFDProjection("service.router", digest, 1); !errors.Is(err, ErrStale) {
		t.Fatal("old recipient generation survived a successful replacement", err)
	}
}

func TestBFDCacheCutoverPreservesNewerUnchangedSourceAndFencesChangedSources(t *testing.T) {
	doc := bfdBGPDocument()
	r, _, _ := fixture(t, doc)
	ctx := context.Background()
	initial := nextBFDUpdate(t, r, doc, 1, false)
	if err := r.Accept(ctx, initial); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(ctx, update(t, r, "service.router", 1, observedBGPRoutes())); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	newer := nextBFDUpdate(t, r, doc, 2, true)
	r.mu.Lock()
	retained, err := r.retainedLocked(doc)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	sources, recipients, err := compileBFDCache(r.bindings)
	if err != nil {
		r.mu.Unlock()
		t.Fatal(err)
	}
	// A publisher advances while the configuration writer owns mu and still
	// carries its earlier retained copy. No source reverts at cache cutover.
	done := make(chan error, 1)
	go func() { done <- r.Accept(ctx, newer) }()
	select {
	case err := <-done:
		if err != nil {
			r.mu.Unlock()
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		r.mu.Unlock()
		t.Fatal("unchanged source could not advance during config preparation")
	}
	r.records = retained
	r.installBFDCacheLocked(sources, recipients)
	effective := r.effectiveRecordsLocked(time.Now())
	r.mu.Unlock()
	if effective["service.edge"].Sequence != 2 || !bytes.Equal(effective["service.edge"].Snapshot, newer.Snapshot) {
		t.Fatal("cutover restored an older BFD receive/counter snapshot")
	}
	if view, err := r.BFDProjection("service.router", digest, 1); err != nil || view.Peers["192.0.2.2"].DownTransitions != 1 {
		t.Fatal("cutover lost the source generation fence", view, err)
	}
	changed := bfdBGPDocument()
	changed.Services[0].BFD.EffectTimeoutMilliseconds = 2000
	if err := r.ApplyConfig(ctx, 2, config.FromNative(changed)); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(ctx, newer); !errors.Is(err, ErrStale) {
		t.Fatal("changed source accepted its old configuration digest", err)
	}
	if view, err := r.BFDProjection("service.router", digest, 1); err != nil || view.Peers["192.0.2.2"].Up || view.Peers["192.0.2.2"].ConfigDigest == initial.ConfigDigest {
		t.Fatal("changed source reused old health before a new observation", view, err)
	}
	newSource := nextBFDUpdate(t, r, changed, 1, false)
	if err := r.Accept(ctx, newSource); err != nil {
		t.Fatal(err)
	}
	if view, err := r.BFDProjection("service.router", digest, 1); err != nil || !view.Peers["192.0.2.2"].Up || view.Peers["192.0.2.2"].ConfigDigest != newSource.ConfigDigest {
		t.Fatal("new source did not acquire its independent binding", view, err)
	}
	if err := r.ApplyConfig(ctx, 3, config.FromNative(config.Native{})); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(ctx, newSource); !errors.Is(err, ErrStale) {
		t.Fatal("removed BFD source retained a publication capability", err)
	}
	if _, err := r.BFDProjection("service.router", digest, 1); !errors.Is(err, ErrStale) {
		t.Fatal("removed recipient retained its BFD capability", err)
	}
	if _, exists := r.bfdInstanceStatus("service.edge", true); exists {
		t.Fatal("removed source remained in the independent status cache")
	}
}

func TestBFDCacheAcceptedEvidenceIsImmutableAndCancellationHasNoEffect(t *testing.T) {
	doc := bfdBGPDocument()
	r, _, _ := fixture(t, doc)
	first := nextBFDUpdate(t, r, doc, 1, false)
	if err := r.Accept(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(context.Background(), update(t, r, "service.router", 1, observedBGPRoutes())); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	want := append([]byte(nil), first.Snapshot...)
	first.Snapshot[0] = '['
	record := r.bfdRecord("service.edge", first.ConfigDigest)
	if !bytes.Equal(record.Snapshot, want) {
		t.Fatal("caller modified an already accepted source record")
	}
	if view, err := r.BFDProjection("service.router", digest, 1); err != nil || !view.Peers["192.0.2.2"].Up {
		t.Fatal("caller modified typed source evidence", view, err)
	}
	next := nextBFDUpdate(t, r, doc, 2, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Accept(ctx, next); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled BFD publication was accepted", err)
	}
	if view, err := r.BFDProjection("service.router", digest, 1); err != nil || view.Peers["192.0.2.2"].Sequence != 1 {
		t.Fatal("canceled publication advanced the dependency", view, err)
	}
	if _, err := r.bfdProjection(ctx, "service.router", digest, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled query observed source state", err)
	}
}

func TestBFDCacheGrantQueriesReadOnlyCompiledRecipientReferences(t *testing.T) {
	doc := bfdBGPDocument()
	r, _, _ := fixture(t, doc)
	if err := r.Accept(context.Background(), nextBFDUpdate(t, r, doc, 1, false)); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(context.Background(), update(t, r, "service.router", 1, observedBGPRoutes())); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	// These are deliberately not a second accepted source. Poison unrelated
	// legacy/full-state data to ensure the endpoint never walks/decodes it.
	r.mu.Lock()
	r.records["service.unrelated"] = serviceconfig.Update{Role: "bfd", Snapshot: json.RawMessage(strings.Repeat("!", 2<<20))}
	owner := r.bindings["service.router"]
	owner.instance.Config = json.RawMessage(`invalid legacy/full config`)
	r.bindings["service.router"] = owner
	r.current.Services = nil
	r.mu.Unlock()
	view, err := r.BFDProjection("service.router", digest, 1)
	if err != nil || len(view.Peers) != 1 || !view.Peers["192.0.2.2"].Up {
		t.Fatal("a minimal query consulted unrelated full config/records", view, err)
	}
}

func TestBFDCacheProjectionCapturesEvidenceBeforeEvaluationClock(t *testing.T) {
	doc := bfdBGPDocument()
	r, _, _ := fixture(t, doc)
	ctx := context.Background()
	if err := r.Accept(ctx, nextBFDUpdate(t, r, doc, 1, false)); err != nil {
		t.Fatal(err)
	}
	if err := r.Accept(ctx, update(t, r, "service.router", 1, observedBGPRoutes())); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	queryClock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	// The clock hook publishes evidence newer than its returned sample. A
	// clock-before-cache implementation would return that newer sequence as
	// Down, followed by Up for the same sequence on the next normal query.
	projection, err := r.bfdProjectionWithClock(ctx, "service.router", digest, 1, func() (bfd.Clock, error) {
		receiveClock, err := bfd.ReadClock()
		if err != nil || receiveClock.BoottimeNanoseconds <= queryClock.BoottimeNanoseconds {
			t.Fatal("later receive clock unavailable", err)
		}
		seen := observedBFD(t, doc.Services[0].BFDConfig(), receiveClock)
		peer := &seen.Peers[0]
		peer.Received = 3
		peer.LastReceivedAt, peer.LastReceivedBootNanoseconds = receiveClock.WallTime, receiveClock.BoottimeNanoseconds
		peer.ValidUntil = receiveClock.WallTime.Add(900 * time.Millisecond)
		peer.DeadlineBootNanoseconds = receiveClock.BoottimeNanoseconds + int64(900*time.Millisecond)
		next := update(t, r, "service.edge", 2, seen)
		fastBFDCall(t, func() error { return r.Accept(ctx, next) })
		return queryClock, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if grant := projection.Peers["192.0.2.2"]; !grant.Up || grant.Sequence != 1 {
		t.Fatalf("query mixed a clock with newer uncaptured evidence: %+v", grant)
	}
	projection, err = r.BFDProjection("service.router", digest, 1)
	if err != nil || !projection.Peers["192.0.2.2"].Up || projection.Peers["192.0.2.2"].Sequence != 2 {
		t.Fatalf("subsequent query lost the newly accepted evidence: %+v, %v", projection, err)
	}
}
