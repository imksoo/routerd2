package serviceruntime

import (
	"context"
	"errors"

	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *effectsClient) runAFTR(ctx context.Context, instance serviceconfig.Instance, digest string, ready func()) error {
	var private serviceconfig.DSLiteInstance
	if err := decode(instance.Config, &private); err != nil {
		return err
	}
	cfg := private.Protocol
	cfg.ConfigDigest = digest
	path := "/v1/services/" + instance.Name + "/aftr-input"
	input := func(ctx context.Context) (aftr.Input, error) {
		var in aftr.Input
		err := c.client.Do(ctx, "GET", path, nil, &in)
		return in, err
	}
	err := aftr.Run(ctx, cfg, input, aftr.EffectsFunc(func(ctx context.Context, s aftr.Snapshot) error { return c.update(ctx, s) }), ready)
	if err == nil {
		return errors.New("AFTR worker stopped without context cancellation")
	}
	return err
}
