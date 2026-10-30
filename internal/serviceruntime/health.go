package serviceruntime

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/telemetry"
)

func (c *effectsClient) runHealth(ctx context.Context, dir, eventDir string, cfg health.Config, ready func()) error {
	history := make(chan daemonapi.DaemonEvent, protocolevents.PageSize)
	var status atomic.Pointer[healthHistoryStatus]
	relayCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.runHealthHistory(relayCtx, eventDir, cfg.Resource, history, &status) }()
	defer func() { cancel(); <-done }()

	networkClient := localapi.NewClient(filepath.Join(dir, "network.sock"))
	var probeStarted, probeFinished time.Time
	probe := func(ctx context.Context, cfg health.Config) old.ProbeResult {
		probeStarted = time.Now()
		result := health.Probe(ctx, cfg)
		probeFinished = time.Now()
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
	var dropped uint64
	return health.RunWithProbe(ctx, cfg, func(ctx context.Context, s health.Snapshot) error {
		if s.ProbeEvent != nil {
			// The existing log handler only queues: receiver/disk failure cannot
			// delay a probe or change the authority carried by its observation.
			telemetry.HealthProbe{Resource: cfg.Resource, Interface: cfg.Interface, Target: cfg.Target, Protocol: cfg.Protocol, Result: s.State.LastResult, Phase: s.State.Phase, StartedAt: probeStarted, FinishedAt: probeFinished}.Record(ctx)
			c.mu.Lock()
			identity := c.identity
			c.mu.Unlock()
			event := *s.ProbeEvent
			event.Attributes["routerd.producer.config_digest"] = identity.ConfigDigest
			event.Attributes["routerd.producer.epoch"] = identity.ProducerEpoch
			event.Attributes["routerd.producer.generation"] = strconv.FormatUint(identity.Generation, 10)
			event.Attributes["routerd.producer.supervisor"] = identity.SupervisorInstance
			event.Attributes["routerd.producer.boot_id"] = s.BootID
			select {
			case history <- event:
			default:
				dropped++
				s.HistoryError = "history recorder busy"
			}
		}
		s.HistoryDropped = dropped
		if current := status.Load(); current != nil {
			s.HistoryDropped += current.dropped
			if s.HistoryError == "" {
				s.HistoryError = current.err
			}
		} else if s.HistoryError == "" {
			s.HistoryError = "history recorder starting"
		}
		return c.update(ctx, s)
	}, ready, probe)
}

func (c *effectsClient) sendProtocolEvents(ctx context.Context, resource string, page []events.Event) error {
	var ack protocolevents.Acknowledgement
	if err := c.client.Do(ctx, http.MethodPost, "/v1/protocol-events", protocolevents.Batch{Resource: resource, Events: page}, &ack); err != nil {
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

// The history recorder owns its store and delivery. It never blocks the probe
// loop: unrecorded events are counted for this producer lifetime, while events
// already committed to the outbox retain their original IDs until delivery.
type healthHistoryStatus struct {
	dropped uint64
	err     string
}

func (c *effectsClient) runHealthHistory(ctx context.Context, eventDir, resource string, input <-chan daemonapi.DaemonEvent, status *atomic.Pointer[healthHistoryStatus]) {
	var store *events.Store
	var outbox *protocolevents.Outbox
	defer func() {
		if store != nil {
			_ = store.Close()
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var dropped uint64
	lastError := ""
	report := func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
			if len(message) > 768 {
				message = message[:768]
			}
		}
		status.Store(&healthHistoryStatus{dropped: dropped, err: message})
		if message != lastError {
			if err != nil {
				slog.Error("health history pending or incomplete", "error", err, "dropped", dropped)
			} else {
				slog.Info("health history delivery recovered", "dropped", dropped)
			}
			lastError = message
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-input:
			if outbox == nil {
				dropped++
				report(errors.New("health history store unavailable"))
				continue
			}
			if err := outbox.Enqueue(ctx, event); err != nil {
				dropped++
				report(err)
			}
		case <-timer.C:
			if store == nil {
				var err error
				store, err = events.Open(events.Options{Directory: eventDir})
				if err != nil {
					report(err)
					timer.Reset(200 * time.Millisecond)
					continue
				}
				outbox = protocolevents.NewOutbox(store, resource)
			}
			err := outbox.Flush(ctx, func(ctx context.Context, page []events.Event) error {
				return c.sendProtocolEvents(ctx, resource, page)
			})
			if ctx.Err() != nil {
				return
			}
			report(err)
			timer.Reset(200 * time.Millisecond)
		}
	}
}
