// SPDX-License-Identifier: BSD-3-Clause

package runtimeplan

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

type DHCPFingerprintHistory struct {
	Service   string                     `json:"service"`
	ReadAt    time.Time                  `json:"read_at"`
	Limit     int                        `json:"limit"`
	Truncated bool                       `json:"truncated"`
	Entries   []logstore.DHCPFingerprint `json:"entries"`
}

func (r *Runtime) dhcpFingerprintsRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/dhcp-fingerprints", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		since, until, limit, err := historyRange(q)
		if err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		if !until.IsZero() {
			localapi.Fail(w, http.StatusBadRequest, errors.New("端末識別記録の日時指定にはsinceを使ってください"))
			return
		}
		mac := strings.TrimSpace(q.Get("mac"))
		if mac != "" {
			address, err := net.ParseMAC(mac)
			if err != nil {
				localapi.Fail(w, http.StatusBadRequest, errors.New("MACアドレスの表記を確認してください"))
				return
			}
			mac = address.String()
		}
		name := req.PathValue("name")
		r.mu.Lock()
		bound, ok := r.bindings[name]
		r.mu.Unlock()
		if !ok || bound.instance.Role != "dhcp_server" {
			localapi.Fail(w, http.StatusNotFound, errors.New("指定したDHCPサービスはありません"))
			return
		}
		var cfg dhcpserverconfig.Config
		if err := strict(bound.instance.Config, &cfg); err != nil {
			localapi.Fail(w, http.StatusInternalServerError, err)
			return
		}
		// DHCP owns collection and retention. Read retained observations even when
		// collection has stopped, without opening a writer or holding the runtime lock.
		db, err := logstore.OpenDHCPFingerprintLogReadOnly(filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-fingerprints.db"))
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("DHCP端末識別記録を開けません"))
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(req.Context(), 1500*time.Millisecond)
		defer cancel()
		page := DHCPFingerprintHistory{Service: name, ReadAt: time.Now().UTC(), Limit: limit}
		page.Entries, err = db.List(ctx, logstore.DHCPFingerprintFilter{MAC: mac, Since: since, Limit: limit + 1})
		if err != nil {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("DHCP端末識別記録を読み取れません"))
			return
		}
		if len(page.Entries) > limit {
			page.Truncated = true
			page.Entries = page.Entries[:limit]
		}
		localapi.JSON(w, http.StatusOK, page)
	})
}
