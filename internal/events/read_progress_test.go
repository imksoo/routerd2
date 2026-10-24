package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestArchivedPageDecodeDoesNotHoldThePublicationConnection(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Append(ctx, testEvent("first")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ArchiveEvents(ctx, 16); err != nil {
		t.Fatal(err)
	}
	// Stall CPU-side body validation independently of SQLite. Live publication
	// must still acquire the single connection while this page is decoded.
	s.archiveCache.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.archiveCache.mu.Unlock()
		}
	}()
	type result struct {
		rows []Record
		err  error
	}
	read := make(chan result, 1)
	go func() { rows, err := s.List(ctx, 0, 16); read <- result{rows, err} }()
	// The blocked cache makes the validation stall indefinite. This head start
	// only lets the reader reach it; it is not a read-performance deadline.
	select {
	case got := <-read:
		t.Fatal("reader bypassed the validation stall", got.err)
	case <-time.After(50 * time.Millisecond):
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, writeErr := s.Append(writeCtx, testEvent("second"))
	s.archiveCache.mu.Unlock()
	locked = false
	got := <-read
	if writeErr != nil {
		t.Fatal("blocked archive decode held the live publication connection", writeErr)
	}
	if got.err != nil || len(got.rows) != 1 || got.rows[0].Event.ID != "first" {
		t.Fatal("reader lost its captured immutable page", got.rows, got.err)
	}
	rows, err := s.List(ctx, 0, 16)
	if err != nil || len(rows) != 2 || rows[1].Event.ID != "second" {
		t.Fatal("concurrent publication disappeared", rows, err)
	}
}
