// SPDX-License-Identifier: BSD-3-Clause

package runtimeplan

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
)

type FirewallHistory struct {
	Service string                      `json:"service"`
	ReadAt  time.Time                   `json:"read_at"`
	Limit   int                         `json:"limit"`
	AtLimit bool                        `json:"at_limit"`
	Entries []logstore.FirewallLogEntry `json:"entries"`
}

func (r *Runtime) firewallHistoryRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/firewall-history", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		since, until, limit, err := historyRange(q)
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		if !until.IsZero() {
			localapi.Fail(w, http.StatusBadRequest, errors.New("ファイアウォール履歴は開始日時で絞り込んでください"))
			return
		}
		filter := logstore.FirewallLogFilter{Since: since, Limit: limit, Action: strings.ToLower(strings.TrimSpace(q.Get("action")))}
		if raw := strings.TrimSpace(q.Get("source")); raw != "" {
			address, err := netip.ParseAddr(raw)
			if err != nil || address.Zone() != "" {
				localapi.Fail(w, http.StatusBadRequest, errors.New("送信元にはゾーン指定のないIPアドレスを指定してください"))
				return
			}
			filter.Src = address.Unmap().String()
		}
		name := req.PathValue("name")
		r.mu.Lock()
		bound, ok := r.bindings[name]
		r.mu.Unlock()
		if !ok || bound.instance.Role != "firewall_log" {
			localapi.Fail(w, http.StatusNotFound, errors.New("指定したファイアウォール記録サービスはありません"))
			return
		}
		var cfg firewalllogconfig.Config
		if err := strict(bound.instance.Config, &cfg); err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		// The NFLOG collector remains the sole writer and expiry owner. This
		// historical read holds neither its queue nor the runtime state lock.
		db, err := logstore.OpenFirewallLogReadOnly(cfg.HistoryPath)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("ファイアウォール履歴を開けません"))
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
		defer cancel()
		page := FirewallHistory{Service: name, ReadAt: time.Now().UTC(), Limit: limit}
		page.Entries, err = db.List(ctx, filter)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("ファイアウォール履歴を読み取れません"))
			return
		}
		// The reused store caps reads at 1000; reaching the requested limit does
		// not prove that another matching entry exists.
		page.AtLimit = len(page.Entries) == limit
		localapi.JSON(w, http.StatusOK, page)
	})
}
