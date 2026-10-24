package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

type configurationClientFunc func(context.Context, string, string, any, any) error

func (f configurationClientFunc) Do(ctx context.Context, method, path string, in, out any) error {
	return f(ctx, method, path, in, out)
}

// Exercise the public HTTP handler, encoding and client response bound without
// opening sockets or accessing the host's network namespace.
type handlerTransport struct {
	handler  http.Handler
	requests []string
	statuses []int
}

func (t *handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	t.requests = append(t.requests, r.Method+" "+r.URL.RequestURI())
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	t.statuses = append(t.statuses, w.Code)
	return w.Result(), nil
}

type configurationNetwork struct{}

func (configurationNetwork) Apply(context.Context, uint64, network.Desired) error { return nil }
func (configurationNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	return network.Snapshot{}, nil
}

func cliLargeDocument(routes int) config.Document {
	doc := config.Document{SchemaVersion: 1, Network: network.Desired{Interfaces: []network.Interface{{Name: "eth0"}}}}
	for i := 0; i < routes; i++ {
		doc.Network.Routes = append(doc.Network.Routes, network.Route{Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255), Gateway: "192.0.2.1", Interface: "eth0", Metric: 100, Table: 254})
	}
	return doc
}

func cliMetadata(t *testing.T, revision uint64, doc config.Document) control.State {
	t.Helper()
	digest, size, err := documentIdentity(doc)
	if err != nil {
		t.Fatal(err)
	}
	return control.State{Committed: config.Committed{Revision: revision}, ConfigOmitted: true, ConfigDigest: digest, ConfigBytes: size}
}

func TestLargeConfigurationPublicAPIRoundTrip(t *testing.T) {
	ctx := context.Background()
	baseline := cliLargeDocument(12000)
	if err := config.Validate(baseline); err != nil {
		t.Fatal(err)
	}
	_, size, _ := documentIdentity(baseline)
	if size <= localapi.MaxBody/2 || size > config.MaxDocumentSize {
		t.Fatal("fixture must fit one document but exceed the response bound when doubled", size)
	}
	store := &config.Store{Dir: t.TempDir()}
	current := config.Committed{Revision: 1, Config: baseline}
	if err := store.Save(current); err != nil {
		t.Fatal(err)
	}
	engine := control.New(store, current, configurationNetwork{})
	transport := &handlerTransport{handler: engine.Handler()}
	client := &localapi.Client{HTTP: &http.Client{Transport: transport}}
	candidate, err := cloneDocument(baseline)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Network.Interfaces[0].MTU = 1400
	request := control.CommitRequest{ExpectedRevision: 1, RequestID: "large-cli-confirmed", Config: candidate, ConfirmTimeoutSeconds: 60}
	state, err := commitConfiguration(ctx, client, request)
	if err != nil || state.Revision != 2 || state.ConfigOmitted || !state.ConfirmationPreviousOmitted || documentsDiffer(state.Config, candidate) {
		t.Fatalf("large commit acknowledgment did not restore the verified candidate: revision=%d omitted=%v previous-omitted=%v error=%v", state.Revision, state.ConfigOmitted, state.ConfirmationPreviousOmitted, err)
	}
	if len(transport.requests) != 1 || transport.statuses[0] != http.StatusOK {
		t.Fatal("large successful commit must need one request and return 200", transport.requests, transport.statuses)
	}
	var output bytes.Buffer
	if err := WriteConfigurationState(&output, state); err != nil {
		t.Fatal(err)
	}
	var written map[string]json.RawMessage
	var confirmation map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &written); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(written["confirmation"], &confirmation); err != nil {
		t.Fatal(err)
	}
	if _, exists := confirmation["previous"]; exists || string(written["confirmation_previous_omitted"]) != "true" || len(written["config"]) == 0 {
		t.Fatal("CLI rendered an omitted previous document as a real zero-value configuration")
	}
	// A lost response can be recovered with the same candidate/request ID;
	// a second POST must not create revision 3 or perform a racing GET.
	if repeated, err := commitConfiguration(ctx, client, request); err != nil || repeated.Revision != 2 || len(transport.requests) != 2 {
		t.Fatal("large idempotent acknowledgment could not be recovered", err, transport.requests)
	}
	transport.requests, transport.statuses = nil, nil
	state, err = readConfiguration(ctx, client, true)
	if err != nil || state.Revision != 2 || state.ConfigOmitted || !state.ConfirmationPreviousOmitted || documentsDiffer(state.Config, candidate) {
		t.Fatal("large current configuration was not reconstructed exactly", err)
	}
	if want := []string{"GET /v1/config", "GET /v1/config/metadata", "GET /v1/config/document?revision=2"}; !reflect.DeepEqual(transport.requests, want) || !reflect.DeepEqual(transport.statuses, []int{413, 200, 200}) {
		t.Fatal("configuration did not use the explicit 413 fallback and revision fence", transport.requests, transport.statuses)
	}
	stored, err := store.Load()
	if err != nil || stored.Confirmation == nil || documentsDiffer(stored.Confirmation.Previous, baseline) || documentsDiffer(stored.Config, candidate) {
		t.Fatal("API acknowledgment lost durable current/previous documents", err)
	}
	transport.requests = nil
	metadata, err := readConfiguration(ctx, client, false)
	if err != nil || !metadata.ConfigOmitted || metadata.Confirmation == nil || len(transport.requests) != 2 {
		t.Fatal("confirmation identity should not download the large current document", err, transport.requests)
	}
	confirmed, err := confirmConfiguration(ctx, client, control.ConfirmRequest{Revision: metadata.Revision, RequestID: metadata.Confirmation.RequestID})
	if err != nil || confirmed.Confirmation != nil || confirmed.Revision != 2 || documentsDiffer(confirmed.Config, candidate) {
		t.Fatal("large pending configuration could not be confirmed", err)
	}
}

