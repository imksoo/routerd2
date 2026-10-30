// SPDX-License-Identifier: BSD-3-Clause

// Package webconsole presents the existing control and kernel observations.
// It retains routerd's read-only, escaped HTML, base-path and link behavior,
// using the native typed API instead of recreating the old resource store.
package webconsole

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed page.html
var pageHTML string

//go:embed flows.html
var flowsHTML string

type page struct {
	Config         webconsoleconfig.Config
	Heading        string
	Error          string
	Health         *control.Health
	Network        *network.ObservationView
	NetworkPage    *networkPage
	Resources      *runtimeplan.ResourceObservationPage
	Metadata       *control.ConfigurationMetadata
	Next           string
	Clients        *clientPage
	Flows          *flowPage
	VPN            *vpnPage
	BGP            *bgpPage
	DNSQueries     *dnsQueryPage
	FirewallEvents *firewallEventPage
	Logs           *logPage
	Time           *timePage
	Update         *updatePage
}

type clientPage struct {
	Servers  []string
	Selected runtimeplan.InstanceStatus
	Snapshot *dhcpserverconfig.Snapshot
	Current  bool
}

func Run(ctx context.Context, cfg webconsoleconfig.Config, runtimeDir string, ready func()) error {
	cfg = webconsoleconfig.Normalize(cfg)
	if err := webconsoleconfig.Validate(cfg); err != nil {
		return err
	}
	view, err := template.New("console").Funcs(template.FuncMap{"logGapReason": logGapReason, "bgpPeerState": bgpPeerState, "bgpAdminState": bgpAdminState, "leaseLive": func(t time.Time) bool { return t.IsZero() || t.After(time.Now()) }, "truth": func(t resourceprogram.Truth) string {
		switch t {
		case resourceprogram.True:
			return "有効"
		case resourceprogram.False:
			return "無効"
		default:
			return "不明"
		}
	}}).Parse(pageHTML + flowsHTML + vpnHTML + dnsQueriesHTML + firewallEventsHTML + logsHTML + timeHTML + updateHTML + bgpHTML + networkHTML)
	if err != nil {
		return err
	}
	client := localapi.NewClient(filepath.Join(runtimeDir, control.SocketName))
	client.HTTP.Timeout = 5 * time.Second
	defer client.HTTP.CloseIdleConnections()
	logClient := localapi.NewClient(filepath.Join(runtimeDir, logging.SocketName))
	logClient.HTTP.Timeout = 5 * time.Second
	defer logClient.HTTP.CloseIdleConnections()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only console", http.StatusMethodNotAllowed)
			return
		}
		if cfg.BasePath != "/" && r.URL.Path == strings.TrimSuffix(cfg.BasePath, "/") {
			http.Redirect(w, r, cfg.BasePath, http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, cfg.BasePath) {
			http.NotFound(w, r)
			return
		}
		p := page{Config: cfg}
		status := http.StatusOK
		var err error
		switch strings.TrimPrefix(r.URL.Path, cfg.BasePath) {
		case "":
			p.Heading = "稼働状態"
			var observation control.Health
			observation, status, err = readHealth(r.Context(), client)
			p.Health = &observation
		case "network":
			p.Heading = "ネットワーク"
			p.Network = new(network.ObservationView)
			err = client.Do(r.Context(), http.MethodGet, "/v1/network", nil, p.Network)
		case "routes", "policy-rules", "route-next-hops":
			p.Heading = "ネットワークの詳細"
			p.NetworkPage, err = loadNetworkPage(r.Context(), client, strings.TrimPrefix(r.URL.Path, cfg.BasePath), r.URL.Query(), cfg.BasePath)
		case "resources":
			p.Heading = "設定資源の適用状態"
			p.Resources = new(runtimeplan.ResourceObservationPage)
			query := r.URL.Query()

			err = client.Do(r.Context(), http.MethodGet, "/v1/services/resources?"+query.Encode(), nil, p.Resources)
			if err == nil && p.Resources.NextOffset != nil {
				next := url.Values{"offset": {strconv.Itoa(*p.Resources.NextOffset)}, "observation_id": {p.Resources.ObservationID}}
				p.Next = cfg.BasePath + "resources?" + next.Encode()
			}
		case "clients":
			p.Heading = "DHCP端末"
			p.Clients = new(clientPage)
			var services runtimeplan.Status
			err = client.Do(r.Context(), http.MethodGet, "/v1/services", nil, &services)
			selected := r.URL.Query().Get("service")
			found := false
			for _, instance := range services.Instances {
				if instance.Role != "dhcp_server" {
					continue
				}
				p.Clients.Servers = append(p.Clients.Servers, instance.Name)
				if selected == "" {
					selected = instance.Name
				}
				found = found || selected == instance.Name
			}
			if err == nil && selected != "" {
				if !found {
					err = errors.New("指定したDHCPサービスはありません")
					status = http.StatusNotFound
				} else {
					err = client.Do(r.Context(), http.MethodGet, "/v1/services/"+url.PathEscape(selected), nil, &p.Clients.Selected)
					if err == nil && len(p.Clients.Selected.Snapshot) > 0 {
						p.Clients.Snapshot = new(dhcpserverconfig.Snapshot)
						err = json.Unmarshal(p.Clients.Selected.Snapshot, p.Clients.Snapshot)
						if err == nil {
							var clock boottime.Clock
							clock, err = boottime.Read()
							p.Clients.Current = err == nil && dhcpserverconfig.CurrentObservation(*p.Clients.Snapshot, clock)
						}
					}
				}
			}
		case "flows":
			p.Heading = "通信履歴"
			p.Flows, err = loadFlows(r.Context(), client, r.URL.Query())
		case "firewall-events":
			p.Heading = "ファイアウォール履歴"
			p.FirewallEvents, err = loadFirewallEvents(r.Context(), client, r.URL.Query())
		case "dns-queries":
			p.Heading = "DNS問い合わせ履歴"
			p.DNSQueries, err = loadDNSQueries(r.Context(), client, r.URL.Query())
		case "bgp":
			p.Heading = "BGP経路交換"
			p.BGP, err = loadBGP(r.Context(), client, r.URL.Query().Get("service"))
		case "vpn":
			p.Heading = "VPN状態"
			p.VPN, err = loadVPN(r.Context(), client, r.URL.Query().Get("service"))
		case "time":
			p.Heading = "時刻同期"
			p.Time, err = loadTime(r.Context(), client, r.URL.Query().Get("service"))
		case "updates":
			p.Heading = "更新・復旧"
			p.Update, err = loadUpdate(r.Context(), client)
		case "logs":
			p.Heading = "運用ログ"
			p.Logs, err = loadLogs(r.Context(), logClient, r.URL.Query(), cfg.BasePath)
		case "configuration":
			p.Heading = "設定の確定状態"
			p.Metadata = new(control.ConfigurationMetadata)
			err = client.Do(r.Context(), http.MethodGet, "/v1/config/metadata", nil, p.Metadata)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			p.Error = err.Error()
			if status == http.StatusOK {
				status = http.StatusServiceUnavailable
			}
			var remote *localapi.HTTPError
			if errors.As(err, &remote) {
				status = remote.StatusCode
			}
		}
		var output bytes.Buffer
		if err := view.Execute(&output, p); err != nil {
			slog.Error("web console rendering failed", "error", err)
			http.Error(w, "console rendering failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write(output.Bytes())
		}
	})
	listener, err := net.Listen("tcp", cfg.Listen.String())
	if err != nil {
		return fmt.Errorf("web console listener: %w", err)
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	stopped := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = server.Shutdown(stop)
			_ = server.Close()
		case <-stopped:
		}
	}()
	if ready != nil {
		ready()
	}
	err = server.Serve(listener)
	close(stopped)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Health intentionally uses HTTP 503 with a typed observation during startup
// or failure; decoding it as an ordinary API error would lose that observation.
func readHealth(ctx context.Context, client *localapi.Client) (control.Health, int, error) {
	var observation control.Health
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://routerd/v1/health", nil)
	if err != nil {
		return observation, 0, err
	}
	response, err := client.HTTP.Do(request)
	if err != nil {
		return observation, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
		return observation, response.StatusCode, fmt.Errorf("control health returned HTTP %d", response.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(response.Body, localapi.MaxBody)).Decode(&observation)
	return observation, response.StatusCode, err
}
