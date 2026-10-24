package runtimeplan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// WithStoppedHealthRelays fences conditional reactivation through historical
// recovery without renewing any fact. The caller also holds source authority.
// Reconciliation takes precedence: a busy operation defers this pass. The
// callback holds op, never mu, so current fact acceptance remains available.
// The queue's exclusive store lock is independently mandatory.
func (r *Runtime) WithStoppedHealthRelays(ctx context.Context, names []string, drain func(map[string]bool) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.op.TryLock() {
		return nil
	}
	defer r.op.Unlock()
	status, err := r.manager.Status(ctx)
	if err != nil {
		return fmt.Errorf("observe stopped health workers: %w", err)
	}
	r.mu.Lock()
	if !r.configured || status.Stopping {
		r.mu.Unlock()
		return ErrStale
	}
	blocked := map[string]bool{}
	for name, binding := range r.bindings {
		if binding.instance.Role == "health" {
			blocked[name] = true
		}
	}
	for _, module := range status.Modules {
		if module.Role == "health" {
			blocked[module.Name] = true
		}
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = !blocked["service."+name]
	}
	r.mu.Unlock()
	return drain(allowed)
}

// WithProtocolProducer fences historical delivery to a currently registered,
// supervisor-ready worker. Its observations may predate this producer, but
// they neither register a producer nor renew a current protocol fact. The
// callback is bounded and serialized with runtime/source cutover.
func (r *Runtime) WithProtocolProducer(ctx context.Context, u serviceconfig.Update, appendHistory func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	status, err := r.manager.Status(ctx)
	if err != nil {
		return fmt.Errorf("observe protocol producer: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, exists := r.bindings[u.Name]
	if !r.configured || !exists || status.Stopping || u.SupervisorInstance != r.supervisorInstance || u.Role != "health" || b.instance.Role != u.Role || b.digest != u.ConfigDigest {
		return ErrStale
	}
	record, registered := r.records[u.Name]
	if !registered || record.ConfigDigest != u.ConfigDigest || record.Generation != u.Generation || record.ProducerEpoch != u.ProducerEpoch {
		return ErrStale
	}
	current := false
	for _, module := range status.Modules {
		if module.Name == u.Name && module.Ready && module.Generation == u.Generation && module.ConfigDigest == u.ConfigDigest {
			current = true
			break
		}
	}
	if !current {
		return ErrStale
	}
	var cfg health.Config
	if err = json.Unmarshal(b.instance.Config, &cfg); err != nil {
		return err
	}
	return appendHistory(cfg.Resource)
}
