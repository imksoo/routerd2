package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/runtimeplan"
)

type observationTransport struct{ handler http.Handler }

func (t observationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func handlerClient(handler http.Handler) *localapi.Client {
	return &localapi.Client{HTTP: &http.Client{Transport: observationTransport{handler: handler}}}
}

func callView(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(encoded)))
	return w
}

func largeViewDocument(t *testing.T) config.Document {
	t.Helper()
	d := config.Document{SchemaVersion: config.SchemaVersion, Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0"}}}}
	for i := range 12000 {
		d.Network.Routes = append(d.Network.Routes, network.Route{Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255), Gateway: "192.0.2.1", Interface: "wan0", Metric: 100, Table: 254})
	}
	if err := config.Validate(d); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil || len(b) <= localapi.MaxBody/2 || len(b) > config.MaxDocumentSize {
		t.Fatal("large configuration fixture is not valid and representative", len(b), err)
	}
	return d
}

func TestConfigurationLargeConfirmedACKDocumentFenceAndLightHealth(t *testing.T) {
	ctx := context.Background()
	e, n := fixture(t)
	doc := largeViewDocument(t)
	first, err := e.Commit(ctx, CommitRequest{ExpectedRevision: 1, RequestID: "large-base", Config: doc})
	if err != nil || first.Revision != 2 {
		t.Fatal(first.Revision, err)
	}
	req := CommitRequest{ExpectedRevision: 2, RequestID: "large-confirmed", Config: doc, ConfirmTimeoutSeconds: 30}
	h := e.Handler()
	ack := callView(t, h, http.MethodPost, "/v1/config/commit", req)
	if ack.Code != http.StatusOK || ack.Body.Len() > 4096 {
		t.Fatalf("successful apply looked like a failed transport: status=%d bytes=%d", ack.Code, ack.Body.Len())
	}
	var state State
	if err := json.Unmarshal(ack.Body.Bytes(), &state); err != nil || state.Revision != 3 || state.LastRequestID != req.RequestID || !state.ConfigOmitted || !state.ConfirmationPreviousOmitted || state.Config.SchemaVersion != 0 || state.Confirmation == nil || state.Confirmation.Previous.SchemaVersion != 0 {
		t.Fatal("large success acknowledgement does not distinguish omitted data", state, err)
	}
	canonical, _ := json.Marshal(doc)
	digest := sha256.Sum256(canonical)
	if state.ConfigBytes != len(canonical) || state.ConfigDigest != hex.EncodeToString(digest[:]) {
		t.Fatal("success acknowledgement cannot be bound to the candidate")
	}
	full, _ := json.Marshal(e.State())
	if len(full) <= localapi.MaxBody {
		t.Fatal("fixture no longer reproduces the old oversized state response")
	}
	if saved, err := e.store.Load(); err != nil || saved.Revision != 3 || saved.Confirmation == nil {
		t.Fatal("success ACK was not backed by durable confirmed state", err)
	}
	legacy := callView(t, h, http.MethodGet, "/v1/config", nil)
	if legacy.Code != http.StatusRequestEntityTooLarge || legacy.Body.Len() > 1024 {
		t.Fatal("oversized legacy state did not explicitly reject", legacy.Code, legacy.Body.Len())
	}
	metadata := callView(t, h, http.MethodGet, "/v1/config/metadata", nil)
	if metadata.Code != http.StatusOK || metadata.Body.Len() > 4096 {
		t.Fatal("metadata grew with document bodies", metadata.Code, metadata.Body.Len())
	}
	var meta State
	if err := json.Unmarshal(metadata.Body.Bytes(), &meta); err != nil || !meta.ConfigOmitted || meta.ConfigDigest != state.ConfigDigest || meta.Revision != 3 {
		t.Fatal(meta, err)
	}
	document := callView(t, h, http.MethodGet, "/v1/config/document?revision=3", nil)
	if document.Code != http.StatusOK || !bytes.Equal(document.Body.Bytes(), canonical) {
		t.Fatal("fenced document is not the exact canonical body", document.Code)
	}
	if got := callView(t, h, http.MethodGet, "/v1/config/document?revision=2", nil); got.Code != http.StatusConflict {
		t.Fatal("old document revision was served", got.Code)
	}
	for _, query := range []string{"", "?revision=0", "?revision=3&revision=3", "?revision=3&unknown=x", "?revision=3&bad=%zz", "?revision=-1"} {
		if got := callView(t, h, http.MethodGet, "/v1/config/document"+query, nil); got.Code != http.StatusBadRequest {
			t.Fatal("invalid document query accepted", query, got.Code)
		}
	}
	health := callView(t, h, http.MethodGet, "/v1/health", nil)
	if health.Code != http.StatusOK || health.Body.Len() > 4096 || bytes.Contains(health.Body.Bytes(), []byte(`"config":`)) || bytes.Contains(health.Body.Bytes(), []byte(`"previous":`)) {
		t.Fatal("health copied document bodies", health.Code, health.Body.Len())
	}
	var watch State
	if err := json.Unmarshal(health.Body.Bytes(), &watch); err != nil || watch.Revision != 3 || !watch.Healthy || watch.Confirmation == nil || watch.Confirmation.PreviousRevision != 2 || watch.Sequence == 0 || watch.ProducerEpoch == "" {
		t.Fatal("legacy boot-watch health fields were lost", watch, err)
	}
	before := len(n.calls)
	replay := callView(t, h, http.MethodPost, "/v1/config/commit", req)
	if replay.Code != http.StatusOK || replay.Body.Len() > 4096 || len(n.calls) != before {
		t.Fatal("retry after bounded success repeated the apply", replay.Code, len(n.calls), before)
	}
	// A long retained diagnostic can also make confirm's full State too large.
	// Confirmation remains successful, with a bounded and explicit success ACK.
	e.mu.Lock()
	e.state.LastError = strings.Repeat("<", localapi.MaxBody)
	e.mu.Unlock()
	confirmed := callView(t, h, http.MethodPost, "/v1/config/confirm", ConfirmRequest{Revision: 3, RequestID: req.RequestID})
	if confirmed.Code != http.StatusOK || confirmed.Body.Len() > 32<<10 {
		t.Fatal("successful confirm exceeded its response bound", confirmed.Code, confirmed.Body.Len())
	}
	var confirmation State
	if err := json.Unmarshal(confirmed.Body.Bytes(), &confirmation); err != nil || confirmation.Confirmation != nil || confirmation.ConfirmationPreviousOmitted || !confirmation.ConfigOmitted || !confirmation.LastErrorTruncated || confirmation.ConfigDigest != state.ConfigDigest || confirmation.LastRequestID != req.RequestID {
		t.Fatal("confirmation ACK lost identity or omission information", err)
	}
	saved, err := e.store.Load()
	if err != nil || saved.Revision != 3 || saved.Confirmation != nil {
		t.Fatal("confirmation ACK preceded durable save", err)
	}
}

