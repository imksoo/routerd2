package serviceruntime

import (
	"context"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcp6server"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *effectsClient) runDHCP6Server(ctx context.Context, instance serviceconfig.Instance, digest string, ready func()) error {
	var cfg dhcp6serverconfig.Config
	if err := decode(instance.Config, &cfg); err != nil {
		return err
	}
	path := "/v1/services/" + instance.Name + "/dhcp6-server-input"
	var initial serviceconfig.DHCP6ServerInput
	if err := c.client.Do(ctx, "GET", path, nil, &initial); err != nil {
		return err
	}
	if initial.ConfigDigest != digest {
		return errors.New("DHCPv6 server input belongs to another configuration")
	}
	runCtx, cancel := context.WithCancel(ctx)
	updates := make(chan dhcp6serverconfig.Input, 1)
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
			call, stop := context.WithTimeout(runCtx, 2*time.Second)
			var view serviceconfig.DHCP6ServerInput
			err := c.client.Do(call, "GET", path, nil, &view)
			stop()
			if err != nil {
				continue
			}
			if view.ConfigDigest != digest {
				return
			}
			select {
			case updates <- view.Options:
			case <-runCtx.Done():
				return
			}
		}
	}()
	return dhcp6server.Run(runCtx, cfg, initial.Options, updates, func(ctx context.Context, s dhcp6serverconfig.Snapshot) error { return c.update(ctx, s) }, ready)
}
