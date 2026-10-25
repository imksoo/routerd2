package network

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFormerJournalCannotIgnoreVirtualLinkAuthority(t *testing.T) {
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
	old["version"] = 3
	before, _ := json.Marshal(old)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if restored, err := newEngine(opts, k); err == nil || !strings.Contains(err.Error(), "unsupported network journal version 3") {
		if restored != nil {
			restored.Close()
		}
		t.Fatal("former network journal format was not explicitly refused", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("unsupported journal was modified", err)
	}
}
