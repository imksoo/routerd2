package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func showDerived(ctx context.Context, client *localapi.Client, args []string, out io.Writer) error {
	if len(args) != 1 || !resourceprogram.ValidResourceID(args[0]) {
		return errors.New("usage: show derived RESOURCE_ID")
	}
	var result json.RawMessage
	if err := client.Do(ctx, "GET", "/v1/derived/"+url.PathEscape(args[0]), nil, &result); err != nil {
		return err
	}
	return WriteJSON(out, result)
}
