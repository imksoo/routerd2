// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"net/url"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

//go:embed firewall_events.html
var firewallEventsHTML string

type firewallEventPage struct {
	Services                               []string
	Selected, Source, Action, Since, Limit string
	History                                *runtimeplan.FirewallHistory
}

func loadFirewallEvents(ctx context.Context, client *localapi.Client, query url.Values) (*firewallEventPage, error) {
	p := &firewallEventPage{Selected: query.Get("service"), Source: query.Get("source"), Action: query.Get("action"), Since: query.Get("since"), Limit: query.Get("limit")}
	if p.Limit == "" {
		p.Limit = "100"
	}
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return p, err
	}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "firewall_log" {
			continue
		}
		p.Services = append(p.Services, instance.Name)
		if p.Selected == "" {
			p.Selected = instance.Name
		}
		found = found || p.Selected == instance.Name
	}
	if p.Selected == "" {
		return p, nil
	}
	if !found {
		return p, errors.New("指定したファイアウォール記録サービスはありません")
	}
	p.History = new(runtimeplan.FirewallHistory)
	values := url.Values{"source": {p.Source}, "action": {p.Action}, "since": {p.Since}, "limit": {p.Limit}}
	err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(p.Selected)+"/firewall-history?"+values.Encode(), nil, p.History)
	return p, err
}
