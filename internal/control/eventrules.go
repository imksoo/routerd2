package control

import (
	"context"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/eventcontroller"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// Caller holds the configuration authority lock. Historical rule IO is bounded
// by the caller's deadline; source commit and output publication cannot race.
func (e *Engine) syncEventRulesLocked(ctx context.Context) (err error) {
	defer func() {
		e.state.EventRuleError = ""
		if err != nil {
			e.state.EventRuleError = stateText(err.Error(), 1024)
		}
	}()
	if e.journal == nil {
		for _, resource := range e.state.Config.Resources {
			if resource.EventRule != nil {
				return errors.New("event rules require the durable event journal")
			}
		}
		return nil
	}
	if e.rules == nil {
		e.rules = eventcontroller.New(e.journal)
	}
	var active []resourceprogram.EventRule
	if e.state.Healthy {
		if e.runtime != nil {
			active, err = e.runtime.ActiveEventRules(e.state.Config, e.state.Revision)
		} else {
			var p resourceprogram.Plan
			p, _, err = resourceprogram.Project(e.state.Config, resourceprogram.FactSnapshot{})
			active = p.Native.EventRules
		}
		if err != nil {
			return err
		}
	}
	return e.rules.Sync(ctx, e.state.Revision, active)
}

func (e *Engine) committedRuleLifecycleLocked(ctx context.Context) {
	e.pruneProtocolErrorsLocked()
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = e.syncEventRulesLocked(bounded) // authoritative config is already saved
	_, _ = e.syncDerivedLocked(bounded)
}

func (e *Engine) ProcessEventRules(ctx context.Context, now time.Time) (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err = e.syncEventRulesLocked(ctx); err != nil {
		return err
	}
	if e.rules != nil {
		err = e.rules.Step(ctx, now)
	}
	if err != nil {
		e.state.EventRuleError = stateText(err.Error(), 1024)
	}
	return err
}
