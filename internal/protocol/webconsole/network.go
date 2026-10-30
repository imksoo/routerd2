// SPDX-License-Identifier: BSD-3-Clause

package webconsole

import (
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

//go:embed network.html
var networkHTML string

type networkPage struct {
	network.ObservationHeader
	Section       string
	Total, Offset int
	Table         string
	Next, Start   string
	Routes        []network.RouteObservation
	Rules         []network.PolicyRuleState
	NextHops      []network.NextHop
}

// Each request reads one bounded kernel page. The existing API owns validation
// and projection conflict detection; navigation carries its identity unchanged.
func loadNetworkPage(ctx context.Context, client *localapi.Client, section string, query url.Values, base string) (*networkPage, error) {
	if values, ok := query["table"]; ok && len(values) == 1 && values[0] == "" {
		query.Del("table")
	}
	restart := maps.Clone(query)
	restart.Del("offset")
	restart.Del("projection_id")
	p := &networkPage{Section: section, Table: query.Get("table"), Start: base + section + "?" + restart.Encode()}
	var observed network.ObservationPage
	if err := client.Do(ctx, http.MethodGet, "/v1/network/"+section+"?"+query.Encode(), nil, &observed); err != nil {
		return p, err
	}
	p.ObservationHeader, p.Total, p.Offset = observed.ObservationHeader, observed.Total, observed.Offset
	for _, item := range observed.Items {
		switch section {
		case "routes":
			var route network.RouteObservation
			if err := json.Unmarshal(item, &route); err != nil {
				return nil, err
			}
			p.Routes = append(p.Routes, route)
		case "policy-rules":
			var rule network.PolicyRuleState
			if err := json.Unmarshal(item, &rule); err != nil {
				return nil, err
			}
			p.Rules = append(p.Rules, rule)
		case "route-next-hops":
			var hop network.NextHop
			if err := json.Unmarshal(item, &hop); err != nil {
				return nil, err
			}
			p.NextHops = append(p.NextHops, hop)
		}
	}
	if observed.NextOffset != nil {
		query.Set("offset", strconv.Itoa(*observed.NextOffset))
		query.Set("projection_id", observed.ProjectionID)
		p.Next = base + section + "?" + query.Encode()
	}
	return p, nil
}
