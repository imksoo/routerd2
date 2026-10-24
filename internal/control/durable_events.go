package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
)

// withChange carries all unacknowledged observations into the next authoritative
// record. Confirmed trials reserve one additional slot for confirm or rollback.
// Call before staging or applying effects, so backpressure cannot mutate them.
func (e *Engine) withChange(next config.Committed, kind string, reserve int) (config.Committed, error) {
	if len(e.state.EventOutbox)+reserve > config.MaxEventOutbox {
		return next, events.ErrCapacity
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return next, err
	}
	next.EventOutbox = append([]config.ChangeEvent(nil), e.state.EventOutbox...)
	next.EventOutbox = append(next.EventOutbox, config.ChangeEvent{ID: hex.EncodeToString(id[:]), Type: kind, Revision: next.Revision, ObservedAt: time.Now().UTC()})
	return next, nil
}

func journalEvent(event config.ChangeEvent) events.Event {
	return events.Event{ID: event.ID, Topic: "configuration." + event.Type, Source: "control", ObservedAt: event.ObservedAt, Attributes: map[string]string{"revision": strconv.FormatUint(event.Revision, 10)}}
}

// FlushEvents never holds the configuration lock during journal I/O. Concurrent
// commits retain the same immutable prefix; only an acknowledged prefix is
// removed, under the single configuration writer lock and atomic fsync/rename.
// A crash before this second save repeats stable IDs, which Append deduplicates.
func (e *Engine) FlushEvents(ctx context.Context) (result error) {
	e.eventFlushMu.Lock()
	defer e.eventFlushMu.Unlock()
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.state.EventJournalError = ""
		if result != nil {
			e.state.EventJournalError = stateText(result.Error(), 1024)
		}
	}()
	if e.journal == nil {
		return errors.New("durable event journal is unavailable")
	}
	e.mu.Lock()
	pending := append([]config.ChangeEvent(nil), e.state.EventOutbox...)
	e.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	for _, event := range pending {
		if _, err := e.journal.Append(ctx, journalEvent(event)); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.state.EventOutbox) < len(pending) {
		return errors.New("configuration event outbox changed during publication")
	}
	for i, event := range pending {
		if e.state.EventOutbox[i] != event {
			return errors.New("configuration event outbox prefix changed during publication")
		}
	}
	next := e.state.Committed
	next.EventOutbox = append([]config.ChangeEvent(nil), next.EventOutbox[len(pending):]...)
	if err := e.store.Save(next); err != nil {
		return err
	}
	e.state.Committed = next
	return nil
}
