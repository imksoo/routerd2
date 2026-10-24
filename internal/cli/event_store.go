package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/imksoo/routerd2/internal/localapi"
)

func showEventStore(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: show event-store")
	}
	var status json.RawMessage
	if err := client.Do(ctx, "GET", "/v1/event-store", nil, &status); err != nil {
		return err
	}
	return WriteJSON(out, status)
}
