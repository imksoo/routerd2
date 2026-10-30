package cli

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
)

func update(ctx context.Context, client *localapi.Client, args []string, input io.Reader, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: update status|stage|prepare|rollback|cancel|prune (stage: signed tar; other mutations: JSON on stdin)")
	}
	if args[0] != "status" && input == nil {
		return errors.New("update mutation requires an exec request with input on stdin")
	}
	client.HTTP.Timeout = control.ReleaseOperationTimeout
	switch args[0] {
	case "status":
		var state release.State
		if err := client.Do(ctx, http.MethodGet, "/v1/releases/status", nil, &state); err != nil {
			return err
		}
		return WriteJSON(out, state)
	case "stage":
		// HTTP's response-header budget starts after the request body is sent.
		// The server separately bounds an idle upload and verification.
		client.HTTP.Timeout = 0
		client.HTTP.Transport.(*http.Transport).ResponseHeaderTimeout = control.ReleaseOperationTimeout
		var ref release.Ref
		if err := client.Upload(ctx, "/v1/releases/stage", input, &ref); err != nil {
			return err
		}
		return WriteJSON(out, ref)
	case "prepare":
		var request control.ReleasePrepareRequest
		if err := config.Decode(input, &request); err != nil {
			return err
		}
		var state release.State
		if err := client.Do(ctx, http.MethodPost, "/v1/releases/prepare", request, &state); err != nil {
			return err
		}
		return WriteJSON(out, state)
	case "rollback":
		var request control.ReleaseRollbackRequest
		if err := config.Decode(input, &request); err != nil {
			return err
		}
		var state release.State
		if err := client.Do(ctx, http.MethodPost, "/v1/releases/rollback", request, &state); err != nil {
			return err
		}
		return WriteJSON(out, state)
	case "cancel":
		var request control.ReleaseCancelRequest
		if err := config.Decode(input, &request); err != nil {
			return err
		}
		var state release.State
		if err := client.Do(ctx, http.MethodPost, "/v1/releases/cancel", request, &state); err != nil {
			return err
		}
		return WriteJSON(out, state)
	case "prune":
		var request control.ReleasePruneRequest
		if err := config.Decode(input, &request); err != nil {
			return err
		}
		var result release.CleanupResult
		if err := client.Do(ctx, http.MethodPost, "/v1/releases/prune", request, &result); err != nil {
			return err
		}
		return WriteJSON(out, result)
	default:
		return errors.New("unknown update command")
	}
}
