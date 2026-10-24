package eventrule

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd2/internal/events"
)

func TestRuleCountAndEmissionCommitWithDurableInputCheckpoint(t *testing.T) {
	ctx := context.Background()
	options := events.Options{Directory: filepath.Join(t.TempDir(), "journal")}
	s, err := events.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	p := program(t, api.EventRulePatternSpec{Operator: "count", Topic: "routerd.a", Threshold: 2})
	checkpoint, err := s.Checkpoint(ctx, "rule")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		if _, err = s.Append(ctx, events.Event{ID: id, Topic: "routerd.a", Source: "fixture", ObservedAt: epoch}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := s.List(ctx, checkpoint.Cursor, 10)
	if err != nil || len(records) != 2 {
		t.Fatal(records, err)
	}
	advance := func(checkpoint events.Checkpoint, record events.Record) (Result, []events.Job) {
		t.Helper()
		r, err := p.Advance(checkpoint.State, Input{ID: record.Event.ID, Event: daemonapi.DaemonEvent{Time: record.Event.ObservedAt, Type: record.Event.Topic}}, epoch)
		if err != nil {
			t.Fatal(err)
		}
		var jobs []events.Job
		for _, emission := range r.Emissions {
			body, err := json.Marshal(emission)
			if err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, events.Job{Key: emission.ID, Payload: body})
		}
		return r, jobs
	}
	first, jobs := advance(checkpoint, records[0])
	if len(jobs) != 0 {
		t.Fatal("count fired after one input")
	}
	if err = s.Transition(ctx, "rule", checkpoint, records[0].Cursor, first.State, jobs); err != nil {
		t.Fatal(err)
	}
	checkpoint, err = s.Checkpoint(ctx, "rule")
	if err != nil {
		t.Fatal(err)
	}
	second, jobs := advance(checkpoint, records[1])
	if len(jobs) != 1 {
		t.Fatal("threshold did not emit exactly once")
	}
	injector, err := sql.Open("sqlite", filepath.Join(options.Directory, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_rule_checkpoint BEFORE UPDATE ON consumers BEGIN SELECT RAISE(ABORT,'injected checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = s.Transition(ctx, "rule", checkpoint, records[1].Cursor, second.State, jobs); err == nil {
		t.Fatal("expected real transaction failure")
	}
	current, err := s.Checkpoint(ctx, "rule")
	if err != nil || current.Cursor != checkpoint.Cursor || !bytes.Equal(current.State, checkpoint.State) {
		t.Fatal("failed cursor save changed aggregate state", current, err)
	}
	if work, err := s.Pending(ctx, "rule", 10); err != nil || len(work) != 0 {
		t.Fatal("failed cursor save leaked emission", work, err)
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_rule_checkpoint`); err != nil {
		t.Fatal(err)
	}
	retry, retryJobs := advance(current, records[1])
	if !bytes.Equal(retry.State, second.State) || jobs[0].Key != retryJobs[0].Key || !bytes.Equal(jobs[0].Payload, retryJobs[0].Payload) {
		t.Fatal("retry recounted or changed emission identity")
	}
	if err = s.Transition(ctx, "rule", current, records[1].Cursor, retry.State, retryJobs); err != nil {
		t.Fatal(err)
	}
	if err = injector.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = events.Open(options)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := s.Checkpoint(ctx, "rule")
	if err != nil || reopened.Cursor != records[1].Cursor {
		t.Fatal(reopened, err)
	}
	work, err := s.Pending(ctx, "rule", 10)
	if err != nil || len(work) != 1 || work[0].Key != jobs[0].Key {
		t.Fatal("emission lost across reopen", work, err)
	}
	var emission Emission
	if err = json.Unmarshal(work[0].Payload, &emission); err != nil || emission.Event.Attributes["count"] != "2" {
		t.Fatal("incorrect aggregate persisted", emission, err)
	}
	if err = s.Transition(ctx, "rule", checkpoint, records[1].Cursor, second.State, jobs); !errors.Is(err, events.ErrConflict) {
		t.Fatal("old checkpoint emitted twice after restart", err)
	}
}
