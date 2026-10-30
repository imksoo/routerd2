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
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed time.html
var timeHTML string

type timePage struct {
	Services    []string
	Selected    string
	Observation *ntpconfig.Snapshot
	Current     bool
	State       string
}

func loadTime(ctx context.Context, client *localapi.Client, serviceName string) (*timePage, error) {
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return nil, fmt.Errorf("時刻サービス情報を取得できません: %w", err)
	}
	p := &timePage{State: "未観測"}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "ntp" {
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
		return nil, errors.New("指定した時刻サービスは現在の実行構成にありません")
	}
	p.Selected = serviceName
	var instance runtimeplan.InstanceStatus
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(serviceName), nil, &instance); err != nil {
		return nil, fmt.Errorf("時刻サービスの観測を取得できません: %w", err)
	}
	if len(instance.Snapshot) == 0 {
		return p, nil
	}
	p.Observation = new(ntpconfig.Snapshot)
	if err := json.Unmarshal(instance.Snapshot, p.Observation); err != nil {
		return nil, fmt.Errorf("時刻サービスの観測を解釈できません: %w", err)
	}
	clock, err := boottime.Read()
	if err != nil {
		return nil, fmt.Errorf("時刻サービスの観測期限を確認できません: %w", err)
	}
	snapshot := p.Observation
	now := uint64(clock.BoottimeNanoseconds)
	p.Current = snapshot.BootID == clock.BootID && snapshot.ObservedNanos > 0 && snapshot.ObservedNanos <= now && now < snapshot.ExpiresNanos
	switch {
	case !p.Current:
		p.State = "観測期限切れ"
	case snapshot.LastError != "":
		p.State = "観測失敗"
	case snapshot.Synchronized:
		p.State = "同期済み"
	default:
		p.State = "未同期"
	}
	return p, nil
}
