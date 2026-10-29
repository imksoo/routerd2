package serviceruntime

import (
	"context"
	"os"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/ntp"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *effectsClient) runNTP(ctx context.Context, instance serviceconfig.Instance, ready func()) error {
	var cfg ntpconfig.Config
	if err := decode(instance.Config, &cfg); err != nil {
		return err
	}
	engine := os.NewFile(5, "chronyd-engine")
	defer engine.Close()
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	var updates chan ntpconfig.Sources
	if len(cfg.SourcesFrom) > 0 {
		updates = make(chan ntpconfig.Sources, 1)
		done := make(chan struct{})
		defer func() { cancel(); <-done }()
		go func() { defer close(done); c.watchNTP(life, instance.Name, updates) }()
	}
	return ntp.Run(life, cfg, engine, updates, func(ctx context.Context, snapshot ntpconfig.Snapshot) error { return c.update(ctx, snapshot) }, ready)
}

func (c *effectsClient) watchNTP(ctx context.Context, name string, updates chan ntpconfig.Sources) {
	defer close(updates)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var sources ntpconfig.Sources
		call, stop := context.WithTimeout(ctx, 2*time.Second)
		err := c.client.Do(call, "GET", "/v1/services/"+name+"/ntp-sources", nil, &sources)
		stop()
		if err == nil {
			if sources.ConfigDigest != c.identity.ConfigDigest || sources.Generation != c.identity.Generation {
				return
			}
			select {
			case updates <- sources:
			default:
				select {
				case <-updates:
				default:
				}
				select {
				case updates <- sources:
				case <-ctx.Done():
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
