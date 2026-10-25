package eventcontroller

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

var epoch = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

func declaration(t *testing.T, operator string, threshold int) resourceprogram.EventRule {
	t.Helper()
	spec := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: operator, Topic: "fixture.input", Threshold: threshold, Duration: "1s"}, Emit: api.EventRuleEmitSpec{Topic: "fixture.output", Attributes: map[string]string{"count": "${count}"}}}
	if operator == "absence" {
		spec.Pattern.Trigger = "fixture.input"
		spec.Pattern.Expected = "fixture.expected"
	}
	p, err := resourceprogram.Compile(resourceprogram.Program{SchemaVersion: resourceprogram.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "rule", EventRule: &spec}}})
	if err != nil {
		t.Fatal(err)
	}
	return p.Native.EventRules[0]
}

func appendInput(t *testing.T, s *events.Store, id string) {
	t.Helper()
	if _, err := s.Append(context.Background(), events.Event{ID: id, Topic: "fixture.input", Source: "fixture", ObservedAt: epoch}); err != nil {
		t.Fatal(err)
	}
}

func outputCount(t *testing.T, s *events.Store) int {
	t.Helper()
	rows, err := s.List(context.Background(), 0, 256)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, row := range rows {
		if row.Event.Topic == "fixture.output" {
			n++
			if row.Event.Detail == nil || row.Event.Detail.Daemon.Kind != "routerd-eventrule" || row.Event.Detail.Resource == nil {
				t.Fatal("output lost typed metadata", row)
			}
		}
	}
	return n
}

func TestCountSurvivesControlRestartAndUnrelatedRevisionWithoutRecounting(t *testing.T) {
	ctx := context.Background()
	s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	appendInput(t, s, "historical")
	c := New(s)
	rule := declaration(t, "count", 2)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "one")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 0 {
		t.Fatal("new consumer replayed historical input")
	}
	c = New(s)
	if err = c.Sync(ctx, 2, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "two")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("count reset across unchanged definition/restart")
	}
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("output reprocessed itself or duplicated")
	}
}

func TestTimerRestoresButDeactivationAndSourceChangeFenceOldWork(t *testing.T) {
	ctx := context.Background()
	s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rule := declaration(t, "debounce", 0)
	c := New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "one")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	c = New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	if err = c.Step(ctx, epoch.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("restored due timer lost")
	}
	appendInput(t, s, "two")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if err = c.Sync(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	if err = c.Step(ctx, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("inactive lifetime timer revived")
	}
	appendInput(t, s, "three")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	spec := rule.Spec
	spec.Emit.Attributes = map[string]string{"changed": "true"}
	compiled, err := resourceprogram.Compile(resourceprogram.Program{SchemaVersion: resourceprogram.SchemaVersion, Resources: []resourceprogram.Resource{{ID: rule.ID, EventRule: &spec}}})
	if err != nil {
		t.Fatal(err)
	}
	rule = compiled.Native.EventRules[0]
	if err = c.Sync(ctx, 2, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	if err = c.Step(ctx, epoch.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("old source timer published")
	}
}

func TestPublishedOutputRetryDeduplicatesBeforeCompletion(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "events")
	s, err := events.Open(events.Options{Directory: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	injector, err := sql.Open("sqlite", filepath.Join(directory, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	c := New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{declaration(t, "count", 1)}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "one")
	// Fail the completion ACK only after journal publication. Retry must use
	// the original output ID/content and retain the committed aggregate.
	if _, err = injector.Exec(`CREATE TRIGGER fail_completion BEFORE UPDATE ON outbox BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = c.Step(ctx, epoch); err == nil {
		t.Fatal("expected failed output ACK")
	}
	if outputCount(t, s) != 1 {
		t.Fatal("output not published before failed ACK")
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_completion`); err != nil {
		t.Fatal(err)
	}
	c = New(s)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{declaration(t, "count", 1)}); err != nil {
		t.Fatal(err)
	}
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("failed ACK replayed a duplicate output")
	}
	// Force JSON encoding coverage of the bounded public status, too.
	if _, err = json.Marshal(c.Status()); err != nil {
		t.Fatal(err)
	}
}

func TestAbsenceOrdersQueuedObservationsAgainstTheirOriginalDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
		want int
	}{{"expected-before-deadline", 500 * time.Millisecond, 0}, {"expected-at-deadline", time.Second, 0}, {"expected-after-deadline", 2 * time.Second, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			rule := declaration(t, "absence", 0)
			c := New(s)
			if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
				t.Fatal(err)
			}
			appendInput(t, s, "trigger")
			if err = c.Step(ctx, epoch); err != nil {
				t.Fatal(err)
			}
			at := epoch.Add(tc.at)
			if _, err = s.Append(ctx, events.Event{ID: "expected", Topic: "fixture.expected", Source: "fixture", ObservedAt: at}); err != nil {
				t.Fatal(err)
			}
			if err = c.Step(ctx, epoch.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if got := outputCount(t, s); got != want {
				t.Fatalf("absence emissions %d, want %d", got, want)
			}
		})
	}
}

func TestUnobservedRevisionGapStartsFreshAtJournalTip(t *testing.T) {
	ctx := context.Background()
	s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s)
	rule := declaration(t, "count", 2)
	if err = c.Sync(ctx, 1, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "old-count")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "during-unobserved-interval")
	if err = c.Sync(ctx, 3, []resourceprogram.EventRule{rule}); err != nil {
		t.Fatal(err)
	}
	appendInput(t, s, "fresh-one")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 0 {
		t.Fatal("unobserved revision gap inherited aggregate or replayed past input")
	}
	appendInput(t, s, "fresh-two")
	if err = c.Step(ctx, epoch); err != nil {
		t.Fatal(err)
	}
	if outputCount(t, s) != 1 {
		t.Fatal("fresh lifetime did not count its own inputs")
	}
}
