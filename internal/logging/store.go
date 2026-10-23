package logging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

var ErrClosed = errors.New("log store is closed")

type diskSink interface {
	Append([]byte) error
	Sync() error
	Close() error
	Size() int64
}

type Store struct {
	options  Options
	mu       sync.RWMutex
	status   Status
	ring     []Record
	start    int
	count    int
	closed   bool
	seen     map[string]uint64
	seenFIFO []string
	queue    chan []byte
	stop     chan struct{}
	done     chan struct{}
}

func normalizeOptions(o Options) (Options, error) {
	if !filepath.IsAbs(o.StateDir) || filepath.Clean(o.StateDir) == "/" {
		return o, errors.New("log state directory must be an absolute non-root path")
	}
	if o.MaxFileBytes == 0 {
		o.MaxFileBytes = 4 << 20
	}
	if o.MaxFiles == 0 {
		o.MaxFiles = 4
	}
	if o.MemoryEntries == 0 {
		o.MemoryEntries = 1024
	}
	if o.DiskQueueSize == 0 {
		o.DiskQueueSize = 256
	}
	if o.SyncInterval == 0 {
		o.SyncInterval = time.Second
	}
	if o.MaxFileBytes < 1024 || o.MaxFileBytes > 128<<20 || o.MaxFiles < 2 || o.MaxFiles > 32 || o.MemoryEntries < 1 || o.MemoryEntries > 16384 || o.DiskQueueSize < 1 || o.DiskQueueSize > 8192 || o.SyncInterval < 10*time.Millisecond || o.SyncInterval > time.Minute {
		return o, errors.New("log retention, queue or sync interval is outside supported bounds")
	}
	return o, nil
}

// Open replays the retained NDJSON tail, then starts one bounded disk writer.
// A filesystem error permits a visible memory-only degraded mode. A live writer
// lock is different: two processes may not own the same retained log directory.
func Open(options Options) (*Store, error) {
	o, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	sink, recovered, corrupt, diskErr := openDisk(o, true)
	if errors.Is(diskErr, errStoreInUse) {
		return nil, diskErr
	}
	var reopen func() (diskSink, error)
	if diskErr == nil {
		reopen = func() (diskSink, error) {
			disk, _, _, err := openDisk(o, false)
			return disk, err
		}
	}
	// An unreadable startup history makes the next persisted sequence unknown.
	// Remain memory-only until a worker restart can replay it safely. Runtime
	// disk failures have a known sequence and can retry opening automatically.
	return startStore(o, sink, recovered, corrupt, diskErr, reopen)
}

func startStore(o Options, sink diskSink, recovered []Record, corrupt uint64, diskErr error, reopen func() (diskSink, error)) (*Store, error) {
	epoch, err := newEpoch()
	if err != nil {
		if sink != nil {
			_ = sink.Close()
		}
		return nil, err
	}
	s := &Store{options: o, ring: make([]Record, o.MemoryEntries), seen: make(map[string]uint64), queue: make(chan []byte, o.DiskQueueSize), stop: make(chan struct{}), done: make(chan struct{})}
	s.status = Status{Epoch: epoch, ReleaseID: o.ReleaseID, MemoryLimit: o.MemoryEntries, DiskQueueLimit: o.DiskQueueSize, DiskEnabled: sink != nil, CorruptRecords: corrupt}
	for _, record := range recovered {
		if record.Sequence <= s.status.LatestSequence {
			s.status.CorruptRecords++
			continue
		}
		if s.status.LatestSequence != 0 && record.Sequence-s.status.LatestSequence > 1 {
			s.status.RecoveredGaps += record.Sequence - s.status.LatestSequence - 1
		}
		s.status.LatestSequence = record.Sequence
		s.rememberProducer(record.Entry)
		s.push(record)
	}
	if sink != nil {
		s.status.CurrentFileBytes = sink.Size()
	}
	if diskErr != nil {
		s.status.DiskDegraded = true
		s.status.DiskRecoveryRequired = true
		s.status.DiskErrors++
		s.status.LastDiskError = boundedString(diskErr.Error(), 1024)
	}
	go s.diskLoop(sink, reopen)
	return s, nil
}

