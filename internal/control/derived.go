package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func (e *Engine) derivedInputsLocked() ([]resourceprogram.DerivedEvent, map[string]derived.Snapshot, error) {
	if !e.state.Healthy {
		return nil, nil, nil
	}
	if e.runtime != nil {
		return e.runtime.DerivedInputs(e.state.Revision)
	}
	// In-process engines have no protocol producer registry. Only literals can
	// be known; a declaration never fabricates a live health observation.
	native, _ := e.plan.Project(resourceprogram.FactSnapshot{})
	clock, err := bfd.ReadClock()
	if err != nil {
		return nil, nil, err
	}
	body, _ := json.Marshal([]any{e.state.Config, e.state.Revision, e.state.ProducerEpoch, clock.BootID})
	sum := sha256.Sum256(body)
	scope := hex.EncodeToString(sum[:])
	snapshot, err := derived.NewSnapshot(scope, scope, uint64(clock.BoottimeNanoseconds), time.Now(), nil)
	if err != nil {
		return nil, nil, err
	}
	views := map[string]derived.Snapshot{}
	for _, declaration := range native.DerivedEvents {
		views[declaration.ID] = snapshot
	}
	return native.DerivedEvents, views, nil
}

func (e *Engine) syncDerivedLocked(ctx context.Context) (snapshots map[string]derived.Snapshot, err error) {
	defer func() {
		e.state.DerivedError = ""
		if err != nil {
			e.state.DerivedError = stateText(err.Error(), 1024)
		}
	}()
	if e.journal == nil {
		for _, resource := range e.state.Config.Resources {
			if resource.DerivedEvent != nil {
				return nil, errors.New("derived events require the durable journal")
			}
		}
		return nil, nil
	}
	hasDerived := false
	for _, resource := range e.state.Config.Resources {
		if resource.DerivedEvent != nil {
			hasDerived = true
			break
		}
	}
	// An unused component performs no registry IO. An existing controller must
	// still durably fence removal; after restart, a missed revision gap fences
	// any older registry before a later reactivation.
	if !hasDerived && e.derived == nil {
		return nil, nil
	}
	if e.derived == nil {
		e.derived = derivedcontroller.New(e.journal)
	}
	if !hasDerived {
		return nil, e.derived.Sync(ctx, e.state.Revision, nil)
	}
	active, snapshots, err := e.derivedInputsLocked()
	if err != nil {
		return nil, err
	}
	return snapshots, e.derived.Sync(ctx, e.state.Revision, active)
}

func (e *Engine) ProcessDerivedEvents(ctx context.Context) (err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshots, err := e.syncDerivedLocked(ctx)
	if err != nil {
		return err
	}
	if e.derived != nil {
		err = e.derived.Step(ctx, snapshots)
	}
	if err != nil {
		e.state.DerivedError = stateText(err.Error(), 1024)
	}
	return err
}

func (e *Engine) derivedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/derived/{id...}", func(w http.ResponseWriter, request *http.Request) {
		id := request.PathValue("id")
		e.mu.Lock()
		defer e.mu.Unlock()
		found := false
		for _, resource := range e.state.Config.Resources {
			if resource.ID == id && resource.DerivedEvent != nil {
				found = true
				break
			}
		}
		if !found {
			localapi.Fail(w, 404, errors.New("derived resource not found"))
			return
		}
		if e.state.DerivedError != "" {
			localapi.Fail(w, 503, errors.New(e.state.DerivedError))
			return
		}
		active, snapshots, err := e.derivedInputsLocked()
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		selected := false
		for _, resource := range active {
			if resource.ID == id {
				selected = true
				break
			}
		}
		if !selected {
			localapi.JSON(w, 200, derivedcontroller.Observation{ID: id, State: derived.Status{Phase: "Inactive", Inputs: derived.Indeterminate, PendingTransition: "None"}})
			return
		}
		if e.derived != nil {
			if view, exists := e.derived.View(id, snapshots[id]); exists {
				localapi.JSON(w, 200, view)
				return
			}
		}
		localapi.JSON(w, 200, derivedcontroller.Observation{ID: id, State: derived.Status{Phase: "Indeterminate", Inputs: derived.Indeterminate, PendingTransition: "None"}})
	})
}
