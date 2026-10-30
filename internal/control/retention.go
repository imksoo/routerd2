package control

import (
	"context"
	"time"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
)

// Retention runs in the existing event-store maintenance loop. This operation
// never holds the configuration lock while reading bodies or sending UDP.
func (e *Engine) retainEvents(ctx context.Context) error {
	e.mu.Lock()
	policy := e.state.Config.Retention
	revision := e.state.Revision
	journal := e.journal
	if revision != e.retentionRevision {
		e.retentionRevision = revision
		e.retentionAfter = 0
		e.retentionNext = time.Time{}
	}
	after, next := e.retentionAfter, e.retentionNext
	if policy == nil {
		e.eventStore.RetentionError = ""
	}
	e.mu.Unlock()
	if policy == nil || journal == nil || time.Now().Before(next) {
		return nil
	}
	deliver := func(ctx context.Context, event events.Record, body []byte) error {
		record := logging.Record{Entry: logging.Entry{Timestamp: event.Event.ObservedAt, Module: event.Event.Source, Level: "INFO", Message: "retained event " + string(body)}, Sequence: event.Cursor}
		for _, target := range policy.Syslog {
			if err := syslog.Send(ctx, target, []logging.Record{record}); err != nil {
				return err
			}
		}
		return nil
	}
	after, done, deleted, err := journal.PruneHistory(ctx, after, *policy, deliver)
	if err == nil && policy.Vacuum && (deleted > 0 || done) {
		err = journal.VacuumHistory(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if revision != e.state.Revision {
		return err
	}
	e.retentionAfter = after
	e.eventStore.RetentionDeleted += uint64(deleted)
	e.eventStore.RetentionError = ""
	if err != nil {
		e.eventStore.RetentionError = stateText("event retention: "+err.Error(), 1024)
		e.eventStore.LastError = e.eventStore.RetentionError
		e.state.EventStoreError = e.eventStore.RetentionError
	}
	if done && err == nil {
		e.retentionAfter = 0
		e.retentionNext = time.Now().Add(policy.Interval())
	}
	return err
}
