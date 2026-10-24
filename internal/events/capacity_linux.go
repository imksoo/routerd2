//go:build linux

package events

import (
	"context"
	"errors"
	"math"

	"golang.org/x/sys/unix"
)

type Budget struct {
	Rows      int64 `json:"rows"`
	Bytes     int64 `json:"bytes"`
	RowLimit  int64 `json:"row_limit"`
	ByteLimit int64 `json:"byte_limit"`
}

// Capacity is a bounded observation. Completed jobs retain their charges and
// are deliberately included; this API neither registers nor acknowledges work.
type Capacity struct {
	Tombstones               Budget   `json:"tombstones"`
	Events                   Budget   `json:"events"`
	Jobs                     Budget   `json:"jobs"`
	Registrations            Budget   `json:"registrations"`
	FirstCursor              uint64   `json:"first_cursor"`
	TipCursor                uint64   `json:"tip_cursor"`
	SlowestConsumerCursor    uint64   `json:"slowest_consumer_cursor"`
	DatabaseBytes            uint64   `json:"database_bytes"`
	WALBytes                 uint64   `json:"wal_bytes"`
	AllocatedBytes           uint64   `json:"allocated_bytes"`
	FilesystemBytes          uint64   `json:"filesystem_bytes"`
	FilesystemAvailableBytes uint64   `json:"filesystem_available_bytes"`
	Pressure                 string   `json:"pressure"`
	Reasons                  []string `json:"reasons"`
}

func (s *Store) Capacity(ctx context.Context) (Capacity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return Capacity{}, ErrClosed
	}
	c := Capacity{Tombstones: Budget{RowLimit: s.options.MaxTombstones, ByteLimit: s.options.MaxTombstoneBytes}, Events: Budget{RowLimit: s.options.MaxEvents, ByteLimit: s.options.MaxEventBytes}, Jobs: Budget{RowLimit: s.options.MaxJobs, ByteLimit: s.options.MaxJobBytes}, Registrations: Budget{RowLimit: s.options.MaxConsumers, ByteLimit: s.options.MaxConsumerStateBytes}, Pressure: "ok", Reasons: []string{}}
	// One statement gives a coherent cut without scanning event/job payloads or
	// allocating a producer. Cursor aggregates use their existing primary keys.
	err := s.db.QueryRowContext(ctx, `SELECT e.rows_count,e.bytes_count,j.rows_count,j.bytes_count,c.rows_count,c.bytes_count,d.rows_count,d.bytes_count,
coalesce((SELECT min(cursor) FROM events),0),coalesce((SELECT max(cursor) FROM events),0),
coalesce((SELECT min(cursor) FROM consumers),(SELECT coalesce(max(cursor),0) FROM events))
FROM usage e,usage j,usage c,usage d WHERE e.kind='events' AND j.kind='outbox' AND c.kind='consumer_state' AND d.kind='dedup'`).Scan(&c.Events.Rows, &c.Events.Bytes, &c.Jobs.Rows, &c.Jobs.Bytes, &c.Registrations.Rows, &c.Registrations.Bytes, &c.Tombstones.Rows, &c.Tombstones.Bytes, &c.FirstCursor, &c.TipCursor, &c.SlowestConsumerCursor)
	if err != nil {
		return Capacity{}, err
	}
	for _, b := range []Budget{c.Events, c.Jobs, c.Registrations, c.Tombstones} {
		if b.Rows < 0 || b.Bytes < 0 {
			return Capacity{}, errors.New("negative durable usage counter")
		}
	}
	for _, name := range []string{"journal.sqlite3", "journal.sqlite3-wal", "journal.sqlite3-shm"} {
		if err = checkExistingFile(s.dir, name); err != nil {
			return Capacity{}, err
		}
		var st unix.Stat_t
		err = unix.Fstatat(int(s.dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return Capacity{}, err
		}
		if st.Size < 0 || st.Blocks < 0 {
			return Capacity{}, errors.New("negative event state allocation")
		}
		if name == "journal.sqlite3" {
			c.DatabaseBytes = uint64(st.Size)
		}
		if name == "journal.sqlite3-wal" {
			c.WALBytes = uint64(st.Size)
		}
		c.AllocatedBytes += uint64(st.Blocks) * 512
	}
	var fs unix.Statfs_t
	if err = unix.Fstatfs(int(s.dir.Fd()), &fs); err != nil {
		return Capacity{}, err
	}
	if fs.Bsize <= 0 || fs.Blocks > math.MaxUint64/uint64(fs.Bsize) || fs.Bavail > fs.Blocks {
		return Capacity{}, errors.New("invalid filesystem space observation")
	}
	c.FilesystemBytes = fs.Blocks * uint64(fs.Bsize)
	c.FilesystemAvailableBytes = fs.Bavail * uint64(fs.Bsize)
	for _, item := range []struct {
		name        string
		used, limit int64
	}{
		{"events_rows", c.Events.Rows, c.Events.RowLimit}, {"events_bytes", c.Events.Bytes, c.Events.ByteLimit},
		{"jobs_rows", c.Jobs.Rows, c.Jobs.RowLimit}, {"jobs_bytes", c.Jobs.Bytes, c.Jobs.ByteLimit},
		{"registrations_rows", c.Registrations.Rows, c.Registrations.RowLimit}, {"state_bytes", c.Registrations.Bytes, c.Registrations.ByteLimit},
		{"tombstones_rows", c.Tombstones.Rows, c.Tombstones.RowLimit}, {"tombstones_bytes", c.Tombstones.Bytes, c.Tombstones.ByteLimit},
	} {
		if item.used >= item.limit {
			c.Pressure = "exhausted"
			c.Reasons = append(c.Reasons, item.name)
		} else if item.used*100 >= item.limit*80 {
			if c.Pressure == "ok" {
				c.Pressure = "warning"
			}
			c.Reasons = append(c.Reasons, item.name)
		}
	}
	if c.FilesystemAvailableBytes == 0 {
		c.Pressure = "exhausted"
		c.Reasons = append(c.Reasons, "filesystem_space")
	} else if c.FilesystemAvailableBytes < 16<<20 || c.FilesystemAvailableBytes < c.FilesystemBytes/10 {
		if c.Pressure == "ok" {
			c.Pressure = "warning"
		}
		c.Reasons = append(c.Reasons, "filesystem_space")
	}
	return c, nil
}
