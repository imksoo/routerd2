// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed vpn.html
var vpnHTML string

// Only operator intent needed by the page crosses into its template data.
// Authentication files, control URLs and persisted engine state stay private.
type vpnIntent struct {
	Interface         string
	Hostname          string
	AdvertiseRoutes   []netip.Prefix
	AdvertiseExitNode bool
	AcceptRoutes      bool
	ShieldsUp         bool
}

type vpnPage struct {
	Services    []string
	Selected    string
	Revision    uint64
	Intent      *vpnIntent
	Observation *tailscaleconfig.Snapshot
	Current     bool
	State       string
}

func loadVPN(ctx context.Context, client *localapi.Client, serviceName string) (*vpnPage, error) {
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return nil, fmt.Errorf("VPNのサービス情報を取得できません: %w", err)
	}
	var document config.Document
	path := "/v1/config/document?revision=" + strconv.FormatUint(services.Revision, 10)
	if err := client.Do(ctx, http.MethodGet, path, nil, &document); err != nil {
		return nil, fmt.Errorf("VPNの設定を取得できません: %w", err)
	}
	intents := make(map[string]vpnIntent)
	page := &vpnPage{Revision: services.Revision, State: "未観測"}
	for _, resource := range document.Resources {
		service := resource.Service
		if service == nil || service.Tailscale == nil {
			continue
		}
		name := service.ModuleName()
		node := service.Tailscale
		intents[name] = vpnIntent{Interface: node.Interface, Hostname: node.Hostname,
			AdvertiseRoutes: node.AdvertiseRoutes, AdvertiseExitNode: node.AdvertiseExitNode,
			AcceptRoutes: node.AcceptRoutes, ShieldsUp: node.ShieldsUp}
		page.Services = append(page.Services, name)
	}
	sort.Strings(page.Services)
	if serviceName == "" && len(page.Services) > 0 {
		serviceName = page.Services[0]
	}
	if serviceName == "" {
		return page, nil
	}
	intent, configured := intents[serviceName]
	if !configured {
		return nil, errors.New("指定したTailscaleサービスは構成されていません")
	}
	page.Selected, page.Intent = serviceName, &intent
	bound := false
	for _, instance := range services.Instances {
		if instance.Name == serviceName && instance.Role == "tailscale" {
			bound = true
			break
		}
	}
	// A conditionally disabled service is still configured. Its absence from
	// the runtime does not establish an engine failure or a current membership.
	if !bound {
		return page, nil
	}
	var instance runtimeplan.InstanceStatus
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(serviceName), nil, &instance); err != nil {
		return nil, fmt.Errorf("Tailscaleの観測を取得できません: %w", err)
	}
	if len(instance.Snapshot) == 0 {
		return page, nil
	}
	page.Observation = new(tailscaleconfig.Snapshot)
	if err := json.Unmarshal(instance.Snapshot, page.Observation); err != nil {
		return nil, fmt.Errorf("Tailscaleの観測を解釈できません: %w", err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return nil, fmt.Errorf("Tailscaleの観測期限を確認できません: %w", err)
	}
	observation := page.Observation
	now := uint64(clock.BoottimeNanoseconds)
	page.Current = observation.BootID == clock.BootID && observation.ObservedNanos > 0 && observation.ObservedNanos <= now && now < observation.ExpiresNanos
	if !page.Current {
		page.State = "観測期限切れ"
		return page, nil
	}
	switch observation.BackendState {
	case "Running":
		if observation.Online {
			page.State = "稼働中・制御接続あり"
		} else {
			page.State = "稼働中・制御接続を確認できません"
		}
	case "Starting":
		page.State = "起動中"
	case "Stopped":
		page.State = "停止"
	case "NeedsLogin":
		page.State = "再認証が必要です。管理CLIで確認してください"
	case "NeedsMachineAuth":
		page.State = "端末の承認待ち"
	case "InUseOtherUser":
		page.State = "別の利用者が使用中"
	default:
		page.State = "稼働状態を確認できません"
	}
	return page, nil
}