func TestConfigurationReadRevisionFenceRetries(t *testing.T) {
	doc := config.Document{SchemaVersion: 1}
	for _, conflicts := range []int{0, 1, 3} {
		t.Run(strconv.Itoa(conflicts), func(t *testing.T) {
			metadataReads, documentReads := 0, 0
			client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
				if method != http.MethodGet || in != nil {
					t.Fatal("a read attempted a mutation")
				}
				switch path {
				case "/v1/config":
					return &localapi.HTTPError{StatusCode: http.StatusRequestEntityTooLarge}
				case "/v1/config/metadata":
					metadataReads++
					return jsonCopy(out, cliMetadata(t, uint64(metadataReads+10), doc))
				default:
					documentReads++
					if want := "/v1/config/document?revision=" + strconv.Itoa(metadataReads+10); path != want {
						t.Fatalf("stale revision reused: %q, want %q", path, want)
					}
					if documentReads <= conflicts {
						return &localapi.HTTPError{StatusCode: http.StatusConflict}
					}
					return jsonCopy(out, doc)
				}
			})
			state, err := readConfiguration(context.Background(), client, true)
			if conflicts == 3 {
				if err == nil || !strings.Contains(err.Error(), "all 3 snapshot attempts") || metadataReads != 3 || documentReads != 3 {
					t.Fatal("revision conflicts were not bounded", err, metadataReads, documentReads)
				}
			} else if err != nil || state.Revision != uint64(11+conflicts) || state.ConfigOmitted || metadataReads != conflicts+1 || documentReads != conflicts+1 {
				t.Fatal("consistent retry did not replace its metadata", err, state.Revision, metadataReads, documentReads)
			}
		})
	}
}

func TestConfigurationReadRejectsCorruptOrUnmarkedMetadata(t *testing.T) {
	doc := config.Document{SchemaVersion: 1}
	cases := map[string]func(*control.State){
		"missing omission flag": func(s *control.State) { s.ConfigOmitted = false },
		"wrong digest":          func(s *control.State) { s.ConfigDigest = strings.Repeat("0", 64) },
		"wrong size":            func(s *control.State) { s.ConfigBytes++ },
		"oversized size":        func(s *control.State) { s.ConfigBytes = config.MaxDocumentSize + 1 },
		"invalid revision":      func(s *control.State) { s.Revision = 0 },
		"unmarked previous": func(s *control.State) {
			s.Confirmation = &config.Confirmation{PreviousRevision: 1, RequestID: "pending", Deadline: time.Now().Add(time.Minute)}
		},
		"nonexistent previous": func(s *control.State) { s.ConfirmationPreviousOmitted = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			metadata := cliMetadata(t, 2, doc)
			mutate(&metadata)
			requests := 0
			client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
				requests++
				switch path {
				case "/v1/config":
					return &localapi.HTTPError{StatusCode: 413}
				case "/v1/config/metadata":
					return jsonCopy(out, metadata)
				default:
					return jsonCopy(out, doc)
				}
			})
			if _, err := readConfiguration(context.Background(), client, true); err == nil || requests > 3 {
				t.Fatal("invalid metadata was accepted or retried", err, requests)
			}
		})
	}
}

