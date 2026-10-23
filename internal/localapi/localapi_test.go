package localapi

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUnixServerAndProtectedPath(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, map[string]bool{"ok": true}) })
	go func() { done <- Serve(ctx, path, h, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("not ready")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %v %v", fi, err)
	}
	var response map[string]bool
	if err := NewClient(path).Do(ctx, "GET", "/", nil, &response); err != nil || !response["ok"] {
		t.Fatalf("request: %v %v", response, err)
	}
	if err := Serve(ctx, path, h, nil); err == nil {
		t.Fatal("replaced live socket")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(d, "regular")
	_ = os.WriteFile(regular, []byte("keep"), 0o600)
	if err := Serve(context.Background(), regular, h, nil); err == nil {
		t.Fatal("replaced regular file")
	}
}
