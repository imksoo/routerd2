package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

func committedProgram(address, gateway string) config.Document {
	return config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "default-route", Owner: "wan", DependsOn: []string{"wan-address"}, Route: &network.Route{Destination: "0.0.0.0/0", Interface: "wan0", Gateway: gateway}},
		{ID: "wan-address", Owner: "wan", DependsOn: []string{"wan"}, Address: &resourceprogram.Address{Interface: "wan0", CIDR: address}},
		{ID: "wan", Interface: &network.Interface{Name: "wan0"}, Provenance: &resourceprogram.Provenance{Source: "startup-config", Resource: "wan", Revision: "operator-7"}},
	}}
}

func TestResourceSourceAndProjectionRollbackTogether(t *testing.T) {
	for _, recovery := range []string{"deadline", "restart"} {
		t.Run(recovery, func(t *testing.T) {
			ctx := context.Background()
			e, n := fixture(t)
			baseline := committedProgram("192.0.2.1/24", "192.0.2.2")
			if _, err := e.Commit(ctx, CommitRequest{ExpectedRevision: 1, RequestID: "baseline", Config: baseline}); err != nil {
				t.Fatal(err)
			}
			candidate := committedProgram("198.51.100.1/24", "198.51.100.2")
			candidate.Resources[2].Provenance.Revision = "operator-8"
			if _, err := e.Commit(ctx, CommitRequest{ExpectedRevision: 2, RequestID: "candidate", Config: candidate, ConfirmTimeoutSeconds: 5}); err != nil {
				t.Fatal(err)
			}
			saved, err := e.store.Load()
			if err != nil || saved.Confirmation == nil || !reflect.DeepEqual(saved.Config, candidate) || !reflect.DeepEqual(saved.Confirmation.Previous, baseline) {
				t.Fatal("journal lost source order, dependency, owner or provenance", err)
			}
			if recovery == "restart" {
				e = New(e.store, saved, n)
				err = e.Reconcile(ctx)
			} else {
				err = e.Expire(ctx, saved.Confirmation.Deadline.Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err = e.store.Load()
			if err != nil || saved.Revision != 4 || saved.Confirmation != nil || !reflect.DeepEqual(saved.Config, baseline) || !e.State().Healthy {
				t.Fatal("rollback did not restore the complete source as a new revision", err)
			}
			want, err := config.Compile(baseline)
			if err != nil || n.calls[len(n.calls)-1] != saved.Revision || !reflect.DeepEqual(n.desired[len(n.desired)-1], want.Native.Network) {
				t.Fatal("durable source and applied kernel projection diverged", err)
			}
			if _, err := os.Stat(filepath.Join(e.store.Dir, "pending.json")); !os.IsNotExist(err) {
				t.Fatal("recovered transaction retained a pending candidate", err)
			}
		})
	}
}

func TestInvalidResourceGraphDoesNotStageOrApply(t *testing.T) {
	e, n := fixture(t)
	before, err := os.ReadFile(filepath.Join(e.store.Dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate := committedProgram("192.0.2.1/24", "192.0.2.2")
	candidate.Resources[2].DependsOn = []string{"default-route"}
	if _, err := e.Commit(context.Background(), CommitRequest{ExpectedRevision: 1, RequestID: "cycle", Config: candidate}); err == nil {
		t.Fatal("cyclic source reached a configuration transaction")
	}
	after, err := os.ReadFile(filepath.Join(e.store.Dir, "config.json"))
	if err != nil || string(before) != string(after) || len(n.calls) != 1 || e.State().Revision != 1 {
		t.Fatal("invalid source changed durable or applied state", err)
	}
	if _, err := os.Stat(filepath.Join(e.store.Dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("invalid source was staged", err)
	}
}

func TestCommitAPIRejectsAmbiguousResourceFieldNames(t *testing.T) {
	e, n := fixture(t)
	for _, body := range []string{
		`{"expected_revision":1,"request_id":"alias","config":{"schema_version":2,"resources":[{"id":"wan","ID":"ignored","interface":{"name":"wan0"}}]}}`,
		`{"expected_revision":1,"request_id":"alias","config":{"schema_version":2,"resources":[{"id":"wan","interface":{"name":"wan0","MTU":1400,"mtu":1500}}]}}`,
		`{"expected_revision":1,"Expected_Revision":2,"request_id":"alias","config":{"schema_version":2,"resources":[]}}`,
	} {
		response := httptest.NewRecorder()
		e.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/config/commit", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest || len(n.calls) != 1 || e.State().Revision != 1 {
			t.Fatalf("ambiguous request mutated configuration: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(e.store.Dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("ambiguous request reached the journal", err)
	}
}
