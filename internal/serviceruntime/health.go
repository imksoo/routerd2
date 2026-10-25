package serviceruntime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocolevents"
)

func (c *effectsClient) runHealth(ctx context.Context, dir, eventDir string, cfg health.Config, ready func()) error {
	store, err := events.Open(events.Options{Directory: eventDir})
	if err != nil {
		return err
	}
	defer store.Close()
	outbox := protocolevents.NewOutbox(store, cfg.Resource)
	relayCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.replayProtocolEvents(relayCtx, cfg.Resource, outbox) }()
	defer func() { cancel(); <-done }()
	networkClient := localapi.NewClient(filepath.Join(dir, "network.sock"))
	probe := func(ctx context.Context, cfg health.Config) old.ProbeResult {
		result := health.Probe(ctx, cfg)
		// Evidence is best effort, and cannot turn a successful scoped probe into
		// failure. The network worker alone performs the Netlink route lookup.
		evidenceCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer cancel()
		var route network.RouteLookupResult
		req := network.RouteLookupRequest{Target: cfg.Target, Interface: cfg.Interface, Source: cfg.SourceAddress, Mark: cfg.Mark}
		if networkClient.Do(evidenceCtx, http.MethodPost, "/v1/routes/lookup", req, &route) == nil {
			result.OutInterface = route.Interface
			result.NextHop = route.Gateway
			result.RouteSource = "netlink"
			if result.SourceAddress == "" {
				result.SourceAddress = route.Source
			}
		}
		return result
	}
	return health.RunWithProbe(ctx, cfg, func(ctx context.Context, s health.Snapshot) error {
		if s.ProbeEvent != nil {
			c.mu.Lock()
			identity := c.identity
			c.mu.Unlock()
			event := *s.ProbeEvent
			event.Attributes["routerd.producer.config_digest"] = identity.ConfigDigest
			event.Attributes["routerd.producer.epoch"] = identity.ProducerEpoch
			event.Attributes["routerd.producer.generation"] = strconv.FormatUint(identity.Generation, 10)
			event.Attributes["routerd.producer.supervisor"] = identity.SupervisorInstance
			event.Attributes["routerd.producer.boot_id"] = s.BootID
			if err := outbox.Enqueue(ctx, event); err != nil {
				return err
			}
		}
		return c.update(ctx, s)
	}, ready, probe)
}

func (c *effectsClient) sendProtocolEvents(ctx context.Context, resource string, page []events.Event) error {
	if len(page) == 0 || len(page) > protocolevents.PageSize {
		return errors.New("invalid bounded protocol publication page")
	}
	call, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var ack protocolevents.Acknowledgement
	if err := c.client.Do(call, http.MethodPost, "/v1/protocol-events", protocolevents.Batch{Resource: resource, Events: page}, &ack); err != nil {
		return err
	}
	if len(ack.IDs) != len(page) {
		return errors.New("protocol publication acknowledgement is incomplete")
	}
	for i, event := range page {
		if ack.IDs[i] != event.ID {
			return errors.New("protocol publication acknowledgement changed event identity")
		}
	}
	return nil
}

func (c *effectsClient) replayProtocolEvents(ctx context.Context, resource string, outbox *protocolevents.Outbox) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		err := outbox.Flush(ctx, func(ctx context.Context, page []events.Event) error {
			return c.sendProtocolEvents(ctx, resource, page)
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if err.Error() != lastError {
				slog.Error("durable protocol publication pending", "error", err)
			}
			lastError = err.Error()
		} else if lastError != "" {
			slog.Info("durable protocol publication recovered")
			lastError = ""
		}
		timer.Reset(200 * time.Millisecond)
	}
}
