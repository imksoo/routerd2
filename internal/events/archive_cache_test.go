package events

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
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

func TestSteadyArchiveBatchesHaveBoundedCodecAllocation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var allocated uint64
	for batch := range 8 {
		for i := range 16 {
			e := testEvent(fmt.Sprint(batch*16 + i))
			e.Attributes["payload"] = strings.Repeat("body", 512)
			if _, err := s.Append(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if n, err := s.ArchiveEvents(ctx, 16); err != nil || n != 16 {
			t.Fatal(n, err)
		}
		runtime.ReadMemStats(&after)
		if batch == 0 {
			runtime.GC()
		} else {
			allocated += after.TotalAlloc - before.TotalAlloc
		}
	}
	perBatch := allocated / 7
	// Repeated maintenance must reuse its bounded compression
	// workspace, rather than allocate fresh DEFLATE state every second.
	// Measure memory, not an environment-specific operation timeout.
	if perBatch > 512<<10 {
		t.Fatalf("steady sixteen-record archive batch allocated %d bytes; limit512KiB", perBatch)
	}
	t.Logf("steady sixteen-record archive batch allocated %d bytes", perBatch)
	rows, err := s.List(ctx, 0, 128)
	if err != nil || len(rows) != 128 {
		t.Fatal("codec reuse changed complete logical history", len(rows), err)
	}
}

func TestArchivedReadCacheEvictsWithoutLosingLogicalHistory(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := range 80 {
		e := testEvent(fmt.Sprint(i))
		for j := range 16 {
			e.Attributes[fmt.Sprint(j)] = strings.Repeat("x", 4000)
		}
		if _, err = s.Append(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.ArchiveEvents(ctx, 128); err != nil || n != 80 {
		t.Fatal(n, err)
	}
	if rows, err := s.List(ctx, 0, 128); err != nil || len(rows) != 80 {
		t.Fatal(len(rows), err)
	}
	s.archiveCache.mu.Lock()
	total := 0
	entries := s.archiveCache.order.Len()
	for entry := s.archiveCache.order.Front(); entry != nil; entry = entry.Next() {
		r := entry.Value.(archivedRead)
		total += len(r.body) + len(r.packed) + len(r.digest)
	}
	used := s.archiveCache.bytes
	s.archiveCache.mu.Unlock()
	if total != used || total > 4<<20 || entries >= 80 {
		t.Fatal("decoded archive cache did not evict at its payload bound", total, used, entries)
	}
	if rows, err := s.List(ctx, 0, 1); err != nil || len(rows) != 1 || rows[0].Event.ID != "0" {
		t.Fatal("evicted record no longer readable", rows, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s.archiveCache.mu.Lock()
	defer s.archiveCache.mu.Unlock()
	if len(s.archiveCache.rows) != 0 || s.archiveCache.bytes != 0 {
		t.Fatal("closed store retained archive cache")
	}
}
