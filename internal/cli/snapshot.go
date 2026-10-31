package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
)

func snapshotConfiguration(ctx context.Context, client *localapi.Client, input io.Reader, out io.Writer) error {
	if input == nil {
		return errors.New("snapshot requires SnapshotRequest JSON/YAML on stdin")
	}
	var request control.SnapshotRequest
	if err := config.Decode(input, &request); err != nil {
		return err
	}
	var ref config.SnapshotRef
	if err := client.Do(ctx, http.MethodPost, "/v1/config/snapshots", request, &ref); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.Revision != request.ExpectedRevision {
		return errors.New("snapshot acknowledgment has a different revision")
	}
	return WriteJSON(out, ref)
}

// RestoreConfiguration uses the ordinary commit path through a verified saved
// reference; it does not replay saved runtime state or generate retry IDs.
func (s Session) RestoreConfiguration(ctx context.Context, request control.RestoreRequest) (control.State, error) {
	if request.ExpectedRevision == 0 || request.ExpectedRevision == ^uint64(0) || request.RequestID == "" {
		return control.State{}, errors.New("restore requires a positive, nonexhausted expected revision and request ID")
	}
	if err := request.Snapshot.Validate(); err != nil {
		return control.State{}, err
	}
	client := localapi.NewClient(filepath.Join(s.RuntimeDir, control.SocketName))
	defer client.HTTP.CloseIdleConnections()
	var state control.State
	if err := client.Do(ctx, http.MethodPost, "/v1/config/restore", request, &state); err != nil {
		return control.State{}, err
	}
	if state.Revision != request.ExpectedRevision+1 || state.LastRequestID != request.RequestID || state.StoreID != request.Snapshot.StoreID {
		return control.State{}, errors.New("restore acknowledgment does not match its request and next revision; retain the request ID when recovering")
	}
	if state.Confirmation != nil && (state.Confirmation.RequestID != request.RequestID || state.Confirmation.PreviousRevision != request.ExpectedRevision || request.ConfirmTimeoutSeconds == 0) {
		return control.State{}, errors.New("restore acknowledgment has a different confirmation transaction")
	}
	if state.ConfigOmitted {
		if err := checkMetadata(state); err != nil {
			return control.State{}, err
		}
	} else if err := config.Validate(state.Config); err != nil {
		return control.State{}, fmt.Errorf("invalid restored configuration acknowledgment: %w", err)
	}
	return state, nil
}

func deleteSnapshot(ctx context.Context, client *localapi.Client, input io.Reader, out io.Writer) error {
	if input == nil {
		return errors.New("snapshot delete requires SnapshotDeleteRequest JSON/YAML on stdin")
	}
	var request control.SnapshotDeleteRequest
	if err := config.Decode(input, &request); err != nil {
		return err
	}
	var deleted config.SnapshotRef
	if err := client.Do(ctx, http.MethodPost, "/v1/config/snapshots/delete", request, &deleted); err != nil {
		return err
	}
	if deleted != request.Snapshot {
		return errors.New("snapshot deletion acknowledgment does not match the selected reference")
	}
	return WriteJSON(out, deleted)
}

func exportSnapshot(ctx context.Context, client *localapi.Client, input io.Reader, out io.Writer) error {
	if input == nil {
		return errors.New("snapshot export requires SnapshotRef JSON/YAML on stdin")
	}
	var ref config.SnapshotRef
	if err := config.Decode(input, &ref); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	var document config.Document
	if err := client.Do(ctx, http.MethodPost, "/v1/config/snapshots/export", ref, &document); err != nil {
		return err
	}
	data, err := config.YAML(document)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