func TestConfigurationSmallLegacyResponseAndHealthFailureFields(t *testing.T) {
	e, _ := fixture(t)
	h := e.Handler()
	get := callView(t, h, http.MethodGet, "/v1/config", nil)
	var state State
	if err := json.Unmarshal(get.Body.Bytes(), &state); err != nil || get.Code != http.StatusOK || state.ConfigOmitted || state.Config.SchemaVersion != 1 || state.Revision != 1 {
		t.Fatal("small legacy response changed", get.Code, state, err)
	}
	commit := callView(t, h, http.MethodPost, "/v1/config/commit", request(e, "small"))
	if err := json.Unmarshal(commit.Body.Bytes(), &state); err != nil || commit.Code != http.StatusOK || state.ConfigOmitted || state.Config.SchemaVersion != 1 || state.Revision != 2 {
		t.Fatal("small mutation response changed", commit.Code, state, err)
	}
	e.runtime = runtimeplan.New(t.TempDir(), t.TempDir(), &fakeNetwork{})
	bad := callView(t, e.Handler(), http.MethodGet, "/v1/health", nil)
	var health Health
	if err := json.Unmarshal(bad.Body.Bytes(), &health); err != nil || bad.Code != http.StatusServiceUnavailable || health.Healthy || health.Services == nil || health.Services.Phase != "starting" || health.Services.Revision != 0 {
		t.Fatal("unready service projection was declared healthy", bad.Code, health, err)
	}
	metadata := callView(t, e.Handler(), http.MethodGet, "/v1/config/metadata", nil)
	var meta State
	if err := json.Unmarshal(metadata.Body.Bytes(), &meta); err != nil || !meta.ServicesOmitted || !meta.ConfigOmitted || meta.Services == nil {
		t.Fatal("service summary silently replaced full service state", meta, err)
	}
}