func (s *Store) rememberProducer(entry Entry) uint64 {
	key := entry.ProducerEpoch
	previous, seen := s.seen[key]
	if !seen {
		if len(s.seenFIFO) == 1024 {
			delete(s.seen, s.seenFIFO[0])
			s.seenFIFO = append(s.seenFIFO[:0], s.seenFIFO[1:]...)
			s.status.ProducerStateEvicted++
		}
		s.seenFIFO = append(s.seenFIFO, key)
	}
	s.seen[key] = entry.ProducerSequence
	if entry.ProducerSequence > previous+1 {
		return entry.ProducerSequence - previous - 1
	}
	return 0
}

func (s *Store) push(record Record) {
	if s.count < len(s.ring) {
		s.ring[(s.start+s.count)%len(s.ring)] = record
		s.count++
	} else {
		s.ring[s.start] = record
		s.start = (s.start + 1) % len(s.ring)
		s.status.MemoryEvicted++
	}
}

// Append validates the batch before changing state. The only work under the
// store mutex is a bounded memory update and a nonblocking disk-queue offer.
func (s *Store) Append(entries []Entry) (IngestResult, error) {
	if len(entries) == 0 || len(entries) > MaxBatch {
		return IngestResult{}, fmt.Errorf("log batch must contain 1..%d entries", MaxBatch)
	}
	prepared := make([]Entry, len(entries))
	for i, entry := range entries {
		clean, err := validateEntry(entry)
		if err != nil {
			s.mu.Lock()
			s.status.Rejected += uint64(len(entries))
			s.mu.Unlock()
			return IngestResult{}, err
		}
		prepared[i] = clean
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return IngestResult{}, ErrClosed
	}
	if uint64(len(entries)) > ^uint64(0)-s.status.LatestSequence {
		return IngestResult{}, errors.New("log sequence exhausted")
	}
	result := IngestResult{}
	for _, entry := range prepared {
		if last, exists := s.seen[entry.ProducerEpoch]; exists && entry.ProducerSequence <= last {
			result.Duplicates++
			continue
		}
		s.status.LatestSequence++
		gap := s.rememberProducer(entry)
		record := Record{Entry: entry, Version: 1, Epoch: s.status.Epoch, Sequence: s.status.LatestSequence, ProducerGap: gap}
		s.status.ProducerGaps += gap
		s.status.Accepted++
		s.push(record)
		// Entry limits reserve room for the server metadata; marshal cannot fail
		// after scalar validation. No filesystem operation runs under this lock.
		b, _ := json.Marshal(record)
		b = append(b, '\n')
		if int64(len(b)) > s.options.MaxFileBytes {
			s.status.DiskDropped++
			s.status.DiskDegraded = true
			s.status.LastDiskError = "record exceeds configured file size"
		} else {
			select {
			case s.queue <- b:
			default:
				s.status.DiskDropped++
				s.status.DiskDegraded = true
				s.status.LastDiskError = "disk queue full"
			}
		}
		result.Accepted++
	}
	result.Cursor = Cursor{Epoch: s.status.Epoch, Sequence: s.status.LatestSequence}
	result.Degraded = s.status.DiskDegraded
	return result, nil
}

func (s *Store) statusLocked() Status {
	status := s.status
	status.MemoryEntries = s.count
	status.DiskQueueDepth = len(s.queue)
	if s.count > 0 {
		status.OldestSequence = s.ring[s.start].Sequence
	}
	if status.LastDiskSuccess != nil {
		stamp := *status.LastDiskSuccess
		status.LastDiskSuccess = &stamp
	}
	return status
}

func (s *Store) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statusLocked()
}

