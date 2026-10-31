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
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed pd.html
var pdHTML string

type pdPage struct {
	Services           []string
	Selected           string
	Observation        *dhcp6config.Snapshot
	Clock              boottime.Clock
	Active             bool
	InformationCurrent bool
}

// The client owns assignments and their original boot-clock lifetimes. A page
// reads its complete observation without retaining or renewing that authority.
func loadPD(ctx context.Context, client *localapi.Client, serviceName string) (*pdPage, error) {
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return nil, fmt.Errorf("DHCPv6サービス情報を取得できません: %w", err)
	}
	p := &pdPage{}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "dhcp6" {
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
		return nil, errors.New("指定したDHCPv6サービスは現在の実行構成にありません")
	}
	p.Selected = serviceName
	var instance runtimeplan.InstanceStatus
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(serviceName), nil, &instance); err != nil {
		return nil, fmt.Errorf("DHCPv6サービスの観測を取得できません: %w", err)
	}
	if len(instance.Snapshot) == 0 {
		return p, nil
	}
	p.Observation = new(dhcp6config.Snapshot)
	if err := json.Unmarshal(instance.Snapshot, p.Observation); err != nil {
		return nil, fmt.Errorf("DHCPv6サービスの観測を解釈できません: %w", err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return nil, fmt.Errorf("DHCPv6の期限を確認できません: %w", err)
	}
	p.Clock = clock
	p.Active = p.Observation.Active(clock)
	_, p.InformationCurrent = p.Observation.InformationUntil(clock)
	return p, nil
}

func pdRemaining(d lifetime.Deadline, clock boottime.Clock) string {
	if !d.Live(clock) {
		return "期限切れ"
	}
	if d.Nanos == 0 && d.UTC.IsZero() {
		return "無期限"
	}
	return fmt.Sprintf("残り%d秒", d.Remaining(clock)/time.Second)
}

func pdState(state string) string {
	switch state {
	case "waiting_ra":
		return "ルーター広告待ち"
	case "standby":
		return "待機系"
	case "preparing":
		return "引継ぎ準備中"
	case "soliciting":
		return "サーバー探索中"
	case "requesting":
		return "割当要求中"
	case "confirming":
		return "割当確認中"
	case "bound":
		return "取得済み"
	case "renewing":
		return "更新中"
	case "rebinding":
		return "再取得中"
	case "declining":
		return "重複アドレスの辞退中"
	case "information":
		return "情報取得済み"
	case "stopped":
		return "停止"
	default:
		return "未観測"
	}
}
