package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	o, err := normalizeOptions(Options{StateDir: t.TempDir(), RuntimeDir: t.TempDir(), MemoryEntries: 32, DiskQueueSize: 128, SyncInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func testEntry(n uint64) Entry {
	return Entry{Timestamp: time.Now().UTC(), Module: "network", PID: 42, Generation: 1, Level: "INFO", Message: fmt.Sprintf("record %d", n), ProducerEpoch: strings.Repeat("a", 32), ProducerSequence: n}
}

func closeStore(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true within two seconds")
		}
		time.Sleep(time.Millisecond)
	}
}

type testSink struct {
	mu       sync.Mutex
	records  [][]byte
	size     int64
	writeErr error
	syncErr  error
	block    <-chan struct{}
	entered  chan struct{}
	once     sync.Once
}

func (s *testSink) Append(b []byte) error {
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.records = append(s.records, append([]byte(nil), b...))
	s.size += int64(len(b))
	return nil
}
func (s *testSink) Sync() error  { return s.syncErr }
func (s *testSink) Close() error { return nil }
func (s *testSink) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

func TestMemoryWindowCursorsGapsAndDuplicateDelivery(t *testing.T) {
	o := testOptions(t)
	o.MemoryEntries = 3
	s, err := startStore(o, &testSink{}, nil, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	for i := uint64(1); i <= 5; i++ {
		if _, err := s.Append([]Entry{testEntry(i)}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Query(Query{Cursor: Cursor{Epoch: s.Status().Epoch, Sequence: 1}, Limit: 2})
	if err != nil || !result.Gap || result.Reset || len(result.Records) != 2 || result.Records[0].Sequence != 3 || result.Cursor.Sequence != 4 || !result.More {
		t.Fatalf("unexpected first page: %+v error=%v", result, err)
	}
	second, err := s.Query(Query{Cursor: result.Cursor, Limit: 2})
	if err != nil || second.Gap || second.More || len(second.Records) != 1 || second.Records[0].Sequence != 5 || second.Cursor.Sequence != 5 {
		t.Fatalf("unexpected second page: %+v error=%v", second, err)
	}
	duplicate, err := s.Append([]Entry{testEntry(5)})
	if err != nil || duplicate.Accepted != 0 || duplicate.Duplicates != 1 || s.Status().Accepted != 5 {
		t.Fatal("retried delivery was stored a second time")
	}
	reset, err := s.Query(Query{Cursor: Cursor{Epoch: strings.Repeat("b", 32), Sequence: 100}})
	if err != nil || !reset.Reset || !reset.Gap || reset.Cursor.Epoch != s.Status().Epoch || reset.Records[0].Sequence != 3 {
		t.Fatalf("old-epoch cursor was not reset: %+v error=%v", reset, err)
	}
	if s.Status().MemoryEntries != 3 || s.Status().MemoryEvicted != 2 {
		t.Fatal("memory retention did not remain bounded")
	}
}

func TestProducerLossAndFilteredCursorProgress(t *testing.T) {
	o := testOptions(t)
	s, _ := startStore(o, &testSink{}, nil, 0, nil, nil)
	defer closeStore(t, s)
	_, _ = s.Append([]Entry{testEntry(1), testEntry(4)})
	e := testEntry(5)
	e.Module, e.Level = "control", "WARN"
	_, _ = s.Append([]Entry{e})
	all, _ := s.Query(Query{})
	if !all.Gap || all.Records[1].ProducerGap != 2 || s.Status().ProducerGaps != 2 {
		t.Fatalf("producer sequence gap disappeared: %+v", all)
	}
	filtered, _ := s.Query(Query{Module: "control", MinLevel: "WARN"})
	if len(filtered.Records) != 1 || filtered.Records[0].Module != "control" || filtered.Cursor.Sequence != 3 {
		t.Fatalf("filtered cursor did not advance over nonmatching entries: %+v", filtered)
	}
	none, _ := s.Query(Query{Module: "management"})
	if len(none.Records) != 0 || none.Cursor.Sequence != 3 {
		t.Fatal("empty filtered page would loop on an unchanged cursor")
	}
}

func TestDiskFailurePreservesQueriesAndReportsLoss(t *testing.T) {
	o := testOptions(t)
	o.MemoryEntries = 4
	sink := &testSink{writeErr: syscall.ENOSPC}
	s, _ := startStore(o, sink, nil, 0, nil, nil)
	defer closeStore(t, s)
	for n := uint64(1); n <= 8; n++ {
		if _, err := s.Append([]Entry{testEntry(n)}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return s.Status().DiskDropped == 8 })
	status := s.Status()
	if !status.DiskDegraded || status.DiskEnabled || status.DiskErrors == 0 || !strings.Contains(status.LastDiskError, "space") {
		t.Fatalf("disk failure was not visible: %+v", status)
	}
	query, err := s.Query(Query{Tail: true})
	if err != nil || len(query.Records) != 4 || query.Records[3].ProducerSequence != 8 {
		t.Fatal("disk failure blocked or lost the bounded memory fallback")
	}
}

func TestBlockedDiskNeverBlocksIngestQueryOrBoundedClose(t *testing.T) {
	o := testOptions(t)
	o.DiskQueueSize = 1
	unblock := make(chan struct{})
	sink := &testSink{block: unblock, entered: make(chan struct{})}
	s, _ := startStore(o, sink, nil, 0, nil, nil)
	_, _ = s.Append([]Entry{testEntry(1)})
	<-sink.entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := uint64(2); n <= 20; n++ {
			_, _ = s.Append([]Entry{testEntry(n)})
		}
		_, _ = s.Query(Query{Tail: true})
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(unblock)
		t.Fatal("memory path waited on a blocked disk writer")
	}
	if status := s.Status(); status.DiskDropped < 18 || status.DiskQueueDepth > 1 || !status.DiskDegraded {
		t.Fatalf("disk queue did not drop visibly under overload: %+v", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close did not respect its deadline: %v", err)
	}
	close(unblock)
	closeStore(t, s)
	if _, err := s.Append([]Entry{testEntry(21)}); !errors.Is(err, ErrClosed) {
		t.Fatal("closed store accepted an entry")
	}
}

func TestDiskRotationRetentionAndReopen(t *testing.T) {
	o := testOptions(t)
	o.MaxFileBytes, o.MaxFiles, o.MemoryEntries = 2048, 3, 8
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := s.Status().Epoch
	for n := uint64(1); n <= 36; n++ {
		e := testEntry(n)
		e.Message += strings.Repeat("x", 128)
		if _, err := s.Append([]Entry{e}); err != nil {
			t.Fatal(err)
		}
	}
	closeStore(t, s)
	if status := s.Status(); status.DiskWritten != 36 || status.DiskDropped != 0 {
		t.Fatalf("rotation lost queued records: %+v", status)
	}
	files, err := filepath.Glob(filepath.Join(o.StateDir, "logs", "log.*.ndjson"))
	if err != nil || len(files) != 3 {
		t.Fatalf("retention file count: %v %v", files, err)
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Size() > o.MaxFileBytes || info.Mode().Perm() != 0o600 {
			t.Fatalf("retained file violated size/mode bounds: %s %v %v", path, info, err)
		}
		assertNDJSON(t, path)
	}
	reopened, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, reopened)
	q, err := reopened.Query(Query{Cursor: Cursor{Epoch: oldEpoch, Sequence: 36}, Tail: true})
	if err != nil || !q.Reset || len(q.Records) != 8 || q.Records[0].Sequence != 29 || q.Records[7].Sequence != 36 || q.Cursor.Epoch == oldEpoch {
		t.Fatalf("reopened persisted tail/cursor: %+v %v", q, err)
	}
	if result, err := reopened.Append([]Entry{testEntry(37)}); err != nil || result.Cursor.Sequence != 37 {
		t.Fatalf("sequence did not continue after replay: %+v %v", result, err)
	}
}

func assertNDJSON(t *testing.T, path string) []Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		t.Fatal("retained file has an incomplete final record")
	}
	var records []Record
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("corrupt retained record: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func TestRecoveryTruncatesOnlyIncompleteActiveTail(t *testing.T) {
	o := testOptions(t)
	s, _ := Open(o)
	_, _ = s.Append([]Entry{testEntry(1), testEntry(2), testEntry(3)})
	closeStore(t, s)
	path := logPath(filepath.Join(o.StateDir, "logs"), 0)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(f, `{"version":1,"message":"crash midway`)
	_ = f.Close()
	recovered, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status().CorruptRecords != 1 || recovered.Status().LatestSequence != 3 {
		t.Fatalf("partial crash tail was not diagnosed: %+v", recovered.Status())
	}
	_, _ = recovered.Append([]Entry{testEntry(4)})
	closeStore(t, recovered)
	if records := assertNDJSON(t, path); len(records) != 4 || records[3].Sequence != 4 {
		t.Fatal("recovery appended onto, or removed more than, the incomplete tail")
	}
}

func TestLiveWriterLockAndUnsafePath(t *testing.T) {
	o := testOptions(t)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Open(o); !errors.Is(err, errStoreInUse) {
		if second != nil {
			closeStore(t, second)
		}
		t.Fatalf("second writer was allowed: %v", err)
	}
	closeStore(t, s)
	other := testOptions(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(other.StateDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, logPath(filepath.Join(other.StateDir, "logs"), 0)); err != nil {
		t.Fatal(err)
	}
	memoryOnly, err := Open(other)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, memoryOnly)
	if status := memoryOnly.Status(); status.DiskEnabled || !status.DiskDegraded {
		t.Fatal("unsafe disk path did not enter visible memory-only mode")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "keep" {
		t.Fatal("symlink target was modified")
	}
}

func TestInvalidBatchDoesNotPartiallyAppend(t *testing.T) {
	o := testOptions(t)
	s, _ := startStore(o, &testSink{}, nil, 0, nil, nil)
	defer closeStore(t, s)
	bad := testEntry(2)
	bad.Level = "unknown"
	if _, err := s.Append([]Entry{testEntry(1), bad}); err == nil || s.Status().Accepted != 0 || s.Status().Rejected != 2 {
		t.Fatal("invalid batch partially appended")
	}
}

func TestRecoveredPersistenceHolesAreReported(t *testing.T) {
	o := testOptions(t)
	records := []Record{
		{Entry: testEntry(1), Version: 1, Epoch: strings.Repeat("b", 32), Sequence: 1},
		{Entry: testEntry(3), Version: 1, Epoch: strings.Repeat("b", 32), Sequence: 3},
	}
	s, err := startStore(o, &testSink{}, records, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	query, _ := s.Query(Query{})
	if s.Status().RecoveredGaps != 1 || !query.Gap || !strings.Contains(strings.Join(query.GapReasons, ","), "persistence_gap") {
		t.Fatalf("missing persisted record was hidden after recovery: %+v", query)
	}
}

func TestReducedRetentionRemovesOnlyOwnedExcessArchives(t *testing.T) {
	o := testOptions(t)
	dir := filepath.Join(o.StateDir, "logs")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := os.WriteFile(logPath(dir, i), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "operator-note.txt")
	_ = os.WriteFile(unrelated, []byte("keep"), 0o600)
	o.MaxFiles = 2
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)
	files, _ := filepath.Glob(filepath.Join(dir, "log.*.ndjson"))
	if len(files) != 2 {
		t.Fatalf("reduced retention left orphan archives: %v", files)
	}
	if content, err := os.ReadFile(unrelated); err != nil || string(content) != "keep" {
		t.Fatal("retention removed an unrelated file")
	}
}

func TestUnreadableHistoryRequiresExplicitRecoveryRestart(t *testing.T) {
	o := testOptions(t)
	if err := os.WriteFile(filepath.Join(o.StateDir, "logs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, s)
	status := s.Status()
	if !status.DiskRecoveryRequired || !status.DiskDegraded || status.DiskEnabled {
		t.Fatalf("unknown persisted sequence was not made explicit: %+v", status)
	}
	if result, err := s.Append([]Entry{testEntry(1)}); err != nil || result.Accepted != 1 {
		t.Fatal("unreadable startup history disabled memory diagnostics")
	}
}
