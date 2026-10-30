// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

type flowPage struct {
	Services []string
	Selected string
	Client   string
	Peer     string
	Protocol string
	Since    string
	Until    string
	Limit    string
	History  *runtimeplan.TrafficHistory
}

func loadFlows(ctx context.Context, client *localapi.Client, query url.Values) (*flowPage, error) {
	p := &flowPage{Selected: query.Get("service"), Client: query.Get("client"), Peer: query.Get("peer"), Protocol: query.Get("protocol"), Since: query.Get("since"), Until: query.Get("until"), Limit: query.Get("limit")}
	if p.Limit == "" {
		p.Limit = "100"
	}
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return p, err
	}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "traffic_flow" {
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
		return p, errors.New("指定した通信履歴サービスはありません")
	}
	p.History = new(runtimeplan.TrafficHistory)
	values := url.Values{"client": {p.Client}, "peer": {p.Peer}, "protocol": {p.Protocol}, "since": {p.Since}, "until": {p.Until}, "limit": {p.Limit}}
	err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(p.Selected)+"/traffic-history?"+values.Encode(), nil, p.History)
	return p, err
}
