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

func showTrafficHistory(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) == 0 || len(args)%2 != 1 {
		return errors.New("usage: show traffic-flows NAME [client IP] [peer IP] [protocol NAME] [since RFC3339] [until RFC3339] [limit 1..1000]")
	}
	service := args[0]
	if !strings.HasPrefix(service, "service.") {
		service = "service." + service
	}
	query := url.Values{}
	for i := 1; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if query.Has(key) {
			return errors.New("duplicate traffic history filter")
		}
		switch key {
		case "client", "peer", "protocol", "since", "until", "limit":
			query.Set(key, value)
		default:
			return errors.New("unknown traffic history filter")
		}
	}
	// The existing collector owns storage; the history API owns filtering,
	// deadlines and limits. Management returns that same typed result.
	var history runtimeplan.TrafficHistory
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(service)+"/traffic-history?"+query.Encode(), nil, &history); err != nil {
		return err
	}
	return WriteJSON(out, history)
}
