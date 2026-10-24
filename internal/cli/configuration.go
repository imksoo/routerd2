package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
)

const configurationReadAttempts = 3

func documentIdentity(document config.Document) (string, int, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", 0, err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), len(encoded), nil
}

func checkMetadata(state control.State) error {
	digest, err := hex.DecodeString(state.ConfigDigest)
	if state.Revision == 0 || !state.ConfigOmitted || err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != state.ConfigDigest || state.ConfigBytes < 1 || state.ConfigBytes > config.MaxDocumentSize {
		return errors.New("configuration metadata has invalid revision, omission flag, digest or size")
	}
	if state.ConfirmationPreviousOmitted != (state.Confirmation != nil) {
		return errors.New("configuration metadata has inconsistent confirmation document omission")
	}
	return nil
}

func checkDocument(document config.Document, digest string, size int) error {
	if err := config.Validate(document); err != nil {
		return fmt.Errorf("invalid configuration document returned by control: %w", err)
	}
	actual, count, err := documentIdentity(document)
	if err != nil {
		return err
	}
	if actual != digest || count != size {
		return errors.New("configuration document does not match its metadata digest and size")
	}
	return nil
}

// readConfiguration first preserves the old single-response API. Only an
// explicit 413 triggers metadata plus a separately fenced current document.
// A configuration revision changing between those reads starts a new attempt;
// transport, size, and digest failures are not disguised as ordinary retries.
func readConfiguration(ctx context.Context, client configurationClient, document bool) (control.State, error) {
	var state control.State
	err := client.Do(ctx, http.MethodGet, "/v1/config", nil, &state)
	if err == nil {
		if state.ConfigOmitted || state.Revision == 0 {
			return control.State{}, errors.New("full configuration response omitted its document or revision")
		}
		if err := config.Validate(state.Config); err != nil {
			return control.State{}, fmt.Errorf("invalid configuration document returned by control: %w", err)
		}
		return state, nil
	}
	if !localapi.IsHTTPStatus(err, http.StatusRequestEntityTooLarge) {
		return control.State{}, err
	}
	for attempt := 0; attempt < configurationReadAttempts; attempt++ {
		state = control.State{}
		if err := client.Do(ctx, http.MethodGet, "/v1/config/metadata", nil, &state); err != nil {
			return control.State{}, err
		}
		if err := checkMetadata(state); err != nil {
			return control.State{}, err
		}
		if !document {
			return state, nil
		}
		var current config.Document
		path := "/v1/config/document?revision=" + strconv.FormatUint(state.Revision, 10)
		if err := client.Do(ctx, http.MethodGet, path, nil, &current); err != nil {
			if localapi.IsHTTPStatus(err, http.StatusConflict) {
				continue
			}
			return control.State{}, err
		}
		if err := checkDocument(current, state.ConfigDigest, state.ConfigBytes); err != nil {
			return control.State{}, err
		}
		state.Config, state.ConfigOmitted = current, false
		return state, nil
	}
	return control.State{}, fmt.Errorf("configuration changed during all %d snapshot attempts; retry the read", configurationReadAttempts)
}

func commitConfiguration(ctx context.Context, client configurationClient, request control.CommitRequest) (control.State, error) {
	if request.ExpectedRevision == 0 || request.ExpectedRevision == ^uint64(0) {
		return control.State{}, errors.New("commit requires a positive, nonexhausted expected revision")
	}
	if request.RequestID == "" {
		return control.State{}, errors.New("commit requires a request ID")
	}
	if err := config.Validate(request.Config); err != nil {
		return control.State{}, err
	}
	digest, size, err := documentIdentity(request.Config)
	if err != nil {
		return control.State{}, err
	}
	var result control.State
	if err := client.Do(ctx, http.MethodPost, "/v1/config/commit", request, &result); err != nil {
		return control.State{}, err
	}
	if result.Revision != request.ExpectedRevision+1 || result.LastRequestID != request.RequestID || request.RequestID == "" {
		return control.State{}, errors.New("commit acknowledgment does not match its request ID and next revision; retain the request ID when recovering")
	}
	if result.Confirmation != nil && (result.Confirmation.RequestID != request.RequestID || result.Confirmation.PreviousRevision != request.ExpectedRevision || request.ConfirmTimeoutSeconds == 0) {
		return control.State{}, errors.New("commit acknowledgment has a different confirmation transaction")
	}
	if result.ConfigOmitted {
		if err := checkMetadata(result); err != nil {
			return control.State{}, err
		}
		if result.ConfigDigest != digest || result.ConfigBytes != size {
			return control.State{}, errors.New("commit acknowledgment does not match the submitted configuration digest and size")
		}
		// The acknowledgment identifies exactly the bytes this caller already
		// has. It needs no racing read of a later committed configuration.
		result.Config, result.ConfigOmitted = request.Config, false
	} else if err := checkDocument(result.Config, digest, size); err != nil {
		return control.State{}, fmt.Errorf("commit acknowledgment: %w", err)
	}
	return result, nil
}

func confirmConfiguration(ctx context.Context, client configurationClient, request control.ConfirmRequest) (control.State, error) {
	if request.Revision == 0 || request.RequestID == "" {
		return control.State{}, errors.New("confirm requires a positive revision and request ID")
	}
	var result control.State
	if err := client.Do(ctx, http.MethodPost, "/v1/config/confirm", request, &result); err != nil {
		return control.State{}, err
	}
	if request.Revision == 0 || request.RequestID == "" || result.Revision != request.Revision || result.LastRequestID != request.RequestID || result.Confirmation != nil {
		return control.State{}, errors.New("confirmation acknowledgment does not match the requested revision and transaction")
	}
	if result.ConfigOmitted {
		if err := checkMetadata(result); err != nil {
			return control.State{}, err
		}
	} else if err := config.Validate(result.Config); err != nil {
		return control.State{}, fmt.Errorf("invalid confirmation acknowledgment: %w", err)
	}
	return result, nil
}

// CommitConfiguration applies one caller-supplied idempotent request. Large
// acknowledgments are checked against that exact candidate before returning.
// The method never automatically retries a mutation with a new request ID.
func (s Session) CommitConfiguration(ctx context.Context, request control.CommitRequest) (control.State, error) {
	client := localapi.NewClient(filepath.Join(s.RuntimeDir, control.SocketName))
	return commitConfiguration(ctx, client, request)
}

// WriteConfigurationState preserves explicit omission flags without rendering
// zero-value placeholder documents or producer snapshots as actual data.
func WriteConfigurationState(out io.Writer, state control.State) error {
	if !state.ConfigOmitted && !state.ConfirmationPreviousOmitted && !state.ServicesOmitted {
		return WriteJSON(out, state)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	if state.ConfigOmitted {
		delete(fields, "config")
	}
	if state.ServicesOmitted {
		delete(fields, "services")
	}
	if state.ConfirmationPreviousOmitted && state.Confirmation != nil {
		var confirmation map[string]json.RawMessage
		if err := json.Unmarshal(fields["confirmation"], &confirmation); err != nil {
			return err
		}
		delete(confirmation, "previous")
		fields["confirmation"], err = json.Marshal(confirmation)
		if err != nil {
			return err
		}
	}
	return WriteJSON(out, fields)
}
