package serviceruntime

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgp"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *effectsClient) runBGP(ctx context.Context, instance serviceconfig.Instance, digest string, ready func()) error {
	var cfg bgp.Config
	if err := decode(instance.Config, &cfg); err != nil {
		return err
	}
	effects := bgp.EffectsFunc(func(ctx context.Context, s bgp.Snapshot) error { return c.update(ctx, s) })
	if len(cfg.PeerBFD) == 0 {
		return bgp.Run(ctx, cfg, effects, ready)
	}
	runCtx, cancel := context.WithCancel(ctx)
	updates, done := make(chan bgp.BFDUpdate, 1), make(chan struct{})
	generation := c.identity.Generation
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		defer close(updates)
		c.watchBFD(runCtx, instance.Name, digest, generation, updates)
	}()
	// The first status publication establishes this recipient generation at the
	// controller. The watcher may see 409 until then; peers begin AdminDown, so
	// startup never waits for a health projection or opens an ungated session.
	return bgp.RunWithBFD(runCtx, cfg, digest, generation, updates, effects, ready)
}

func (c *effectsClient) watchBFD(ctx context.Context, name, digest string, generation uint64, updates chan bgp.BFDUpdate) {
	query := url.Values{"config_digest": {digest}, "generation": {strconv.FormatUint(generation, 10)}}
	path := "/v1/services/" + name + "/bfd?" + query.Encode()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var failures uint64
	for ctx.Err() == nil {
		var projection bgpconfig.BFDProjection
		call, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err := c.client.Do(call, "GET", path, nil, &projection)
		cancel()
		if ctx.Err() != nil {
			return
		}
		clock, clockErr := bfd.ReadClock()
		if clockErr != nil {
			return
		}
		if err != nil {
			if failures == math.MaxUint64 {
				return
			}
			failures++
		}
		update := bgp.BFDUpdate{Projection: projection, Received: clock, Available: err == nil, Failures: failures}
		select {
		case updates <- update:
		default:
			select {
			case <-updates:
			default:
			}
			select {
			case updates <- update:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
