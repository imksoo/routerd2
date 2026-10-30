// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVPNHTTPObservationAndPrivateConfiguration(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	observation := tailscaleconfig.Snapshot{
		Resource: "tailnet", Interface: "tailscale0", BackendState: "Running", Online: true,
		DNSName: "router.example.invalid.", Addresses: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
		Tailnet: &tailscaleconfig.Tailnet{Name: "test-net", DNSDomain: "example.invalid", MagicDNSEnabled: true},
		Peers: []tailscaleconfig.Peer{{ID: "peer-1", Hostname: "<peer>", Online: true,
			Addresses:       []netip.Addr{netip.MustParseAddr("100.64.0.2")},
			AllowedPrefixes: []netip.Prefix{netip.MustParsePrefix("198.18.1.0/24")}, RxBytes: 1234, TxBytes: 5678}},
		BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds),
		ExpiresNanos: uint64(clock.BoottimeNanoseconds + int64(time.Minute)),
	}
	for _, tc := range []struct {
		name    string
		bound   bool
		state   string
		expired bool
		want    []string
		absent  []string
	}{
		{name: "current", bound: true, state: "Running", want: []string{"稼働中・制御接続あり", "100.64.0.1", "100.64.0.2", "&lt;peer&gt;", "198.18.1.0/24", "受信 1234 バイト", "経路は設定値", "/ops/vpn?service=service.tailnet"}, absent: []string{"<peer>"}},
		{name: "expired", bound: true, state: "Running", expired: true, want: []string{"観測期限切れ", "現在の接続状態は未確認", "<td>未確認</td>"}, absent: []string{"制御接続あり", "<td>接続あり</td>"}},
		{name: "authentication_required", bound: true, state: "NeedsLogin", want: []string{"再認証が必要です"}, absent: []string{"稼働中"}},
		{name: "configured_without_observation", want: []string{"未観測", "このサービスの接続状態はまだ観測されていません"}, absent: []string{"100.64.0.1", "制御接続あり"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := observation
			snapshot.BackendState = tc.state
			if tc.expired {
				snapshot.ExpiresNanos = snapshot.ObservedNanos
			}
			// The typed read must discard private upstream fields, even if a
			// response accidentally includes them alongside the observation.
			encoded, err := json.Marshal(struct {
				tailscaleconfig.Snapshot
				AuthURL string `json:"auth_url"`
				State   string `json:"state"`
			}{snapshot, "https://secret-auth.invalid/private-code", "private-state-marker"})
			if err != nil {
				t.Fatal(err)
			}
			document := config.Document{SchemaVersion: 2, Resources: []resourceprogram.Resource{{
				ID: "service/tailnet", Service: &serviceconfig.Service{Name: "tailnet", Tailscale: &tailscaleconfig.Node{
					Interface: "tailscale0", Hostname: "router", ServerURL: "https://secret-control.invalid",
					AuthKeyFile: "/private/auth-key-marker", ControlCAFile: "/private/ca-marker",
					AdvertiseRoutes: []netip.Prefix{netip.MustParsePrefix("198.18.0.0/24")}, AcceptRoutes: true,
				}},
			}}}
			list := runtimeplan.Status{Revision: 12}
			if tc.bound {
				list.Instances = []runtimeplan.InstanceStatus{{Name: "service.tailnet", Role: "tailscale", SnapshotTruncated: true}}
			}
			ctx, cancel := context.WithCancel(t.Context())
			ready, stopped := make(chan struct{}), make(chan error, 1)
			socket := filepath.Join(t.TempDir(), "control.sock")
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/services":
					localapi.JSON(w, http.StatusOK, list)
				case "/v1/config/document":
					if r.URL.Query().Get("revision") != "12" {
						http.Error(w, "revision mismatch", http.StatusConflict)
						return
					}
					localapi.JSON(w, http.StatusOK, document)
				case "/v1/services/service.tailnet":
					localapi.JSON(w, http.StatusOK, runtimeplan.InstanceStatus{Name: "service.tailnet", Role: "tailscale", Snapshot: encoded})
				default:
					http.NotFound(w, r)
				}
			})
			go func() { stopped <- localapi.Serve(ctx, socket, handler, func() { close(ready) }) }()
			defer func() {
				cancel()
				if err := <-stopped; err != nil {
					t.Error(err)
				}
			}()
			select {
			case <-ready:
			case <-time.After(3 * time.Second):
				t.Fatal("control API did not listen")
			}
			client := localapi.NewClient(socket)
			defer client.HTTP.CloseIdleConnections()
			view, err := template.New("vpn").Parse(vpnHTML)
			if err != nil {
				t.Fatal(err)
			}
			console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page, err := loadVPN(r.Context(), client, r.URL.Query().Get("service"))
				if err != nil {
					http.Error(w, err.Error(), http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if err := view.ExecuteTemplate(w, "vpn", struct {
					Config webconsoleconfig.Config
					VPN    *vpnPage
				}{
					webconsoleconfig.Config{BasePath: "/ops/"}, page,
				}); err != nil {
					t.Error(err)
				}
			}))
			defer console.Close()
			response, err := console.Client().Get(console.URL + "/ops/vpn?service=service.tailnet")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.StatusCode, body)
			}
			for _, want := range append(tc.want, "198.18.0.0/24", "対向経路の受入") {
				if !strings.Contains(string(body), want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, absent := range append(tc.absent, "secret-auth", "private-code", "private-state-marker", "secret-control", "auth-key-marker", "ca-marker") {
				if strings.Contains(string(body), absent) {
					t.Errorf("unexpected %q", absent)
				}
			}
		})
	}
}
