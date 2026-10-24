package events

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWarmArchivedPageHasBoundedDecodeAllocation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 16 {
		e := testEvent(string(rune('a' + i)))
		if _, err = s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.ArchiveEvents(ctx, 16); err != nil {
		t.Fatal(err)
	}
	if _, err = s.List(ctx, 0, 16); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range 8 {
		rows, err := s.List(ctx, 0, 16)
		if err != nil || len(rows) != 16 {
			t.Fatal(len(rows), err)
		}
	}
	runtime.ReadMemStats(&after)
	perPage := (after.TotalAlloc - before.TotalAlloc) / 8
	// Repeated immutable-prefix queries must not allocate a fresh DEFLATE
	// window per record. This is a memory bound, not a machine-speed deadline.
	if perPage > 256<<10 {
		t.Fatalf("warm sixteen-record archive page allocated %d bytes; limit256KiB", perPage)
	}
	t.Logf("warm sixteen-record page allocated %d bytes", perPage)
}
