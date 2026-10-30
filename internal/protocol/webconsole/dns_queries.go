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

//go:embed dns_queries.html
var dnsQueriesHTML string

type dnsQueryPage struct {
	Services []string
	Selected string
	Client   string
	Name     string
	Response string
	Since    string
	Until    string
	Limit    string
	History  *runtimeplan.DNSHistory
}

func loadDNSQueries(ctx context.Context, client *localapi.Client, query url.Values) (*dnsQueryPage, error) {
	p := &dnsQueryPage{Selected: query.Get("service"), Client: query.Get("client"), Name: query.Get("name"), Response: query.Get("response"), Since: query.Get("since"), Until: query.Get("until"), Limit: query.Get("limit")}
	if p.Limit == "" {
		p.Limit = "100"
	}
	var services runtimeplan.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/services", nil, &services); err != nil {
		return p, err
	}
	found := false
	for _, instance := range services.Instances {
		if instance.Role != "dns" {
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
		return p, errors.New("指定したDNSサービスはありません")
	}
	p.History = new(runtimeplan.DNSHistory)
	values := url.Values{"client": {p.Client}, "name": {p.Name}, "response": {p.Response}, "since": {p.Since}, "until": {p.Until}, "limit": {p.Limit}}
	err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(p.Selected)+"/dns-history?"+values.Encode(), nil, p.History)
	return p, err
}