// Query reads only the bounded memory window, including the retained tail
// replayed at startup. Files are never read on the query or ingest path.
func (s *Store) Query(q Query) (QueryResult, error) {
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > MaxQuery || q.Module != "" && !moduleName.MatchString(q.Module) {
		return QueryResult{}, errors.New("invalid log query limit or module")
	}
	minimum := -4
	if q.MinLevel != "" {
		var ok bool
		minimum, ok = levelNumber(q.MinLevel)
		if !ok {
			return QueryResult{}, errors.New("invalid minimum log level")
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := QueryResult{Records: make([]Record, 0, q.Limit), Status: s.statusLocked()}
	result.OldestSequence, result.LatestSequence = result.Status.OldestSequence, result.Status.LatestSequence
	result.Cursor = Cursor{Epoch: s.status.Epoch, Sequence: q.Cursor.Sequence}
	addGap := func(reason string) {
		for _, old := range result.GapReasons {
			if old == reason {
				return
			}
		}
		result.Gap, result.GapReasons = true, append(result.GapReasons, reason)
	}
	after := q.Cursor.Sequence
	if q.Cursor.Epoch != "" && q.Cursor.Epoch != s.status.Epoch {
		result.Reset, after = true, 0
		addGap("logger_restart")
	}
	if after > s.status.LatestSequence {
		result.Reset, after = true, 0
		addGap("cursor_ahead")
	}
	if result.OldestSequence > after && result.OldestSequence-after > 1 {
		addGap("memory_retention")
	}
	start := 0
	if q.Tail {
		matches := 0
		start = s.count
		for start > 0 && matches < q.Limit {
			start--
			record := s.ring[(s.start+start)%len(s.ring)]
			level, _ := levelNumber(record.Level)
			if (q.Module == "" || record.Module == q.Module) && level >= minimum {
				matches++
			}
		}
		after = 0
	}
	result.Cursor.Sequence = after
	previous := after
	if start > 0 {
		previous = s.ring[(s.start+start-1)%len(s.ring)].Sequence
	}
	for i := start; i < s.count; i++ {
		record := s.ring[(s.start+i)%len(s.ring)]
		if record.Sequence <= after {
			continue
		}
		if len(result.Records) == q.Limit {
			result.More = true
			break
		}
		if previous != 0 && record.Sequence-previous > 1 {
			addGap("persistence_gap")
		}
		previous = record.Sequence
		result.Cursor.Sequence = record.Sequence
		level, _ := levelNumber(record.Level)
		if (q.Module != "" && record.Module != q.Module) || level < minimum {
			continue
		}
		result.Records = append(result.Records, copyRecord(record))
		if record.ProducerGap > 0 {
			addGap("producer_drop")
		}
	}
	if !result.More {
		result.Cursor.Sequence = s.status.LatestSequence
	}
	return result, nil
}

func (s *Store) diskFailure(err error, dropped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.DiskEnabled, s.status.DiskDegraded = false, true
	s.status.DiskErrors++
	if dropped {
		s.status.DiskDropped++
	}
	s.status.LastDiskError = boundedString(err.Error(), 1024)
}

func (s *Store) diskLoop(sink diskSink, reopen func() (diskSink, error)) {
	defer close(s.done)
	ticker := time.NewTicker(s.options.SyncInterval)
	defer ticker.Stop()
	lastReopen := time.Time{}
	write := func(b []byte) {
		if sink == nil {
			s.mu.Lock()
			s.status.DiskDropped++
			s.status.DiskDegraded = true
			s.mu.Unlock()
			return
		}
		if err := sink.Append(b); err != nil {
			s.diskFailure(err, true)
			_ = sink.Close()
			sink = nil
			return
		}
		stamp, size := time.Now().UTC(), sink.Size()
		s.mu.Lock()
		s.status.DiskWritten++
		s.status.DiskEnabled = true
		s.status.CurrentFileBytes = size
		s.status.LastDiskSuccess = &stamp
		s.mu.Unlock()
	}
	flush := func() {
		if sink != nil {
			if err := sink.Sync(); err != nil {
				s.diskFailure(err, false)
				_ = sink.Close()
				sink = nil
			}
		}
	}
	defer func() {
		if sink != nil {
			_ = sink.Close()
		}
	}()
	for {
		select {
		case b := <-s.queue:
			write(b)
		case <-ticker.C:
			flush()
			if sink == nil && reopen != nil && time.Since(lastReopen) >= time.Second {
				lastReopen = time.Now()
				var err error
				sink, err = reopen()
				if err != nil {
					s.diskFailure(err, false)
				} else if sink != nil {
					s.mu.Lock()
					s.status.DiskEnabled = true
					s.status.CurrentFileBytes = sink.Size()
					s.mu.Unlock()
				}
			}
		case <-s.stop:
			for {
				select {
				case b := <-s.queue:
					write(b)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Close stops accepting records and drains the bounded disk queue. Its deadline
// still bounds callers if a broken filesystem blocks the writer itself.
func (s *Store) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
