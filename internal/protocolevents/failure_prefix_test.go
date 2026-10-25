package protocolevents

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
)

func TestNewTargetFailureCanRetainAnAlreadyCommittedPendingPrefix(t *testing.T) {
	ctx := context.Background()
	local, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "local")})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	dir := filepath.Join(t.TempDir(), "target")
	target, err := events.Open(events.Options{Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	q := NewOutbox(local, "wan")
	if err = q.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	send := func(ctx context.Context, page []events.Event) error {
		for _, e := range page {
			if _, err := target.Append(ctx, e); err != nil {
				return err
			}
		}
		return nil
	}
	if err = q.Flush(ctx, func(ctx context.Context, page []events.Event) error {
		if err := send(ctx, page); err != nil {
			return err
		}
		return errors.New("target committed but acknowledgment was lost")
	}); err == nil {
		t.Fatal("lost acknowledgment hidden")
	}
	before, err := target.List(ctx, 0, 16)
	if err != nil || len(before) != 1 {
		t.Fatal(before, err)
	}
	if err = q.Enqueue(ctx, probeEvent("wan")); err != nil {
		t.Fatal(err)
	}
	injector, err := sql.Open("sqlite", filepath.Join(dir, "journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err = injector.Exec(`CREATE TRIGGER fail_new_health BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'injected new target failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = q.Flush(ctx, send); err == nil {
		t.Fatal("new target insert failure hidden")
	}
	pending, err := q.Pending(ctx)
	if err != nil || len(pending) != 2 || pending[0].ID != before[0].Event.ID {
		t.Fatal(pending, err)
	}
	after, err := target.List(ctx, 0, 16)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("fault admitted a new observation or changed prefix", after, err)
	}
	// The old integration oracle rejected this legitimate pending/history
	// intersection. Only IDs absent at the fault boundary must remain absent.
	if pending[1].ID == after[0].Event.ID {
		t.Fatal("new pending observation crossed failed insert")
	}
	if _, err = injector.Exec(`DROP TRIGGER fail_new_health`); err != nil {
		t.Fatal(err)
	}
	if err = q.Flush(ctx, send); err != nil {
		t.Fatal(err)
	}
	complete, err := target.List(ctx, 0, 16)
	if err != nil || len(complete) != 2 || !reflect.DeepEqual(complete[0], before[0]) || complete[1].Event.ID != pending[1].ID {
		t.Fatal("recovery lost stable prefix/new observation", complete, err)
	}
	if remaining, err := q.Pending(ctx); err != nil || len(remaining) != 0 {
		t.Fatal(remaining, err)
	}
}
