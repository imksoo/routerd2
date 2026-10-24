package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

type networkAPIFixture struct {
	snapshot network.Snapshot
	queries  []network.ObservationQuery
}

func (f *networkAPIFixture) Apply(context.Context, uint64, network.Desired) error { return nil }
func (f *networkAPIFixture) Snapshot(context.Context) (network.Snapshot, error) {
	return f.snapshot, nil
}
func (f *networkAPIFixture) Observe(context.Context) (network.ObservationView, error) {
	return network.BoundSnapshot(f.snapshot)
}
func (f *networkAPIFixture) header() network.ObservationHeader {
	view, err := network.BoundSnapshot(f.snapshot)
	if err != nil {
		panic(err)
	}
	return network.ObservationHeader{Revision: view.Revision, AttemptedRevision: view.AttemptedRevision, Status: view.Status, ObservedAt: view.ObservedAt, ProjectionID: view.ProjectionID}
}
func (f *networkAPIFixture) ObservationPage(_ context.Context, q network.ObservationQuery) (network.ObservationPage, error) {
	f.queries = append(f.queries, q)
	header := f.header()
	if q.ProjectionID != "" && q.ProjectionID != header.ProjectionID {
		return network.ObservationPage{}, network.ErrObservationChanged
	}
	page := network.ObservationPage{ObservationHeader: header, Section: q.Section, Offset: q.Offset, Items: []json.RawMessage{}}
	if q.Section == "routes" {
		page.Total = len(f.snapshot.Routes)
		for i := q.Offset; i < page.Total && len(page.Items) < q.Limit; i++ {
			encoded, err := json.Marshal(f.snapshot.Routes[i])
			if err != nil {
				return page, err
			}
			page.Items = append(page.Items, encoded)
		}
		if next := q.Offset + len(page.Items); next < page.Total {
			page.NextOffset = &next
		}
	}
	return page, nil
}
func (f *networkAPIFixture) DesiredObservation(_ context.Context, projection string, offset, limit int) (network.DesiredPage, error) {
	header := f.header()
	if projection != "" && projection != header.ProjectionID {
		return network.DesiredPage{}, network.ErrObservationChanged
	}
	data, err := json.Marshal(f.snapshot.Desired)
	if err != nil {
		return network.DesiredPage{}, err
	}
	end := min(offset+limit, len(data))
	page := network.DesiredPage{ObservationHeader: header, TotalBytes: len(data), Offset: offset, Data: data[offset:end]}
	if end < len(data) {
		page.NextOffset = &end
	}
	return page, nil
}

func newNetworkAPIClient(f *networkAPIFixture) (*localapi.Client, *handlerTransport) {
	engine := control.New(nil, config.Committed{Revision: 1, Config: config.Document{SchemaVersion: 1}}, f)
	transport := &handlerTransport{handler: engine.Handler()}
	return &localapi.Client{HTTP: &http.Client{Transport: transport}}, transport
}