func TestConfigurationReadDoesNotHideTransportOrOtherHTTPFailures(t *testing.T) {
	for _, failure := range []error{&localapi.HTTPError{StatusCode: 503}, context.Canceled, errors.New("module response exceeds size limit")} {
		requests := 0
		client := configurationClientFunc(func(context.Context, string, string, any, any) error {
			requests++
			return failure
		})
		if _, err := readConfiguration(context.Background(), client, true); !errors.Is(err, failure) || requests != 1 {
			t.Fatal("a non-413 failure was disguised as a metadata fallback", err, requests)
		}
	}
}

func TestMutationAcknowledgmentIdentityAndOmission(t *testing.T) {
	doc := config.Document{SchemaVersion: 1}
	request := control.CommitRequest{ExpectedRevision: 4, RequestID: "bounded-commit", Config: doc}
	for name, mutate := range map[string]func(*control.State){
		"valid":            func(*control.State) {},
		"wrong revision":   func(s *control.State) { s.Revision++ },
		"wrong request ID": func(s *control.State) { s.LastRequestID = "another" },
		"wrong digest":     func(s *control.State) { s.ConfigDigest = strings.Repeat("f", 64) },
		"wrong size":       func(s *control.State) { s.ConfigBytes++ },
		"unmarked omission": func(s *control.State) {
			s.ConfigOmitted = false
		},
		"wrong full document": func(s *control.State) {
			s.ConfigOmitted = false
			s.Config = config.Document{SchemaVersion: 1, Network: network.Desired{Interfaces: []network.Interface{{Name: "wrong0"}}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			state := cliMetadata(t, 5, doc)
			state.LastRequestID = request.RequestID
			mutate(&state)
			posts := 0
			client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
				posts++
				if method != http.MethodPost || path != "/v1/config/commit" {
					t.Fatal("mutation acknowledgment triggered a racing read or retry")
				}
				return jsonCopy(out, state)
			})
			got, err := commitConfiguration(context.Background(), client, request)
			if posts != 1 || name == "valid" && (err != nil || got.ConfigOmitted || documentsDiffer(got.Config, doc)) || name != "valid" && err == nil {
				t.Fatal("mutation acknowledgment contract was not enforced", err, posts)
			}
		})
	}
	metadata := cliMetadata(t, 5, doc)
	metadata.LastRequestID = request.RequestID
	client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
		if method != http.MethodPost || path != "/v1/config/confirm" {
			t.Fatal("unexpected confirmation API call")
		}
		return jsonCopy(out, metadata)
	})
	confirmed, err := confirmConfiguration(context.Background(), client, control.ConfirmRequest{Revision: 5, RequestID: request.RequestID})
	if err != nil || !confirmed.ConfigOmitted {
		t.Fatal("bounded confirmation acknowledgment was rejected or invented a document", err)
	}
	var output bytes.Buffer
	if err := WriteConfigurationState(&output, confirmed); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil || fields["config"] != nil || string(fields["config_omitted"]) != "true" {
		t.Fatal("bounded confirmation must retain the explicit absence of its configuration", err)
	}
}

func TestInvalidLocalMutationDoesNotReachAPI(t *testing.T) {
	client := configurationClientFunc(func(context.Context, string, string, any, any) error {
		t.Fatal("invalid local request reached the mutation API")
		return nil
	})
	for _, request := range []control.CommitRequest{
		{RequestID: "missing-revision", Config: config.Document{SchemaVersion: 1}},
		{ExpectedRevision: 1, Config: config.Document{SchemaVersion: 1}},
		{ExpectedRevision: 1, RequestID: "invalid-document", Config: config.Document{SchemaVersion: 9}},
		{ExpectedRevision: 1, RequestID: "oversized-document", Config: cliLargeDocument(30000)},
	} {
		if _, err := commitConfiguration(context.Background(), client, request); err == nil {
			t.Fatal("invalid local commit accepted")
		}
	}
	if _, err := confirmConfiguration(context.Background(), client, control.ConfirmRequest{}); err == nil {
		t.Fatal("invalid local confirmation accepted")
	}
}

func TestInteractiveCandidateRetainsExplicitSizeLimit(t *testing.T) {
	doc := cliLargeDocument(1500)
	client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
		return jsonCopy(out, control.State{Committed: config.Committed{Revision: 1, Config: doc}})
	})
	shell := &interactiveShell{client: client}
	if _, err := shell.execute(context.Background(), "configure", io.Discard, nil); err == nil || shell.candidate != nil {
		t.Fatal("oversized interactive candidate was silently accepted")
	}
	var out bytes.Buffer
	if _, err := shell.execute(context.Background(), "show running-config", &out, nil); err != nil || !strings.Contains(out.String(), "10.0.5.219/32") {
		t.Fatal("interactive edit limit must not prevent exporting the complete running configuration", err)
	}
}
