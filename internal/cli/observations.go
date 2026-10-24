package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
)

func showObservations(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	invalid := errors.New("usage: show observations [after CURSOR] [limit 1..16]")
	if len(args)%2 != 0 {
		return invalid
	}
	query := url.Values{"after_cursor": {"0"}, "limit": {"16"}}
	seen := map[string]bool{}
	for i := 0; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		if seen[key] {
			return invalid
		}
		seen[key] = true
		switch key {
		case "after":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil || n > math.MaxInt64 {
				return invalid
			}
			query.Set("after_cursor", strconv.FormatUint(n, 10))
		case "limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 16 {
				return invalid
			}
			query.Set("limit", strconv.Itoa(n))
		default:
			return invalid
		}
	}
	var result json.RawMessage
	if err := client.Do(ctx, http.MethodGet, "/v1/observations?"+query.Encode(), nil, &result); err != nil {
		return err
	}
	return WriteJSON(out, result)
}
