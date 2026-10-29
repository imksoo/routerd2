//go:build linux

package release

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/configref"
)

func pairOptions() PrepareOptions {
	base := configref.Ref{StoreID: strings.Repeat("a", 32), Revision: 7, Digest: strings.Repeat("b", 64)}
	target := configref.Ref{StoreID: base.StoreID, Revision: 3, Digest: strings.Repeat("c", 64)}
	return PrepareOptions{MaxAttempts: 2, Startup: &configref.Selection{Generation: 20, ExpectedRevision: 7, Snapshot: target}, KnownGood: &base}
}

func TestPairedPrepareBootAndConditionalFallbackAreOneGeneration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first := initializeFixture(t, s)
	second := stageFixture(t, s, "fixture-r2", 2)
	opts := pairOptions()
	p, err := s.Prepare(ctx, second, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	if *p.Active != first || p.Startup != nil || p.Pending.Startup == nil || !p.Pending.Startup.Equal(*opts.Startup) || p.Pending.Fallback == nil || p.ConfigHighWatermark != 21 || p.ConfigStoreID != opts.KnownGood.StoreID {
		t.Fatal("prepare mixed active runtime with next configuration")
	}
	if p.Pending.Fallback.Recovery == nil || p.Pending.Fallback.Recovery.FailedGeneration != 20 || p.Pending.Fallback.Snapshot != *opts.KnownGood {
		t.Fatal("fallback lacks its exact conditional fence")
	}
	b, err := s.BeginBoot(ctx, "paired-boot-1")
	if err != nil {
		t.Fatal(err)
	}
	b.Image.Close()
	if *b.State.Active != second || b.State.Startup == nil || !b.State.Startup.Equal(*opts.Startup) || *b.State.Previous != first || b.State.PreviousStartup == nil || !b.State.PreviousStartup.Equal(*p.Pending.Fallback) {
		t.Fatal("boot did not select exact candidate and fallback tuples")
	}
	duplicate, err := s.BeginBoot(ctx, "paired-boot-1")
	if err != nil {
		t.Fatal(err)
	}
	duplicate.Image.Close()
	if !reflect.DeepEqual(b.State, duplicate.State) {
		t.Fatal("exec handoff spent a second attempt or changed configuration")
	}
	if _, err := s.Confirm(ctx, "paired-boot-1", b.State.Attempt.Token, second.Digest); !errors.Is(err, ErrConfigurationRequired) {
		t.Fatal("runtime-only confirmation accepted a configuration pair", err)
	}
	f, err := s.FailBoot(ctx, "paired-boot-1", b.State.Attempt.Token, "configuration was not ready")
	if err != nil {
		t.Fatal(err)
	}
	if *f.Active != first || f.Startup == nil || !f.Startup.Equal(*p.Pending.Fallback) || f.Previous != nil || f.PreviousStartup != nil || f.Pending != nil || f.ConfigHighWatermark != 21 || f.LastFailure.Startup == nil || !f.LastFailure.Startup.Equal(*opts.Startup) {
		t.Fatal("failure mixed runtime and configuration or lost fences")
	}
}

func TestPairedConfirmationRequiresExactReceiptAndPreparedRollback(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	first := initializeFixture(t, s)
	second := stageFixture(t, s, "fixture-r2", 2)
	opts := pairOptions()
	if _, err := s.Prepare(ctx, second, 1, opts); err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginBoot(ctx, "paired-confirm")
	if err != nil {
		t.Fatal(err)
	}
	b.Image.Close()
	proof := ConfigurationConfirmation{Adoption: configref.Adoption{Selection: opts.Startup.Clone(), Revision: 8}, Revision: 8}
	for _, mutate := range []func(*ConfigurationConfirmation){func(p *ConfigurationConfirmation) { p.Revision++ }, func(p *ConfigurationConfirmation) { p.Adoption.Selection.Generation++ }, func(p *ConfigurationConfirmation) { p.Adoption.Selection.Snapshot.Digest = strings.Repeat("d", 64) }} {
		bad := proof
		bad.Adoption.Selection = proof.Adoption.Selection.Clone()
		mutate(&bad)
		if _, err := s.ConfirmConfiguration(ctx, "paired-confirm", b.State.Attempt.Token, second.Digest, func(context.Context) (ConfigurationConfirmation, error) { return bad, nil }); !errors.Is(err, ErrStale) {
			t.Fatal("wrong or changed configuration confirmed", err)
		}
	}
	c, err := s.ConfirmConfiguration(ctx, "paired-confirm", b.State.Attempt.Token, second.Digest, func(context.Context) (ConfigurationConfirmation, error) { return proof, nil })
	if err != nil {
		t.Fatal(err)
	}
	if c.Startup == nil || !c.Startup.Equal(*opts.Startup) || c.Pending != nil || c.Attempt != nil {
		t.Fatal("confirmation lost configuration authority")
	}
	if _, err := s.Rollback(ctx, c.Generation, "unsafe direct runtime rollback"); !errors.Is(err, ErrConfigurationRequired) {
		t.Fatal("paired rollback bypassed configuration preflight", err)
	}
	next := pairOptions()
	next.Startup.Generation = 22
	next.Startup.ExpectedRevision = 9
	next.KnownGood.Revision = 9
	next.KnownGood.Digest = strings.Repeat("d", 64)
	p, err := s.PrepareRollback(ctx, c.Generation, next)
	if err != nil {
		t.Fatal(err)
	}
	if p.Pending.Release != first || *p.Active != second || p.HighWatermark != 2 || p.ConfigHighWatermark != 23 || p.Operation != "prepare-rollback" {
		t.Fatal("explicit prepared rollback bypassed trial or lowered a watermark")
	}
}

