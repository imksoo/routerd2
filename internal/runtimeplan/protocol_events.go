package runtimeplan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// RetiredHealthRelays observes stopped ownership without renewing any fact.
// The control caller holds its source-authority lock through recovery. Full
// source absence prevents a conditional reconcile from starting these health
// workers; the queue's exclusive store lock remains independently mandatory.
// Historical disk IO must not hold the runtime mutex or delay current facts.
func (r *Runtime) RetiredHealthRelays(ctx context.Context, names []string) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	status, err := r.manager.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("observe retired health workers: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured || status.Stopping {
		return nil, ErrStale
	}
	blocked := map[string]bool{}
	for _, service := range r.current.Services {
		if service.Health != nil {
			blocked[service.Name] = true
		}
	}
	for _, module := range status.Modules {
		if module.Role == "health" {
			blocked[module.Name] = true
		}
	}
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = !blocked[name] && !blocked["service."+name]
	}
	return allowed, nil
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
