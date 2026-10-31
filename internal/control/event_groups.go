package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/imksoo/routerd/pkg/federation"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/eventgroupconfig"
)

func (e *Engine) eventGroup(name string) (eventgroupconfig.Config, *events.Store, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.journal == nil {
		return eventgroupconfig.Config{}, nil, errors.New("event journal is unavailable")
	}
	for _, r := range e.state.Config.Resources {
		if r.Service != nil && r.Service.ModuleName() == name && r.Service.EventGroup != nil {
			return *r.Service.EventGroup, e.journal, nil
		}
	}
	return eventgroupconfig.Config{}, nil, errors.New("event group service is not configured")
}
func (e *Engine) eventGroupRoutes(mux *http.ServeMux) {
	// These are product inputs: local observers emit facts here; the transport
	// delivers authenticated remote facts to the same journal owner.
	for _, op := range []string{"emit", "receive"} {
		mux.HandleFunc("POST /v1/event-groups/{name}/"+op, func(w http.ResponseWriter, r *http.Request) {
			e.eventGroupMu.Lock()
			defer e.eventGroupMu.Unlock()
			cfg, journal, err := e.eventGroup(r.PathValue("name"))
			if err != nil {
				localapi.Fail(w, 503, err)
				return
			}
			var wire federation.Event
			if err = localapi.Decode(w, r, &wire); err != nil {
				localapi.Fail(w, 400, err)
				return
			}
			if op == "emit" {
				wire.Group, wire.SourceNode = cfg.Group, cfg.NodeName
				if wire.ObservedAt.IsZero() {
					wire.ObservedAt = time.Now().UTC()
				}
				// Register before append, so the first event cannot precede its senders.
				for _, peer := range cfg.Peers {
					if _, err = journal.Cursor(r.Context(), eventgroupconfig.Consumer(r.PathValue("name"), peer.NodeName)); err != nil {
						localapi.Fail(w, 503, err)
						return
					}
				}
			}
			if err = wire.Normalize(); err != nil || wire.Group != cfg.Group {
				localapi.Fail(w, 400, errors.New("event does not identify this configured group"))
				return
			}
			if wire.ObservedAt.IsZero() {
				wire.ObservedAt = time.Now().UTC()
			}
			record, err := journal.Append(r.Context(), events.FromFederation(wire))
			// The existing federation protocol acknowledges repeated IDs without
			// replacing the original fact, even if a retry changed its envelope.
			if errors.Is(err, events.ErrConflict) {
				localapi.JSON(w, 200, struct {
					Accepted bool `json:"accepted"`
				}{true})
				return
			}
			if err != nil {
				localapi.Fail(w, 503, err)
				return
			}
			localapi.JSON(w, 200, record)
		})
	}
	mux.HandleFunc("GET /v1/event-groups/{name}/outbox/{peer}", func(w http.ResponseWriter, r *http.Request) {
		e.eventGroupMu.Lock()
		defer e.eventGroupMu.Unlock()
		cfg, journal, err := e.eventGroup(r.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		var peer *eventgroupconfig.Peer
		for i := range cfg.Peers {
			if cfg.Peers[i].NodeName == r.PathValue("peer") {
				peer = &cfg.Peers[i]
				break
			}
		}
		if peer == nil {
			localapi.Fail(w, 404, errors.New("event peer is not configured"))
			return
		}
		consumer := eventgroupconfig.Consumer(r.PathValue("name"), peer.NodeName)
		pending, err := pendingFederation(r.Context(), journal, consumer, cfg, *peer)
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		if len(pending) > 0 {
			localapi.JSON(w, 200, eventgroupconfig.Page{Deliveries: pending})
			return
		}
		before, err := journal.Cursor(r.Context(), eventgroupconfig.Consumer(r.PathValue("name"), peer.NodeName))
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		records, err := journal.List(r.Context(), before, 64)
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		more := len(records) == 64
		for _, record := range records {
			var jobs []events.Job
			f := record.Event.Federation
			if f != nil && f.Group == cfg.Group && f.SourceNode == cfg.NodeName && !record.Event.IsExpired(time.Now()) {
				wire := events.ToFederation(record.Event)
				if eventgroupconfig.Matches(*peer, wire) {
					payload, err := json.Marshal(wire)
					if err != nil {
						localapi.Fail(w, 503, err)
						return
					}
					jobs = []events.Job{{Key: record.Event.ID, Payload: payload}}
				}
			}
			if err := journal.Schedule(r.Context(), consumer, before, record.Cursor, jobs); err != nil {
				// Already queued work must remain deliverable when new work
				// cannot fit. The existing compactor frees completed payloads.
				if errors.Is(err, events.ErrCapacity) {
					more = false
					break
				}
				localapi.Fail(w, 503, err)
				return
			}
			before = record.Cursor
		}
		pending, err = pendingFederation(r.Context(), journal, consumer, cfg, *peer)
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		page := eventgroupconfig.Page{More: more, Deliveries: pending}
		localapi.JSON(w, 200, page)
	})
	mux.HandleFunc("POST /v1/event-groups/{name}/outbox/{peer}", func(w http.ResponseWriter, r *http.Request) {
		e.eventGroupMu.Lock()
		defer e.eventGroupMu.Unlock()
		cfg, journal, err := e.eventGroup(r.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		found := false
		for _, p := range cfg.Peers {
			if p.NodeName == r.PathValue("peer") {
				found = true
				break
			}
		}
		if !found {
			localapi.Fail(w, 404, errors.New("event peer is not configured"))
			return
		}
		var ack eventgroupconfig.Ack
		if err = localapi.Decode(w, r, &ack); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		err = journal.Complete(r.Context(), eventgroupconfig.Consumer(r.PathValue("name"), r.PathValue("peer")), ack.Key)
		if err != nil {
			localapi.Fail(w, 409, err)
			return
		}
		localapi.JSON(w, 200, ack)
	})
	mux.HandleFunc("POST /v1/event-groups/{name}/prune", func(w http.ResponseWriter, r *http.Request) {
		e.eventGroupMu.Lock()
		defer e.eventGroupMu.Unlock()
		cfg, journal, err := e.eventGroup(r.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		deleted, err := journal.PruneFederation(r.Context(), cfg.Group, cfg.MaxAge, cfg.MaxEvents, time.Now())
		if err != nil {
			localapi.Fail(w, 503, err)
			return
		}
		localapi.JSON(w, 200, struct {
			Deleted int `json:"deleted"`
		}{deleted})
	})
}

// Called by the existing journal maintenance owner. A removed peer must not
// leave a permanently stalled cursor or retain undeliverable work forever.
func (e *Engine) retireEventGroupPeers(ctx context.Context) error {
	e.eventGroupMu.Lock()
	defer e.eventGroupMu.Unlock()
	e.mu.Lock()
	journal := e.journal
	var keep []string
	for _, r := range e.state.Config.Resources {
		if r.Service != nil && r.Service.EventGroup != nil {
			for _, p := range r.Service.EventGroup.Peers {
				keep = append(keep, eventgroupconfig.Consumer(r.Service.ModuleName(), p.NodeName))
			}
		}
	}
	e.mu.Unlock()
	if journal == nil {
		return nil
	}
	return journal.RetireFederationPeers(ctx, keep)
}

func pendingFederation(ctx context.Context, journal *events.Store, consumer string, cfg eventgroupconfig.Config, peer eventgroupconfig.Peer) ([]eventgroupconfig.Delivery, error) {
	pending, err := journal.Pending(ctx, consumer, 64)
	if err != nil {
		return nil, err
	}
	var deliveries []eventgroupconfig.Delivery
	for _, job := range pending {
		var wire federation.Event
		if err := json.Unmarshal(job.Payload, &wire); err != nil {
			return nil, err
		}
		if wire.Group != cfg.Group || wire.SourceNode != cfg.NodeName || wire.IsExpired(time.Now()) || cfg.MaxAge > 0 && wire.ObservedAt.Before(time.Now().Add(-cfg.MaxAge)) || !eventgroupconfig.Matches(peer, wire) {
			if err := journal.Complete(ctx, consumer, job.Key); err != nil {
				return nil, err
			}
			continue
		}
		deliveries = append(deliveries, eventgroupconfig.Delivery{Key: job.Key, Event: wire})
	}
	return deliveries, nil
}
