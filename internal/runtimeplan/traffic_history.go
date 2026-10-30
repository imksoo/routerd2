// SPDX-License-Identifier: BSD-3-Clause

package runtimeplan

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
)

type TrafficHistory struct {
	Service   string                 `json:"service"`
	ReadAt    time.Time              `json:"read_at"`
	Limit     int                    `json:"limit"`
	Truncated bool                   `json:"truncated"`
	Flows     []logstore.TrafficFlow `json:"flows"`
}

// The existing collector remains the only writer. Resolve its prepared path,
// then release runtime ownership before any historical database work.
func (r *Runtime) trafficHistoryRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/traffic-history", func(w http.ResponseWriter, req *http.Request) {
		filter, err := trafficHistoryFilter(req)
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		name := req.PathValue("name")
		r.mu.Lock()
		bound, ok := r.bindings[name]
		r.mu.Unlock()
		if !ok || bound.instance.Role != "traffic_flow" {
			localapi.Fail(w, http.StatusNotFound, errors.New("unknown traffic flow service"))
			return
		}
		var cfg trafficflowconfig.Config
		if err := strict(bound.instance.Config, &cfg); err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		db, err := logstore.OpenTrafficFlowLogReadOnly(cfg.HistoryPath)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("traffic history is not available"))
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
		defer cancel()
		page := TrafficHistory{Service: name, ReadAt: time.Now().UTC(), Limit: filter.Limit}
		filter.Limit++
		page.Flows, err = db.List(ctx, filter)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("traffic history could not be read"))
			return
		}
		if len(page.Flows) > page.Limit {
			page.Truncated = true
			page.Flows = page.Flows[:page.Limit]
		}
		localapi.JSON(w, http.StatusOK, page)
	})
}

func trafficHistoryFilter(req *http.Request) (logstore.TrafficFlowFilter, error) {
	q := req.URL.Query()
	f := logstore.TrafficFlowFilter{Limit: 100, Protocol: strings.ToLower(strings.TrimSpace(q.Get("protocol")))}
	for _, field := range []struct {
		name  string
		value *string
	}{{"client", &f.Client}, {"peer", &f.Peer}} {
		if raw := strings.TrimSpace(q.Get(field.name)); raw != "" {
			address, err := netip.ParseAddr(raw)
			if err != nil || address.Zone() != "" {
				return f, errors.New("client and peer must be IP addresses")
			}
			*field.value = address.Unmap().String()
		}
	}
	for _, field := range []struct {
		name  string
		value *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if raw := q.Get(field.name); raw != "" {
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil || !time.Unix(0, value.UnixNano()).Equal(value) {
				return f, errors.New("time must be an RFC3339 timestamp within the stored range")
			}
			*field.value = value
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && f.Until.Before(f.Since) {
		return f, errors.New("until must not precede since")
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			return f, errors.New("limit must be between 1 and 1000")
		}
		f.Limit = limit
	}
	return f, nil
}
