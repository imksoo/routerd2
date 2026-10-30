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

func showDNSHistory(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) == 0 || len(args)%2 != 1 {
		return errors.New("usage: show dns-queries NAME [client IP] [name PATTERN] [response RCODE] [since RFC3339] [until RFC3339] [limit 1..1000]")
	}
	service := args[0]
	if !strings.HasPrefix(service, "service.") {
		service = "service." + service
	}
	query := url.Values{}
	for i := 1; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if query.Has(key) {
			return errors.New("duplicate DNS history filter")
		}
		switch key {
		case "client", "name", "response", "since", "until", "limit":
			query.Set(key, value)
		default:
			return errors.New("unknown DNS history filter")
		}
	}
	// The existing history owner validates and normalizes the filter, reads
	// its database and reports truncation. This command does not read files.
	var history runtimeplan.DNSHistory
	if err := client.Do(ctx, http.MethodGet, "/v1/services/"+url.PathEscape(service)+"/dns-history?"+query.Encode(), nil, &history); err != nil {
		return err
	}
	return WriteJSON(out, history)
}