type detailNetwork struct {
	observedNetwork
	query                      network.ObservationQuery
	detailErr                  error
	sourceCalls, snapshotCalls int
}

func (n *detailNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	n.snapshotCalls++
	return n.snapshot, nil
}
func (n *detailNetwork) ObservationPage(_ context.Context, query network.ObservationQuery) (network.ObservationPage, error) {
	n.query = query
	if n.detailErr != nil {
		return network.ObservationPage{}, n.detailErr
	}
	if query.ProjectionID != "" && query.ProjectionID != strings.Repeat("a", 64) {
		return network.ObservationPage{}, network.ErrObservationChanged
	}
	next := query.Offset + 1
	return network.ObservationPage{ObservationHeader: network.ObservationHeader{Revision: 7, AttemptedRevision: 7, Status: "applied", ObservedAt: time.Now().UTC(), ProjectionID: strings.Repeat("a", 64)}, Section: query.Section, Total: 100, Offset: query.Offset, NextOffset: &next, Items: []json.RawMessage{json.RawMessage(`{"address":"2001:db8::1/64","owned":true}`)}}, nil
}
func (n *detailNetwork) DesiredObservation(_ context.Context, projection string, offset, limit int) (network.DesiredPage, error) {
	if n.detailErr != nil {
		return network.DesiredPage{}, n.detailErr
	}
	if projection != "" && projection != strings.Repeat("a", 64) {
		return network.DesiredPage{}, network.ErrObservationChanged
	}
	return network.DesiredPage{ObservationHeader: network.ObservationHeader{Revision: 7, ProjectionID: strings.Repeat("a", 64), ObservedAt: time.Now().UTC()}, TotalBytes: 2, Offset: offset, Data: []byte("{}")}, nil
}
func (n *detailNetwork) SourceAddress(_ context.Context, name, address string) (network.SourceObservation, error) {
	n.sourceCalls++
	return network.SourceObservation{ObservationHeader: network.ObservationHeader{Revision: 7, ObservedAt: time.Now().UTC()}, Interface: name, RequestedAddress: address, InterfacePresent: true, Index: 9, Up: true, Found: true, Address: &network.AddressState{Address: address + "/64", Owned: true, PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}, n.detailErr
}

func TestNetworkObservationHTTPBoundsClientAndControlForwarding(t *testing.T) {
	backend := &detailNetwork{}
	for i := range 16000 {
		route := network.Route{Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255), Interface: "wan0", Table: 254}
		backend.snapshot.Routes = append(backend.snapshot.Routes, network.RouteState{Route: route, Protocol: 99, Type: 1})
		backend.snapshot.Desired.Routes = append(backend.snapshot.Desired.Routes, route)
	}
	encoded, _ := json.Marshal(backend.snapshot)
	if len(encoded) <= localapi.MaxBody {
		t.Fatal("full snapshot fixture must exceed transport cap", len(encoded))
	}
	worker := networkHandler(backend)
	client := &NetworkClient{client: handlerClient(worker)}
	value, err := client.Observe(context.Background())
	if err != nil || value.Complete || !value.Truncated || value.Counts.Routes != 16000 || !value.Truncation.Routes || !value.Truncation.Desired {
		t.Fatal("bounded client silently lost collections", value.Counts, value.Truncation, err)
	}
	if _, err := client.Snapshot(context.Background()); err == nil {
		t.Fatal("legacy full Snapshot accepted a truncated preview")
	}
	e, _ := fixture(t)
	e.network = client
	control := e.Handler()
	response := callView(t, control, http.MethodGet, "/v1/network", nil)
	if response.Code != http.StatusOK || response.Body.Len() > network.ObservationResponseLimit {
		t.Fatal("control network proxy re-expanded the response", response.Code, response.Body.Len())
	}
	var forwarded network.ObservationView
	if err := json.Unmarshal(response.Body.Bytes(), &forwarded); err != nil || !forwarded.Truncated || forwarded.Counts.Routes != 16000 {
		t.Fatal("control dropped truncation/count metadata", err)
	}
	page := callView(t, control, http.MethodGet, "/v1/network/addresses?interface=wan0&offset=7&limit=13&projection_id="+strings.Repeat("a", 64), nil)
	var detail network.ObservationPage
	if err := json.Unmarshal(page.Body.Bytes(), &detail); err != nil || page.Code != http.StatusOK || detail.Offset != 7 || detail.NextOffset == nil || *detail.NextOffset != 8 || detail.Total != 100 || backend.query.Interface != "wan0" || backend.query.Limit != 13 {
		t.Fatal("detail query was not forwarded faithfully", page.Code, detail, backend.query, err)
	}
	desired := callView(t, control, http.MethodGet, "/v1/network/desired?offset=0&limit=1024", nil)
	var desiredPage network.DesiredPage
	if err := json.Unmarshal(desired.Body.Bytes(), &desiredPage); err != nil || desired.Code != http.StatusOK || !bytes.Equal(desiredPage.Data, []byte("{}")) || desiredPage.TotalBytes != 2 {
		t.Fatal("desired byte page contract changed", desired.Code, desiredPage, err)
	}
	before := backend.snapshotCalls
	source, err := client.SourceAddress(context.Background(), "wan0", "2001:db8::1")
	if err != nil || source.Address == nil || !source.Address.Owned || backend.sourceCalls != 1 || backend.snapshotCalls != before {
		t.Fatal("minimal source query consulted full observation", source, err)
	}
	for _, check := range []struct {
		err  error
		code int
	}{{network.ErrObservationChanged, 409}, {network.ErrObservationNotFound, 404}, {network.ErrObservationTooLarge, 413}, {errors.New("kernel observation failed"), 503}} {
		backend.detailErr = check.err
		got := callView(t, control, http.MethodGet, "/v1/network/addresses?interface=wan0", nil)
		if got.Code != check.code {
			t.Fatal("typed worker error lost through control proxy", got.Code, check.code, got.Body.String())
		}
	}
	backend.detailErr = nil
	for _, path := range []string{
		"/v1/network/unknown", "/v1/network/addresses", "/v1/network/addresses?interface=wan0&interface=wan1", "/v1/network/addresses?interface=wan0&bad=%zz", "/v1/network/routes?interface=wan0", "/v1/network/routes?table=-1", "/v1/network/routes?table=4294967296", "/v1/network/routes?limit=0", "/v1/network/routes?limit=257", "/v1/network/routes?offset=-1", "/v1/network/routes?offset=9223372036854775808", "/v1/network/routes?projection_id=abc", "/v1/network/wireguard-allowed-ips?interface=wg0&public_key=invalid", "/v1/network/desired?limit=65537", "/v1/network/desired?table=254", "/v1/network/desired?offset=2097153",
	} {
		if got := callView(t, control, http.MethodGet, path, nil); got.Code != http.StatusBadRequest {
			t.Fatal("invalid observation query accepted", path, got.Code)
		}
	}
	for _, path := range []string{"/v1/source", "/v1/source?interface=wan0&address=::", "/v1/source?interface=wan0&address=ff02::1", "/v1/source?interface=wan0&address=2001:db8::1&offset=0", "/v1/source?interface=wan0&address=2001:db8::1&address=2001:db8::2"} {
		if got := callView(t, worker, http.MethodGet, path, nil); got.Code != http.StatusBadRequest {
			t.Fatal("invalid source query accepted", path, got.Code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.SourceAddress(ctx, "wan0", "2001:db8::1"); !errors.Is(err, context.Canceled) || backend.sourceCalls != 1 {
		t.Fatal("canceled source query was sent", err)
	}
}

func TestNetworkClientSmallSnapshotPreservesLegacyFields(t *testing.T) {
	snapshot := network.Snapshot{Revision: 9, AttemptedRevision: 9, Status: "applied", Desired: network.Desired{Interfaces: []network.Interface{{Name: "wan0"}}}}
	client := &NetworkClient{client: handlerClient(networkHandler(&observedNetwork{snapshot: snapshot}))}
	got, err := client.Snapshot(context.Background())
	if err != nil || !reflect.DeepEqual(got, snapshot) {
		t.Fatal("legacy complete Snapshot fields changed", got, err)
	}
}
