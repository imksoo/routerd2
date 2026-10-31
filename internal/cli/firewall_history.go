// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

func showFirewallHistory(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) == 0 || len(args)%2 != 1 {
		return errors.New("usage: show firewall-events NAME [source IP] [action VALUE] [since RFC3339] [limit 1..1000]")
	}
	service := args[0]
	if !strings.HasPrefix(service, "service.") {
		service = "service." + service
	}
	query := url.Values{}
	for i := 1; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if query.Has(key) {
			return errors.New("duplicate firewall history filter")
		}
		switch key {
		case "source", "action", "since", "limit":
			query.Set(key, value)
		default:
			return errors.New("unknown firewall history filter")
		}
	}
	// The existing history API owns validation and bounded database reads.
	// Management never opens the database or changes its retention lifetime.
	var history runtimeplan.FirewallHistory
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(service)+"/firewall-history?"+query.Encode(), nil, &history); err != nil {
		return err
	}
	return WriteJSON(out, history)
}
