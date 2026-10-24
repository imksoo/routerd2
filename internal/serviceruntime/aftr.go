package serviceruntime

import (
	"context"
	"errors"
	"time"

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
	var initial aftr.Input
	path := "/v1/services/" + instance.Name + "/aftr-input"
	if err := c.client.Do(ctx, "GET", path, nil, &initial); err != nil {
		return err
	}
	if err := aftr.ValidateInput(cfg, initial); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	updates := make(chan aftr.Input, 1)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		defer close(updates)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
			var in aftr.Input
			call, stop := context.WithTimeout(runCtx, 2*time.Second)
			err := c.client.Do(call, "GET", path, nil, &in)
			stop()
			if err != nil {
				continue
			} // absolute dependencies continue to expire
			if in.ConfigDigest != digest || in.Generation == 0 {
				return
			}
			select {
			case updates <- in:
			case <-runCtx.Done():
				return
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- in:
				case <-runCtx.Done():
					return
				}
			}
		}
	}()
	err := aftr.Run(runCtx, cfg, initial, updates, aftr.EffectsFunc(func(ctx context.Context, s aftr.Snapshot) error { return c.update(ctx, s) }), ready)
	if err == nil {
		return errors.New("AFTR worker stopped without context cancellation")
	}
	return err
}
