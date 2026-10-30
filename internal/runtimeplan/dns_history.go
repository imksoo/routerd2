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
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

type DNSHistory struct {
	Service   string              `json:"service"`
	ReadAt    time.Time           `json:"read_at"`
	Limit     int                 `json:"limit"`
	Truncated bool                `json:"truncated"`
	Queries   []logstore.DNSQuery `json:"queries"`
}

func (r *Runtime) dnsHistoryRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/dns-history", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		filter := logstore.DNSQueryFilter{QName: strings.TrimSpace(q.Get("name")), ResponseCode: strings.ToUpper(strings.TrimSpace(q.Get("response")))}
		var err error
		filter.Since, filter.Until, filter.Limit, err = historyRange(q)
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		if raw := strings.TrimSpace(q.Get("client")); raw != "" {
			address, err := netip.ParseAddr(raw)
			if err != nil || address.Zone() != "" {
				localapi.Fail(w, http.StatusBadRequest, errors.New("端末にはゾーン指定のないIPアドレスを指定してください"))
				return
			}
			filter.Client = address.Unmap().String()
		}
		name := req.PathValue("name")
		r.mu.Lock()
		bound, ok := r.bindings[name]
		r.mu.Unlock()
		if !ok || bound.instance.Role != "dns" {
			localapi.Fail(w, http.StatusNotFound, errors.New("指定したDNSサービスはありません"))
			return
		}
		var cfg dnsconfig.Config
		if err := strict(bound.instance.Config, &cfg); err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		if !cfg.Runtime.Spec.QueryLog.Enabled {
			localapi.Fail(w, http.StatusConflict, errors.New("DNS問い合わせ履歴の保存は無効です"))
			return
		}
		// The DNS worker alone writes and retains this prepared database.
		// No DNS socket or runtime lock is held during the historical read.
		db, err := logstore.OpenDNSQueryLogReadOnly(cfg.Runtime.Spec.QueryLog.Path)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("DNS問い合わせ履歴を開けません"))
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
		defer cancel()
		page := DNSHistory{Service: name, ReadAt: time.Now().UTC(), Limit: filter.Limit}
		filter.Limit++
		page.Queries, err = db.List(ctx, filter)
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("DNS問い合わせ履歴を読み取れません"))
			return
		}
		if len(page.Queries) > page.Limit {
			page.Truncated = true
			page.Queries = page.Queries[:page.Limit]
		}
		localapi.JSON(w, http.StatusOK, page)
	})
}
