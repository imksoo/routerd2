package runtimeplan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

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
