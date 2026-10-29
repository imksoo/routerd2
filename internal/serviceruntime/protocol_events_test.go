package serviceruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestProtocolSenderRequiresAnExplicitMatchingDurableACK(t *testing.T) {
	bad := true
	c := &effectsClient{identity: serviceconfig.Update{SupervisorInstance: strings.Repeat("a", 32), Name: "service.wan", Role: "health", ConfigDigest: strings.Repeat("b", 64), Generation: 7, ProducerEpoch: strings.Repeat("c", 32), Sequence: 99}, client: &localapi.Client{HTTP: &http.Client{Transport: bfdRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.Path != "/v1/protocol-events" {
			t.Fatal(r.Method, r.URL)
		}
		var batch protocolevents.Batch
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Fatal(err)
		}
		if batch.Resource != "wan" || len(batch.Events) != 1 || batch.Events[0].ID != "stable-id" {
			t.Fatal(batch)
		}
		id := "stable-id"
		if bad {
			id = "wrong-id"
		}
		body, _ := json.Marshal(protocolevents.Acknowledgement{IDs: []string{id}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}}}
	page := []events.Event{{ID: "stable-id"}}
	if err := c.sendProtocolEvents(context.Background(), "wan", page); err == nil {
		t.Fatal("wrong ACK discarded pending event")
	}
	bad = false
	if err := c.sendProtocolEvents(context.Background(), "wan", page); err != nil {
		t.Fatal(err)
	}
}
