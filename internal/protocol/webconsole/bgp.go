// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed bgp.html
var bgpHTML string

type bgpPage struct {
	Services    []string
	Selected    string
	Observation *bgpconfig.Snapshot
	Current     bool
}

// The BGP worker already owns peer and selected-path observations. Reading
// that complete snapshot preserves its original deadline and adds no poller.
func loadBGP(ctx context.Context, client *localapi.Client, serviceName string) (*bgpPage, error) {
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return nil, fmt.Errorf("BGPサービス情報を取得できません: %w", err)
	}
	p := &bgpPage{}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "bgp" {
			continue
		}
		p.Services = append(p.Services, instance.Name)
		if serviceName == "" {
			serviceName = instance.Name
		}
		found = found || instance.Name == serviceName
	}
	if serviceName == "" {
		return p, nil
	}
	if !found {
		return nil, errors.New("指定したBGPサービスは現在の実行構成にありません")
	}
	p.Selected = serviceName
	var instance runtimeplan.InstanceStatus
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(serviceName), nil, &instance); err != nil {
		return nil, fmt.Errorf("BGPサービスの観測を取得できません: %w", err)
	}
	if len(instance.Snapshot) == 0 {
		return p, nil
	}
	p.Observation = new(bgpconfig.Snapshot)
	if err := json.Unmarshal(instance.Snapshot, p.Observation); err != nil {
		return nil, fmt.Errorf("BGPサービスの観測を解釈できません: %w", err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return nil, fmt.Errorf("BGPサービスの観測期限を確認できません: %w", err)
	}
	p.Current = bgpconfig.CurrentObservation(*p.Observation, clock)
	return p, nil
}

func bgpPeerState(state string) string {
	switch state {
	case "SESSION_STATE_IDLE":
		return "待機"
	case "SESSION_STATE_CONNECT":
		return "TCP接続中"
	case "SESSION_STATE_ACTIVE":
		return "TCP接続待ち"
	case "SESSION_STATE_OPENSENT":
		return "開始メッセージ送信済み"
	case "SESSION_STATE_OPENCONFIRM":
		return "開始確認中"
	case "SESSION_STATE_ESTABLISHED":
		return "接続確立"
	default:
		return "未取得"
	}
}

func bgpAdminState(state string) string {
	switch state {
	case "ADMIN_STATE_UP":
		return "有効"
	case "ADMIN_STATE_DOWN":
		return "停止"
	case "ADMIN_STATE_PFX_CT":
		return "経路数の制限により停止"
	default:
		return "未取得"
	}
}