func TestNetworkPublicAPITruncationPagesAndDesired(t *testing.T) {
	doc := cliLargeDocument(12000)
	f := &networkAPIFixture{snapshot: network.Snapshot{Revision: 3, Status: "applied", Desired: doc.Network}}
	for _, route := range doc.Network.Routes {
		f.snapshot.Routes = append(f.snapshot.Routes, network.RouteState{Route: route, Owned: true})
	}
	client, transport := newNetworkAPIClient(f)
	var out bytes.Buffer
	if err := showNetwork(context.Background(), client, nil, &out); err != nil {
		t.Fatal(err)
	}
	var summary struct {
		Counts     network.ObservationCounts     `json:"counts"`
		Truncated  bool                          `json:"truncated"`
		Truncation network.ObservationTruncation `json:"truncation"`
		Desired    json.RawMessage               `json:"desired"`
		Notice     string                        `json:"notice"`
	}
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil || !summary.Truncated || !summary.Truncation.Desired || !summary.Truncation.Routes || summary.Counts.Routes != 12000 || summary.Desired != nil || summary.Notice == "" {
		t.Fatal("large network summary hid omissions or rendered an empty desired document", err)
	}
	out.Reset()
	if err := showNetwork(context.Background(), client, strings.Fields("routes table 254 limit 3"), &out); err != nil {
		t.Fatal(err)
	}
	var page struct {
		network.ObservationPage
		NextCommand string `json:"next_command"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil || page.Total != 12000 || len(page.Items) != 3 || page.NextOffset == nil || *page.NextOffset != 3 || !strings.Contains(page.NextCommand, "table 254 offset 3 limit 3 projection "+page.ProjectionID) {
		t.Fatal("first route page did not expose a pinned continuation", err, page.NextCommand)
	}
	out.Reset()
	if err := showNetwork(context.Background(), client, strings.Fields(page.NextCommand)[2:], &out); err != nil {
		t.Fatal(err)
	}
	var next network.ObservationPage
	if err := json.Unmarshal(out.Bytes(), &next); err != nil || next.Offset != 3 || len(next.Items) != 3 || next.ProjectionID != page.ProjectionID {
		t.Fatal("next_command did not request the next page of the same projection", err)
	}
	out.Reset()
	transport.requests = nil
	if err := showNetwork(context.Background(), client, []string{"desired"}, &out); err != nil {
		t.Fatal(err)
	}
	var full struct {
		network.ObservationHeader
		TotalBytes int             `json:"total_bytes"`
		Desired    network.Desired `json:"desired"`
	}
	if err := json.Unmarshal(out.Bytes(), &full); err != nil || len(full.Desired.Routes) != 12000 || !reflect.DeepEqual(full.Desired, doc.Network) || full.TotalBytes != summary.Counts.DesiredBytes {
		t.Fatal("large desired document was not reconstructed exactly", err)
	}
	if want := (full.TotalBytes + network.DesiredObservationChunk - 1) / network.DesiredObservationChunk; len(transport.requests) != want || want < 2 {
		t.Fatal("unexpected number of bounded desired requests", len(transport.requests), want)
	}
	for i, request := range transport.requests {
		parsed, err := url.Parse(strings.TrimPrefix(request, "GET "))
		if err != nil || parsed.Query().Get("offset") != strconv.Itoa(i*network.DesiredObservationChunk) || parsed.Query().Get("limit") != strconv.Itoa(network.DesiredObservationChunk) || i > 0 && parsed.Query().Get("projection_id") != full.ProjectionID {
			t.Fatal("desired chunks were not bounded and projection-fenced", request, err)
		}
	}
}

func TestNetworkDetailFiltersThroughPublicAPI(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{251}, 32))
	id := strings.Repeat("a", 64)
	f := &networkAPIFixture{snapshot: network.Snapshot{Revision: 1, Status: "applied"}}
	client, _ := newNetworkAPIClient(f)
	for _, command := range []string{
		"interfaces limit 256", "addresses interface eth0", "routes table 254",
		"route-next-hops route-id " + id + " table 254", "policy-rules", "wireguard",
		"wireguard-peers interface wg0", "wireguard-allowed-ips interface wg0 public-key " + key,
	} {
		var out bytes.Buffer
		if err := showNetwork(context.Background(), client, strings.Fields(command), &out); err != nil {
			t.Fatal(command, err)
		}
		query := f.queries[len(f.queries)-1]
		if query.Section != strings.Fields(command)[0] || query.Section == "wireguard-allowed-ips" && query.PublicKey != key {
			t.Fatal("section/filter was changed in transit, including base64 URL escaping", command, query)
		}
	}
	if len(f.queries) != 8 {
		t.Fatal("a supported detail command did not reach its API", f.queries)
	}
}

func TestNetworkInvalidFiltersRejectedBeforeRequest(t *testing.T) {
	client := configurationClientFunc(func(context.Context, string, string, any, any) error {
		t.Fatal("invalid network filters reached the API")
		return nil
	})
	for _, command := range []string{
		"missing", "routes offset", "routes offset -1", "routes offset 999999999999999999999",
		"routes limit 0", "routes limit 257", "routes limit 3 limit 4", "routes table 4294967296",
		"routes interface eth0", "interfaces table 0", "interfaces projection malformed",
		"addresses", "addresses interface ../eth0", "wireguard-peers", "wireguard-allowed-ips interface wg0",
		"wireguard-allowed-ips interface wg0 public-key malformed", "route-next-hops route-id malformed",
		"desired offset 0", "desired limit 65536", "help extra",
	} {
		var out bytes.Buffer
		if err := showNetwork(context.Background(), client, strings.Fields(command), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid network command accepted or emitted partial output", command, err)
		}
	}
}

func TestNetworkPageRejectsInconsistentContinuation(t *testing.T) {
	base := network.ObservationPage{ObservationHeader: network.ObservationHeader{Revision: 1, ObservedAt: time.Now(), ProjectionID: strings.Repeat("a", 64)}, Section: "routes", Total: 3, Items: []json.RawMessage{json.RawMessage(`{"destination":"192.0.2.0/24"}`)}}
	for name, mutate := range map[string]func(*network.ObservationPage){
		"missing next":       func(p *network.ObservationPage) { p.NextOffset = nil },
		"repeated next":      func(p *network.ObservationPage) { value := 0; p.NextOffset = &value },
		"skipped next":       func(p *network.ObservationPage) { value := 2; p.NextOffset = &value },
		"wrong section":      func(p *network.ObservationPage) { p.Section = "interfaces" },
		"wrong offset":       func(p *network.ObservationPage) { p.Offset++ },
		"wrong total":        func(p *network.ObservationPage) { p.Total = 0 },
		"oversized page":     func(p *network.ObservationPage) { p.Items = append(p.Items, p.Items[0]) },
		"missing projection": func(p *network.ObservationPage) { p.ProjectionID = "" },
		"missing timestamp":  func(p *network.ObservationPage) { p.ObservedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			page := base
			next := 1
			page.NextOffset = &next
			mutate(&page)
			client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error { return jsonCopy(out, page) })
			var out bytes.Buffer
			if err := showNetwork(context.Background(), client, []string{"routes", "limit", "1"}, &out); err == nil || out.Len() != 0 {
				t.Fatal("inconsistent page was displayed", err)
			}
		})
	}
}

func TestNetworkDesiredRejectsChangedOrMalformedPagesWithoutPartialOutput(t *testing.T) {
	desired := cliLargeDocument(1500).Network
	encoded, err := json.Marshal(desired)
	if err != nil || len(encoded) < 2*network.DesiredObservationChunk {
		t.Fatal("fixture must need multiple chunks", err, len(encoded))
	}
	base := network.ObservationHeader{Revision: 1, Status: "applied", ObservedAt: time.Now(), ProjectionID: strings.Repeat("a", 64)}
	for _, failure := range []string{"projection", "revision", "total", "oversized", "offset", "next", "missing-next", "short-chunk", "zero-chunk", "http-conflict"} {
		t.Run(failure, func(t *testing.T) {
			requests := 0
			client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
				requests++
				u, err := url.Parse(path)
				if err != nil {
					t.Fatal(err)
				}
				offset, _ := strconv.Atoi(u.Query().Get("offset"))
				end := min(offset+network.DesiredObservationChunk, len(encoded))
				page := network.DesiredPage{ObservationHeader: base, TotalBytes: len(encoded), Offset: offset, Data: encoded[offset:end]}
				if end < len(encoded) {
					page.NextOffset = &end
				}
				if requests == 2 {
					if u.Query().Get("projection_id") != base.ProjectionID {
						t.Fatal("second chunk was not projection-fenced")
					}
					switch failure {
					case "projection":
						page.ProjectionID = strings.Repeat("b", 64)
					case "revision":
						page.Revision++
					case "total":
						page.TotalBytes++
					case "oversized":
						page.TotalBytes = network.MaxDesiredObservationBytes + 1
					case "offset":
						page.Offset++
					case "next":
						page.NextOffset = &offset
					case "missing-next":
						page.NextOffset = nil
					case "short-chunk":
						page.Data = page.Data[:len(page.Data)-1]
					case "zero-chunk":
						page.Data = nil
					case "http-conflict":
						return &localapi.HTTPError{StatusCode: 409}
					}
				}
				return jsonCopy(out, page)
			})
			var out bytes.Buffer
			err := showNetwork(context.Background(), client, []string{"desired"}, &out)
			if err == nil || requests != 2 || out.Len() != 0 {
				t.Fatal("bad desired stream was accepted, continued or partially displayed", err, requests, out.Len())
			}
			if (failure == "http-conflict" || failure == "projection") && !errors.Is(err, network.ErrObservationChanged) {
				t.Fatal("configuration change did not request a new observation", err)
			}
		})
	}
}

func TestNetworkDesiredRejectsInvalidReconstructedJSON(t *testing.T) {
	for _, data := range []string{`{"unknown":true}`, `{"interfaces":[],"interfaces":[]}`, `{} {}`, `{"interfaces":[{"name":"../eth0"}]}`} {
		client := configurationClientFunc(func(_ context.Context, method, path string, in, out any) error {
			return jsonCopy(out, network.DesiredPage{ObservationHeader: network.ObservationHeader{ObservedAt: time.Now(), ProjectionID: strings.Repeat("a", 64)}, TotalBytes: len(data), Data: []byte(data)})
		})
		var out bytes.Buffer
		if err := showNetwork(context.Background(), client, []string{"desired"}, &out); err == nil || out.Len() != 0 {
			t.Fatal("malformed desired document displayed", data, err)
		}
	}
}

func TestNetworkCompletionAndHelp(t *testing.T) {
	shell := &interactiveShell{}
	for before, want := range map[string]string{
		"show network route-n":                "show network route-next-hops ",
		"show network wireguard-peers inter":  "show network wireguard-peers interface ",
		"show network routes table 254 off":   "show network routes table 254 offset ",
		"show network desired proj":           "show network desired projection ",
		"show network routes table 254 table": "show network routes table 254 table",
		"show network routes projection valu": "show network routes projection valu",
	} {
		got, _, handled := shell.complete(before, len(before), '\t')
		if !handled || got != want {
			t.Fatal("network completion mismatch", before, got, want)
		}
	}
	var out bytes.Buffer
	if err := (Session{}).Execute(context.Background(), "show network help", nil, &out); err != nil || !strings.Contains(out.String(), "Kernel observations remain live") {
		t.Fatal("network help requires an API socket or hides consistency limits", err)
	}
}
