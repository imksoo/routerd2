package serviceruntime

import (
	"context"
	"encoding/json"

	"github.com/imksoo/routerd2/internal/protocol/conntracksync"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

type vrrpEffects struct{ client *effectsClient }

func (e vrrpEffects) Apply(ctx context.Context, s vrrp.Snapshot) (vrrp.Decision, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return vrrp.Decision{}, err
	}
	c := e.client
	c.mu.Lock()
	defer c.mu.Unlock()
	receipt, err := c.send(ctx, encoded)
	if err != nil {
		return vrrp.Decision{}, err
	}
	c.latest = encoded
	return receipt.VRRP, nil
}

// The same VRRP owner holds the replication process through its withdrawal.
// Commit before publishing elected preparation: WAN VMAC traffic can arrive
// before the LAN VIP is admitted. Demotion waits for the writer to revoke it.
type syncedVRRPEffects struct {
	effects vrrpEffects
	sync    *conntracksync.Process
}

func (e syncedVRRPEffects) Apply(ctx context.Context, s vrrp.Snapshot) (vrrp.Decision, error) {
	if s.State == vrrp.StateActive {
		if err := e.sync.Promote(ctx); err != nil {
			return vrrp.Decision{}, err
		}
	}
	decision, err := e.effects.Apply(ctx, s)
	if err != nil {
		return decision, err
	}
	if s.State != vrrp.StateActive {
		err = e.sync.Demote(ctx)
	}
	return decision, err
}
