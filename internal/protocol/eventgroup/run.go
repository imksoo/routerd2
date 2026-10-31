// SPDX-License-Identifier: BSD-3-Clause
// Package eventgroup transports signed events; control remains their journal owner.
package eventgroup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd/pkg/federation"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/eventgroupconfig"
)

func Run(ctx context.Context, cfg eventgroupconfig.Config, runtimeDir, name string, ready func()) error {
	if err := eventgroupconfig.Validate(cfg); err != nil {
		return err
	}
	data, err := os.ReadFile(cfg.SecretFile)
	if err != nil {
		return err
	}
	secret := []byte(strings.TrimSpace(string(data)))
	if len(secret) == 0 {
		return errors.New("event group HMAC secret is empty")
	}
	local := localapi.NewClient(filepath.Join(runtimeDir, "control.sock"))
	base := "/v1/event-groups/" + url.PathEscape(name)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	var listener net.Listener
	if cfg.Listen.IsValid() {
		listener, err = net.Listen("tcp", cfg.Listen.String())
		if err != nil {
			return err
		}
		defer listener.Close()
	}
	for _, peer := range cfg.Peers {
		wg.Add(1)
		go func() { defer wg.Done(); send(ctx, cfg, peer, secret, local, base) }()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := local.Do(ctx, "POST", base+"/prune", nil, nil); err != nil && ctx.Err() == nil {
					slog.Error("event group retention failed", "group", cfg.Group, "error", err)
				}
			}
		}
	}()
	if listener == nil {
		if ready != nil {
			ready()
		}
		<-ctx.Done()
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body", 400)
			return
		}
		timestamp, err := strconv.ParseInt(r.Header.Get("X-Routerd-Timestamp"), 10, 64)
		if err != nil {
			http.Error(w, "bad timestamp", 400)
			return
		}
		if err = federation.Verify(secret, timestamp, body, r.Header.Get("X-Routerd-Signature"), time.Now(), cfg.ReplayWindow); err != nil {
			code := http.StatusUnauthorized
			if errors.Is(err, federation.ErrStaleTimestamp) {
				code = http.StatusForbidden
			}
			http.Error(w, err.Error(), code)
			return
		}
		var event federation.Event
		if err = json.Unmarshal(body, &event); err != nil {
			http.Error(w, "bad event body", 400)
			return
		}
		if err = event.Normalize(); err != nil || event.Group != cfg.Group {
			http.Error(w, "event does not identify this group", 400)
			return
		}
		if err = local.Do(r.Context(), "POST", base+"/receive", event, nil); err != nil {
			code := 503
			var failure *localapi.HTTPError
			if errors.As(err, &failure) {
				code = failure.StatusCode
			}
			http.Error(w, err.Error(), code)
			return
		}
		localapi.JSON(w, 200, struct {
			Accepted bool   `json:"accepted"`
			ID       string `json:"id"`
		}{true, event.ID})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		localapi.JSON(w, 200, struct {
			Node  string `json:"node"`
			Group string `json:"group"`
		}{cfg.NodeName, cfg.Group})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stop:
		}
	}()
	if ready != nil {
		ready()
	}
	err = server.Serve(listener)
	close(stop)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Each peer has ordered delivery work in the existing journal. A failed peer never
// blocks another, and a retry keeps the source event's ID and original expiry.
func send(ctx context.Context, cfg eventgroupconfig.Config, peer eventgroupconfig.Peer, secret []byte, local *localapi.Client, base string) {
	client := &http.Client{Timeout: 5 * time.Second}
	path := base + "/outbox/" + url.PathEscape(peer.NodeName)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		err := drain(ctx, peer, secret, local, client, path)
		if err != nil && ctx.Err() == nil {
			slog.Warn("event peer delivery pending", "group", cfg.Group, "peer", peer.NodeName, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func drain(ctx context.Context, peer eventgroupconfig.Peer, secret []byte, local *localapi.Client, client *http.Client, path string) error {
	for {
		var page eventgroupconfig.Page
		if err := local.Do(ctx, "GET", path, nil, &page); err != nil {
			return err
		}
		if len(page.Deliveries) == 0 && !page.More {
			return nil
		}
		for _, delivery := range page.Deliveries {
			event := delivery.Event
			if event.IsExpired(time.Now()) {
				if err := local.Do(ctx, "POST", path, eventgroupconfig.Ack{Key: delivery.Key}, nil); err != nil {
					return err
				}
				continue
			}
			body, err := json.Marshal(event)
			if err != nil {
				return err
			}
			// The existing eventd wire format signs the exact body and Unix timestamp.
			timestamp := time.Now().Unix()
			request, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(peer.Endpoint, "/")+"/v1/events", bytes.NewReader(body))
			if err != nil {
				return err
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Routerd-Timestamp", strconv.FormatInt(timestamp, 10))
			request.Header.Set("X-Routerd-Signature", federation.Sign(secret, timestamp, body))
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			closeErr := response.Body.Close()
			if err = errors.Join(readErr, closeErr); err != nil {
				return err
			}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return fmt.Errorf("event peer returned HTTP %d", response.StatusCode)
			}
			if err := local.Do(ctx, "POST", path, eventgroupconfig.Ack{Key: delivery.Key}, nil); err != nil {
				return err
			}
		}
	}
}
