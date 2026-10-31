package control

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
)

type EventStoreHealth struct {
	RetentionError   string           `json:"retention_error,omitempty"`
	RetentionDeleted uint64           `json:"retention_deleted"`
	Capacity         *events.Capacity `json:"capacity,omitempty"`
	ObservedAt       time.Time        `json:"observed_at,omitempty"`
	ArchiveError     string           `json:"archive_error,omitempty"`
	CompactionError  string           `json:"compaction_error,omitempty"`
	LastError        string           `json:"last_error,omitempty"`
}

// RefreshEventStore never holds control authority while querying SQLite or
// filesystem space. Health reads only this bounded cache, never event bodies.
func (e *Engine) RefreshEventStore(ctx context.Context) error {
	e.mu.Lock()
	journal := e.journal
	e.mu.Unlock()
	if journal == nil {
		return nil
	}
	capacity, err := journal.Capacity(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal != journal {
		return err
	}
	if err != nil {
		e.eventStore.LastError = stateText(err.Error(), 1024)
	} else {
		e.eventStore = EventStoreHealth{Capacity: &capacity, ObservedAt: time.Now().UTC(), CompactionError: e.eventStore.CompactionError, ArchiveError: e.eventStore.ArchiveError, RetentionError: e.eventStore.RetentionError, RetentionDeleted: e.eventStore.RetentionDeleted}
		if capacity.Pressure == "exhausted" {
			e.eventStore.LastError = "event store capacity exhausted: " + strings.Join(capacity.Reasons, ",")
		}
	}
	if e.eventStore.CompactionError != "" {
		if e.eventStore.LastError == "" {
			e.eventStore.LastError = e.eventStore.CompactionError
		} else if e.eventStore.LastError != e.eventStore.CompactionError {
			e.eventStore.LastError = stateText(e.eventStore.LastError+"; "+e.eventStore.CompactionError, 1024)
		}
	}
	if e.eventStore.ArchiveError != "" {
		if e.eventStore.LastError == "" {
			e.eventStore.LastError = e.eventStore.ArchiveError
		} else if e.eventStore.LastError != e.eventStore.ArchiveError {
			e.eventStore.LastError = stateText(e.eventStore.LastError+"; "+e.eventStore.ArchiveError, 1024)
		}
	}
	if e.eventStore.RetentionError != "" {
		if e.eventStore.LastError == "" {
			e.eventStore.LastError = e.eventStore.RetentionError
		} else if e.eventStore.LastError != e.eventStore.RetentionError {
			e.eventStore.LastError = stateText(e.eventStore.LastError+"; "+e.eventStore.RetentionError, 1024)
		}
	}
	e.state.EventStoreError = e.eventStore.LastError
	return err
}

type eventStoreWork uint8

const (
	compactEventWork eventStoreWork = iota
	archiveEventWork
	inspectEventWork
	retainEventWork
)

// One bounded operation runs per tick. Reclamation, compression and inspection
// are independent work; one operation must not consume the next one's deadline.
func (e *Engine) maintainEventStore(ctx context.Context, work eventStoreWork) error {
	if work == retainEventWork {
		return e.retainEvents(ctx)
	}
	if work == inspectEventWork {
		return e.RefreshEventStore(ctx)
	}
	e.mu.Lock()
	journal := e.journal
	e.mu.Unlock()
	if journal == nil {
		return nil
	}
	var err error
	if work == compactEventWork {
		if err = e.retireEventGroupPeers(ctx); err == nil {
			_, err = journal.CompactCompleted(ctx, 64)
		}
	} else {
		_, err = journal.ArchiveEvents(ctx, 16)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal != journal {
		return err
	}
	message := ""
	if err != nil {
		operation := "event archive"
		if work == compactEventWork {
			operation = "completed work compaction"
		}
		message = stateText(operation+": "+err.Error(), 1024)
		// Report a failed write immediately. Inspection will combine all
		// current alarms; only the corresponding successful work clears one.
		e.state.EventStoreError = message
		e.eventStore.LastError = message
	}
	if work == compactEventWork {
		e.eventStore.CompactionError = message
	} else {
		e.eventStore.ArchiveError = message
	}
	return err
}

func (e *Engine) eventStoreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/event-store/format", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			localapi.Fail(w, 400, fmt.Errorf("event format accepts no query parameters"))
			return
		}
		e.mu.Lock()
		journal := e.journal
		e.mu.Unlock()
		if journal == nil {
			localapi.Fail(w, 503, fmt.Errorf("durable event journal is unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		format, err := journal.InspectFormat(ctx)
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		e.mu.Lock()
		same := e.journal == journal
		e.mu.Unlock()
		if !same {
			localapi.Fail(w, 503, fmt.Errorf("event store changed during inspection"))
			return
		}
		localapi.JSON(w, 200, format)
	})
	mux.HandleFunc("GET /v1/event-store", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			localapi.Fail(w, 400, fmt.Errorf("event-store accepts no query parameters"))
			return
		}
		e.mu.Lock()
		available := e.journal != nil
		e.mu.Unlock()
		if !available {
			localapi.Fail(w, 503, fmt.Errorf("durable event journal is unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		err := e.RefreshEventStore(ctx)
		e.mu.Lock()
		status := e.eventStore
		e.mu.Unlock()
		code := http.StatusOK
		if err != nil {
			code = http.StatusServiceUnavailable
		}
		localapi.JSON(w, code, status)
	})
}

func (e *Engine) monitorEventStore(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	previous := ""
	work := compactEventWork
	for {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.maintainEventStore(call, work)
		cancel()
		work = (work + 1) % 4
		e.mu.Lock()
		status := e.eventStore
		e.mu.Unlock()
		pressure := "unknown"
		if status.Capacity != nil {
			pressure = status.Capacity.Pressure
		}
		message := status.LastError
		if err != nil {
			message = err.Error()
		}
		key := pressure + ":" + message
		if ctx.Err() == nil && key != previous {
			if message != "" || pressure != "ok" {
				slog.Warn("durable event storage pressure", "pressure", pressure, "error", message, "capacity", status.Capacity)
			} else if previous != "" {
				slog.Info("durable event storage pressure recovered")
			}
			previous = key
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
