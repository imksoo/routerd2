package serviceruntime

import (
	"context"
	"errors"

	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

type vrrpEffects struct{ client *effectsClient }

func (e vrrpEffects) Update(ctx context.Context, s vrrp.Snapshot) error {
	return e.client.update(ctx, s)
}
func (e vrrpEffects) GrantActivation(ctx context.Context, s vrrp.Snapshot) (vrrp.ActivationGrant, error) {
	var grant vrrp.ActivationGrant
	c := e.client
	c.mu.Lock()
	defer c.mu.Unlock()
	var previous vrrp.Snapshot
	if c.identity.Role != "vrrp" || decode(c.latest, &previous) != nil || previous.Active || previous.BootID != s.BootID || previous.Epoch != s.Epoch || previous.Transition != s.Transition || previous.ObservedNanos != s.ObservedNanos {
		return grant, errors.New("VRRP activation request has no matching acknowledged election")
	}
	request := c.identity
	request.Snapshot = append(request.Snapshot[:0:0], c.latest...)
	err := c.client.Do(ctx, "POST", "/v1/services/vrrp-activation", request, &grant)
	return grant, err
}
