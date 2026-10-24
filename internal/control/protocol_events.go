package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
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
	if err == nil && e.protocolRelays != nil && e.protocolRelays.Status().Retiring > 0 {
		err = fmt.Errorf("%d retired health relays awaiting durable recovery", e.protocolRelays.Status().Retiring)
	}
	e.setProtocolErrorLocked(protocolRetirementError, err)
}

func (e *Engine) ProcessRetiredProtocolEvents(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal == nil || e.runtime == nil {
		return nil
	}
	if e.protocolRelays == nil {
		e.protocolRelays = protocolevents.NewRetirement(e.journal, e.store.Dir)
	}
	err := e.protocolRelays.Recover(ctx, healthRelayNames(e.state.Config), func(ctx context.Context, names []string, drain func(map[string]bool) error) error {
		allowed, err := e.runtime.RetiredHealthRelays(ctx, names)
		if err != nil {
			return err
		}
		return drain(allowed)
	})
	e.updateRetirementHealthLocked(err)
	return err
}

func validateProtocolPage(name string, page []events.Event) error {
	if len(page) == 0 || len(page) > protocolevents.PageSize {
		return errors.New("protocol publication requires 1..16 observations")
	}
	seen := map[string]bool{}
	for _, event := range page {
		if err := protocolevents.ValidateHealth(name, event); err != nil {
			return err
		}
		if seen[event.ID] {
			return errors.New("duplicate observation in protocol page")
		}
		seen[event.ID] = true
	}
	return nil
}

func appendProtocolBatch(ctx context.Context, journal *events.Store, name string, page []events.Event) (protocolevents.Acknowledgement, error) {
	ack := protocolevents.Acknowledgement{}
	if err := validateProtocolPage(name, page); err != nil {
		return ack, err
	}
	if journal == nil {
		return ack, errors.New("durable protocol journal unavailable")
	}
	for _, event := range page {
		if _, err := journal.Append(ctx, event); err != nil {
			return protocolevents.Acknowledgement{}, err
		}
		ack.IDs = append(ack.IDs, event.ID)
	}
	return ack, nil
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
		u := batch.Producer
		if !serviceconfig.ValidSupervisorInstance(u.SupervisorInstance) || !serviceconfig.ValidSupervisorInstance(u.ProducerEpoch) || u.Role != "health" || u.Generation == 0 || u.Sequence != 0 || len(u.Snapshot) != 0 || len(u.ConfigDigest) != 64 || !strings.HasPrefix(u.Name, "service.") {
			localapi.Fail(w, 422, errors.New("invalid current protocol producer envelope"))
			return
		}
		if err := validateProtocolPage(strings.TrimPrefix(u.Name, "service."), batch.Events); err != nil {
			localapi.Fail(w, 422, err)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.runtime == nil {
			localapi.Fail(w, 503, errors.New("protocol runtime unavailable"))
			return
		}
		var ack protocolevents.Acknowledgement
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		err := e.runtime.WithProtocolProducer(ctx, u, func(name string) error {
			var err error
			ack, err = appendProtocolBatch(ctx, e.journal, name, batch.Events)
			e.setProtocolErrorLocked(u.Name, err)
			return err
		})
		if err != nil {
			code := http.StatusServiceUnavailable
			if errors.Is(err, runtimeplan.ErrStale) {
				code = http.StatusConflict
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, ack)
	})
}
