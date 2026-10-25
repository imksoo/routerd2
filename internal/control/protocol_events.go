package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocolevents"
)

const protocolRetirementError = "retired-health-relays"

func healthRelayNames(source config.Document) []string {
	names := []string{}
	for _, r := range source.Resources {
		if r.Service != nil && r.Service.Health != nil {
			names = append(names, r.Service.Name)
		}
	}
	return names
}

func (e *Engine) registerProtocolRelaysLocked(ctx context.Context, source config.Document) error {
	names := healthRelayNames(source)
	// Removing health never needs a new registry write before withdrawal.
	// Recovery will independently load retained registrations after apply.
	if len(names) == 0 {
		return nil
	}
	if e.journal == nil {
		return errors.New("health relays require the durable journal")
	}
	if e.protocolRelays == nil {
		e.protocolRelays = protocolevents.NewRetirement(e.journal, e.store.Dir)
	}
	return e.protocolRelays.Register(ctx, names)
}

func (e *Engine) declareProtocolRelaysLocked() {
	if e.protocolRelays == nil {
		return
	}
	e.protocolRelays.Declare(healthRelayNames(e.state.Config))
	e.updateRetirementHealthLocked(nil)
}

func (e *Engine) updateRetirementHealthLocked(err error) {
	if err == nil && e.protocolRelays != nil {
		status := e.protocolRelays.Status()
		if status.Retiring+status.StoppedPending > 0 {
			err = fmt.Errorf("%d retired and %d stopped health relays awaiting durable recovery", status.Retiring, status.StoppedPending)
		}
	}
	e.setProtocolErrorLocked(protocolRetirementError, err)
}

func (e *Engine) ProcessRetiredProtocolEvents(ctx context.Context) error {
	e.mu.Lock()
	if e.journal == nil || e.runtime == nil {
		e.mu.Unlock()
		return nil
	}
	if e.protocolRelays == nil {
		e.protocolRelays = protocolevents.NewRetirement(e.journal, e.store.Dir)
	}
	relays, runtime := e.protocolRelays, e.runtime
	names, err := relays.RegisteredNames(ctx)
	if err != nil || len(names) == 0 {
		e.updateRetirementHealthLocked(err)
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()
	// Waiting for runtime ownership must not block normal protocol publication
	// on source authority. ApplyConfig takes source -> runtime, so the callback
	// must try source authority and defer if busy, never invert that lock order.
	processed := false
	err = runtime.WithStoppedHealthRelays(ctx, names, func(allowed map[string]bool) error {
		if !e.mu.TryLock() {
			return nil
		}
		defer e.mu.Unlock()
		if e.runtime != runtime || e.protocolRelays != relays {
			return nil
		}
		processed = true
		err := relays.Recover(ctx, healthRelayNames(e.state.Config), func(_ context.Context, _ []string, drain func(map[string]bool) error) error {
			return drain(allowed)
		})
		for _, name := range relays.Recovered() {
			e.setProtocolErrorLocked("service."+name, nil)
		}
		e.updateRetirementHealthLocked(err)
		return err
	})
	if err != nil && !processed {
		e.mu.Lock()
		if e.runtime == runtime && e.protocolRelays == relays {
			e.updateRetirementHealthLocked(err)
		}
		e.mu.Unlock()
	}
	return err
}

func (e *Engine) setProtocolErrorLocked(name string, err error) {
	if e.protocolErrors == nil {
		e.protocolErrors = map[string]string{}
	}
	if err == nil {
		delete(e.protocolErrors, name)
	} else {
		e.protocolErrors[name] = stateText(err.Error(), 768)
	}
	e.state.ProtocolEventError = ""
	if len(e.protocolErrors) > 0 {
		names := make([]string, 0, len(e.protocolErrors))
		for name := range e.protocolErrors {
			names = append(names, name)
		}
		sort.Strings(names)
		e.state.ProtocolEventError = stateText(fmt.Sprintf("%d protocol producers pending delivery recovery; %s: %s", len(names), names[0], e.protocolErrors[names[0]]), 1024)
	}
}

func (e *Engine) pruneProtocolErrorsLocked() {
	if len(e.protocolErrors) == 0 {
		return
	}
	declared := map[string]bool{}
	for _, r := range e.state.Config.Resources {
		if r.Service != nil && r.Service.Health != nil {
			declared[r.Service.ModuleName()] = true
		}
	}
	for name := range e.protocolErrors {
		if name != protocolRetirementError && !declared[name] {
			e.setProtocolErrorLocked(name, nil)
		}
	}
}

func (e *Engine) protocolEventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/protocol-events", func(w http.ResponseWriter, r *http.Request) {
		var batch protocolevents.Batch
		if err := localapi.Decode(w, r, &batch); err != nil {
			localapi.Fail(w, 400, err)
			return
		}

		e.mu.Lock()
		defer e.mu.Unlock()
		if e.protocolRelays == nil {
			localapi.Fail(w, 503, errors.New("protocol journal unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		ack, err := e.protocolRelays.Deliver(ctx, batch)
		if err == nil || !errors.Is(err, protocolevents.ErrInvalidBatch) && !errors.Is(err, protocolevents.ErrUnregistered) {
			e.setProtocolErrorLocked("service."+batch.Resource, err)
		}
		if err != nil {
			code := http.StatusServiceUnavailable
			if errors.Is(err, protocolevents.ErrUnregistered) {
				code = http.StatusConflict
			} else if errors.Is(err, protocolevents.ErrInvalidBatch) {
				code = http.StatusUnprocessableEntity
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, ack)
	})
}
