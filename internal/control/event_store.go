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
	Capacity   *events.Capacity `json:"capacity,omitempty"`
	ObservedAt time.Time        `json:"observed_at,omitempty"`
	LastError  string           `json:"last_error,omitempty"`
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
		e.eventStore = EventStoreHealth{Capacity: &capacity, ObservedAt: time.Now().UTC()}
		if capacity.Pressure == "exhausted" {
			e.eventStore.LastError = "event store capacity exhausted: " + strings.Join(capacity.Reasons, ",")
		}
	}
	e.state.EventStoreError = e.eventStore.LastError
	return err
}

func (e *Engine) eventStoreRoutes(mux *http.ServeMux) {
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
	for {
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := e.RefreshEventStore(call)
		cancel()
		e.mu.Lock()
		status := e.eventStore
		e.mu.Unlock()
		pressure := "unknown"
		if status.Capacity != nil {
			pressure = status.Capacity.Pressure
		}
		key := pressure + ":" + status.LastError
		if ctx.Err() == nil && key != previous {
			if err != nil || pressure != "ok" {
				slog.Warn("durable event storage pressure", "pressure", pressure, "error", status.LastError, "capacity", status.Capacity)
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
