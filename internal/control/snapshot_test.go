package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestSnapshotSerializesConfirmedRevisionWithoutKernelApply(t *testing.T) {
	e, n := fixture(t)
	identity := e.State().StoreID
	if e.health().StoreID != identity {
		t.Fatal("bounded health omitted the store identity")
	}
	before := len(n.calls)
	ref, err := e.Snapshot(SnapshotRequest{ExpectedRevision: 1})
	if err != nil || ref.StoreID != identity || len(n.calls) != before {
		t.Fatal("capture mutated running configuration", err)
	}
	if _, err := e.Snapshot(SnapshotRequest{ExpectedRevision: 0}); !errors.Is(err, ErrConflict) {
		t.Fatal("missing revision accepted", err)
	}
	r := request(e, "unconfirmed")
	r.ConfirmTimeoutSeconds = 60
	trial, err := e.Commit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if trial.StoreID != identity {
		t.Fatal("commit changed store identity")
	}
	if _, err := e.Snapshot(SnapshotRequest{ExpectedRevision: trial.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatal("unconfirmed revision was captured", err)
	}
	if err := e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.State().StoreID != identity {
		t.Fatal("confirmed rollback changed store identity")
	}
	if _, err := e.store.ResolveSnapshot(ref); err != nil {
		t.Fatal("rollback changed saved snapshot", err)
	}
	if _, err := e.Snapshot(SnapshotRequest{ExpectedRevision: e.State().Revision}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAPIRequiresCurrentRevision(t *testing.T) {
	e, _ := fixture(t)
	for _, test := range []struct {
		body string
		code int
	}{
		{`{"expected_revision":1}`, 200},
		{`{"expected_revision":1}`, 200},
		{`{"expected_revision":0}`, 409},
		{`{"expected_revision":2}`, 409},
		{`{"expected_revision":1,"path":"/tmp/snapshot"}`, 400},
	} {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/config/snapshots", strings.NewReader(test.body)))
		if w.Code != test.code {
			t.Fatalf("snapshot status %d, wanted %d: %s", w.Code, test.code, w.Body)
		}
		if w.Code == 200 {
			var ref config.SnapshotRef
			if err := json.Unmarshal(w.Body.Bytes(), &ref); err != nil || ref.StoreID != e.State().StoreID {
				t.Fatal("bad snapshot acknowledgment", err)
			}
		}
	}
}
