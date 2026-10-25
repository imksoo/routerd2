package network

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestFormerJournalCannotIgnoreNewBootLifetimeAuthority(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	if _, err := e.Apply(context.Background(), 1, Desired{}); err != nil {
		t.Fatal(err)
	}
	path := opts.StatePath
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	old["version"] = 1
	before, _ := json.Marshal(old)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if restored, err := newEngine(opts, k); err == nil {
		restored.Close()
		t.Fatal("former address journal format was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("unsupported journal was modified", err)
	}
}
