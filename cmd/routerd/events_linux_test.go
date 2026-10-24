//go:build linux

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
)

type archiveTransport func(*http.Request) (*http.Response, error)

func (f archiveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func archiveClient(t *testing.T, fail bool) (*localapi.Client, *events.Store) {
	t.Helper()
	s, err := events.Open(events.Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, id := range []string{"one", "two"} {
		if _, err = s.Append(context.Background(), events.Event{ID: id, Topic: "routerd.fixture", Source: "fixture", ObservedAt: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}); err != nil {
			t.Fatal(err)
		}
	}
	client := &localapi.Client{HTTP: &http.Client{Transport: archiveTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.URL.Path == "/v1/event-store" {
			c, err := s.Capacity(r.Context())
			if err != nil {
				t.Fatal(err)
			}
			localapi.JSON(w, 200, control.EventStoreHealth{Capacity: &c})
		} else if fail {
			localapi.Fail(w, 503, fmt.Errorf("injected page failure"))
		} else {
			after, _ := strconv.ParseUint(r.URL.Query().Get("after_cursor"), 10, 64)
			rows, err := s.List(r.Context(), after, 16)
			if err != nil {
				t.Fatal(err)
			}
			next := after
			if len(rows) > 0 {
				next = rows[len(rows)-1].Cursor
			}
			localapi.JSON(w, 200, control.ObservationPage{Records: rows, NextCursor: next})
		}
		return w.Result(), nil
	})}}
	return client, s
}

func TestEventExportPublishesPrivateVerifiedFileAndPreservesExistingTargets(t *testing.T) {
	client, _ := archiveClient(t, false)
	root := t.TempDir()
	path := filepath.Join(root, "history.jsonl")
	summary, err := exportEventHistory(context.Background(), client, path)
	if err != nil || summary.Records != 2 {
		t.Fatal(summary, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	verified, err := events.VerifyHistory(context.Background(), file)
	if err != nil || verified != summary {
		t.Fatal(verified, err)
	}
	if info, err := file.Stat(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exportEventHistory(context.Background(), client, path); err == nil {
		t.Fatal("existing archive overwritten")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("existing archive changed")
	}
	link := filepath.Join(root, "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err = exportEventHistory(context.Background(), client, link); err == nil {
		t.Fatal("symlink target accepted")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("partial files leaked", entries)
	}
}

func TestFailedEventExportPublishesNothingAndDoesNotAcknowledgeHistory(t *testing.T) {
	client, s := archiveClient(t, true)
	root := t.TempDir()
	path := filepath.Join(root, "failed.jsonl")
	if _, err := exportEventHistory(context.Background(), client, path); err == nil {
		t.Fatal("failed page reported export success")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("failed export exposed partial file", entries)
	}
	rows, err := s.List(context.Background(), 0, 16)
	if err != nil || len(rows) != 2 {
		t.Fatal("failed export altered source", rows, err)
	}
	var out strings.Builder
	if err := eventCommand([]string{"verify", path}, &out); err == nil || out.Len() != 0 {
		t.Fatal("missing archive reported verified", err, out.String())
	}
}

func TestConcurrentOutputCreationIsNotOverwrittenAtArchivePublication(t *testing.T) {
	client, _ := archiveClient(t, false)
	root := t.TempDir()
	path := filepath.Join(root, "contested.jsonl")
	original := client.HTTP.Transport
	created := false
	client.HTTP.Transport = archiveTransport(func(r *http.Request) (*http.Response, error) {
		if !created && r.URL.Path == "/v1/observations" {
			created = true
			if err := os.WriteFile(path, []byte("competing operator output"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return original.RoundTrip(r)
	})
	if _, err := exportEventHistory(context.Background(), client, path); err == nil || !created {
		t.Fatal("archive publication replaced a concurrent output", err, created)
	}
	if body, err := os.ReadFile(path); err != nil || string(body) != "competing operator output" {
		t.Fatal("competing output changed", string(body), err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("failed publication leaked partial file", entries)
	}
}
