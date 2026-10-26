//go:build linux

package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/network"
)

func TestNetworkJournalAPIInspectsDiskAndDoesNotReturnCachedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	raw := []byte(`{"version":7,"revision":7,"attempted_revision":7,"status":"applied"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	// New only acquires/reads a journal. No Apply, Snapshot or kernel effect
	// runs in this test, even if its host has networking privileges.
	e, err := network.New(network.Options{StatePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	h := networkHandler(e)
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/journal", nil))
	var got network.JournalInspection
	sum := sha256.Sum256(raw)
	if err := json.Unmarshal(r.Body.Bytes(), &got); err != nil || r.Code != 200 || !got.Present || got.Version != network.JournalVersion || got.Revision != 7 || got.SHA256 != hex.EncodeToString(sum[:]) || r.Body.Len() > 1024 {
		t.Fatal("journal metadata", r.Code, r.Body.String(), err)
	}
	client := &NetworkClient{client: handlerClient(h)}
	observed, err := client.InspectJournal(context.Background())
	if err != nil || observed != got {
		t.Fatal("typed client lost disk metadata", err)
	}
	for _, query := range []string{"?path=/another/store", "?unknown=1"} {
		r = httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/journal"+query, nil))
		if r.Code != 400 {
			t.Fatal("journal endpoint accepted caller-selected inputs", r.Code)
		}
	}
	if err := os.WriteFile(path, []byte(`{"version":7,"version":7,"status":"applied"}`), 0600); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/journal", nil))
	if r.Code != 503 {
		t.Fatal("corrupt disk replaced by cached success", r.Code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/journal", nil))
	if r.Code != 503 {
		t.Fatal("lost established journal called a new empty store", r.Code)
	}
}
