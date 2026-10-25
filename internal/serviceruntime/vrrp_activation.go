package serviceruntime

import (
	"context"
	"encoding/json"

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