func TestPairedCancelBurnsGenerationsAndRejectsUnpairedOrForeignPlans(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	initializeFixture(t, s)
	second := stageFixture(t, s, "fixture-r2", 2)
	third := stageFixture(t, s, "fixture-r3", 3)
	opts := pairOptions()
	p, err := s.Prepare(ctx, second, 1, opts)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CancelPending(ctx, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfigHighWatermark != 21 || c.ConfigStoreID != opts.Startup.Snapshot.StoreID {
		t.Fatal("cancel reset configuration authority")
	}
	if _, err := s.Prepare(ctx, third, c.Generation, PrepareOptions{}); !errors.Is(err, ErrConfigurationRequired) {
		t.Fatal("runtime-only update dropped bound configuration", err)
	}
	if _, err := s.Prepare(ctx, third, c.Generation, opts); !errors.Is(err, ErrStale) {
		t.Fatal("canceled selection generation was reused", err)
	}
	opts.Startup.Generation = 22
	foreign := *opts.KnownGood
	foreign.StoreID = strings.Repeat("e", 32)
	opts.KnownGood = &foreign
	if _, err := s.Prepare(ctx, third, c.Generation, opts); err == nil {
		t.Fatal("foreign known-good snapshot accepted")
	}
	if got := mustState(t, s); !reflect.DeepEqual(got, c) {
		t.Fatal("rejected pair changed state")
	}
}

func TestPairedJournalErrorsNeverPublishHalfATuple(t *testing.T) {
	for _, point := range journalBoundaries {
		t.Run(point, func(t *testing.T) {
			s := newTestStore(t)
			initializeFixture(t, s)
			next := stageFixture(t, s, "fixture-r2", 2)
			before := mustState(t, s)
			cut := errors.New("paired publication interruption")
			s.fault = func(at string) error {
				if at == point {
					return cut
				}
				return nil
			}
			_, err := s.Prepare(context.Background(), next, before.Generation, pairOptions())
			s.fault = nil
			if !errors.Is(err, cut) {
				t.Fatal("fault missed real journal boundary", err)
			}
			after := mustState(t, s)
			if errors.Is(err, ErrUncertain) {
				if after.Pending == nil || after.Pending.Startup == nil || after.Pending.Fallback == nil || after.ConfigHighWatermark != 21 {
					t.Fatal("published runtime without complete configuration tuple")
				}
			} else if !reflect.DeepEqual(after, before) {
				t.Fatal("unpublished pair changed selected state")
			}
		})
	}
}

func TestPairedJournalRejectsMismatchedAuthorityAndRecoveryFences(t *testing.T) {
	s := newTestStore(t)
	initializeFixture(t, s)
	next := stageFixture(t, s, "fixture-r2", 2)
	p, err := s.Prepare(context.Background(), next, 1, pairOptions())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*State){
		func(v *State) { v.ConfigHighWatermark++ },
		func(v *State) { v.ConfigStoreID = strings.Repeat("e", 32) },
		func(v *State) { v.Pending.Fallback = nil },
		func(v *State) { v.Pending.Fallback.Recovery.FailedGeneration-- },
		func(v *State) { v.Pending.Fallback.ExpectedRevision++ },
	} {
		var bad State
		if err := json.Unmarshal(raw, &bad); err != nil {
			t.Fatal(err)
		}
		mutate(&bad)
		if err := s.commit(bad); err == nil {
			t.Fatal("mismatched configuration tuple was published")
		}
		if got := mustState(t, s); !reflect.DeepEqual(got, p) {
			t.Fatal("refused tuple changed authority")
		}
	}
}
