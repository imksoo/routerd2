//go:build linux

package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Store struct {
	mu        sync.RWMutex
	closed    bool
	db        *sql.DB
	dir, lock *os.File
	options   Options
}

const applicationID = 0x52443245 // RD2E; separate from every other product store
const schemaVersion = 2          // unreleased prototype v1 is deliberately not imported

const schema = `
CREATE TABLE events(cursor INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE, body BLOB NOT NULL);
CREATE TABLE consumers(name TEXT PRIMARY KEY, cursor INTEGER NOT NULL CHECK(cursor>=0), version INTEGER NOT NULL DEFAULT 0 CHECK(version>=0), stateful INTEGER NOT NULL DEFAULT 0 CHECK(stateful IN (0,1)), body BLOB NOT NULL DEFAULT X'7b7d');
CREATE TABLE outbox(consumer TEXT NOT NULL REFERENCES consumers(name), key TEXT NOT NULL, event_cursor INTEGER NOT NULL REFERENCES events(cursor), payload BLOB NOT NULL, done INTEGER NOT NULL DEFAULT 0 CHECK(done IN (0,1)), PRIMARY KEY(consumer,key));
CREATE INDEX pending_work ON outbox(consumer,done,event_cursor,key);
CREATE TABLE usage(kind TEXT PRIMARY KEY, rows_count INTEGER NOT NULL, bytes_count INTEGER NOT NULL);
INSERT INTO usage VALUES('events',0,0),('outbox',0,0),('consumer_state',0,0);
PRAGMA user_version=2;
`

func Open(options Options) (_ *Store, result error) {
	if options.MaxEvents == 0 {
		options.MaxEvents = 100000
	}
	if options.MaxJobs == 0 {
		options.MaxJobs = 100000
	}
	if options.MaxConsumers == 0 {
		options.MaxConsumers = 1024
	}
	if options.MaxEventBytes == 0 {
		options.MaxEventBytes = 64 << 20
	}
	if options.MaxJobBytes == 0 {
		options.MaxJobBytes = 64 << 20
	}
	if options.MaxConsumerStateBytes == 0 {
		options.MaxConsumerStateBytes = 64 << 20
	}
	if options.MaxEvents < 1 || options.MaxEvents > 1000000 || options.MaxJobs < 1 || options.MaxJobs > 1000000 || options.MaxConsumers < 1 || options.MaxConsumers > 100000 || options.MaxEventBytes < 1 || options.MaxEventBytes > 1<<30 || options.MaxJobBytes < 1 || options.MaxJobBytes > 1<<30 {
		return nil, errors.New("invalid durable event store limits")
	}
	if options.MaxConsumerStateBytes < 2 || options.MaxConsumerStateBytes > 1<<30 {
		return nil, errors.New("invalid consumer state capacity")
	}
	dir, err := privateDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, options: options}
	defer func() {
		if result != nil {
			_ = s.Close()
		}
	}()
	s.lock, err = privateFile(dir, "store.lock")
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("event store already owned: %w", err)
	}
	for _, name := range []string{"journal.sqlite3", "journal.sqlite3-wal", "journal.sqlite3-shm", "journal.sqlite3-journal"} {
		if err = checkExistingFile(dir, name); err != nil {
			return nil, err
		}
	}
	file, err := privateFile(dir, "journal.sqlite3")
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	_ = file.Close()
	if err != nil {
		return nil, err
	}
	query := url.Values{"mode": {"rw"}, "_txlock": {"immediate"}, "_pragma": {"busy_timeout(1000)"}}
	uri := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/journal.sqlite3", dir.Fd()), RawQuery: query.Encode()}
	if info.Size() > 0 {
		// A read/write SQLite open may recover a hot rollback journal before
		// its first query. Authenticate existing state through a read-only
		// connection first, so rejection cannot roll back a foreign database.
		query.Set("mode", "ro")
		probeURI := uri
		probeURI.RawQuery = query.Encode()
		probe, err := sql.Open("sqlite", probeURI.String())
		if err != nil {
			return nil, err
		}
		var id, version, objects int
		err = probe.QueryRow(`PRAGMA application_id`).Scan(&id)
		if err == nil {
			err = probe.QueryRow(`PRAGMA user_version`).Scan(&version)
		}
		if err == nil {
			err = probe.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`).Scan(&objects)
		}
		err = errors.Join(err, probe.Close())
		if err != nil {
			return nil, err
		}
		if id != applicationID && !(id == 0 && version == 0 && objects == 0) || id == applicationID && version != schemaVersion {
			return nil, errors.New("foreign or unsupported event database")
		}
	}
	// Reapply connection-local invariants if database/sql replaces a connection
	// after cancellation or a driver failure. The read-only probe is complete.
	query.Set("mode", "rw")
	query.Add("_pragma", "synchronous(FULL)")
	query.Add("_pragma", "foreign_keys(ON)")
	uri.RawQuery = query.Encode()
	s.db, err = sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	var id, version int
	if err = s.db.QueryRow(`PRAGMA application_id`).Scan(&id); err != nil {
		return nil, err
	}
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	if id == 0 {
		var tables int
		if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
			return nil, err
		}
		if tables != 0 || version != 0 {
			return nil, errors.New("refusing foreign event database")
		}
	} else if id != applicationID {
		return nil, errors.New("foreign event database application identity")
	} else if version != schemaVersion {
		return nil, errors.New("unsupported event database schema")
	}
	// Inspect ownership/schema before any pragma that can modify the database.
	var journal string
	if err = s.db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journal); err != nil || journal != "wal" {
		return nil, errors.Join(errors.New("durable event store requires WAL"), err)
	}
	if _, err = s.db.Exec(`PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON`); err != nil {
		return nil, err
	}
	if id == 0 {
		tx, err := s.db.BeginTx(context.Background(), nil)
		if err != nil {
			return nil, err
		}
		if _, err = tx.Exec(schema); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if _, err = tx.Exec(fmt.Sprintf("PRAGMA application_id=%d", applicationID)); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
	}
	if err = s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return nil, err
	}
	if version != schemaVersion {
		return nil, errors.New("unsupported event database schema")
	}
	var integrity string
	if err = s.db.QueryRow(`PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return nil, fmt.Errorf("event database integrity: %s %w", integrity, err)
	}
	for _, entry := range []struct{ kind, table, column string }{
		{"events", "events", "body"}, {"outbox", "outbox", "payload"}, {"consumer_state", "consumers", "body"},
	} {
		var rows, bytes, storedRows, storedBytes int64
		if err = s.db.QueryRow(`SELECT count(*),coalesce(sum(length(`+entry.column+`)),0) FROM `+entry.table).Scan(&rows, &bytes); err != nil {
			return nil, err
		}
		if err = s.db.QueryRow(`SELECT rows_count,bytes_count FROM usage WHERE kind=?`, entry.kind).Scan(&storedRows, &storedBytes); err != nil {
			return nil, err
		}
		if rows != storedRows || bytes != storedBytes {
			return nil, errors.New("event store usage counters do not match durable rows")
		}
	}
	if err = dir.Sync(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var result error
	if s.db != nil {
		result = errors.Join(result, s.db.Close())
	}
	if s.dir != nil {
		result = errors.Join(result, s.dir.Sync())
	}
	if s.lock != nil {
		result = errors.Join(result, s.lock.Close())
	}
	if s.dir != nil {
		result = errors.Join(result, s.dir.Close())
	}
	return result
}

func privateDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || len(path) > 4096 {
		return nil, errors.New("event directory must be a clean absolute private path")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 128 {
		return nil, errors.New("event directory is too deep")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	for i, part := range parts {
		next, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(int(current.Fd()), part, 0700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		current.Close()
		if err != nil {
			return nil, err
		}
		current = os.NewFile(uintptr(next), part)
		var stat unix.Stat_t
		if err = unix.Fstat(next, &stat); err == nil {
			stickyRoot := stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
			if stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) || stat.Mode&0022 != 0 && !stickyRoot || i == len(parts)-1 && (stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0) {
				err = errors.New("event directory is writable by others or is not private")
			}
		}
		if err != nil {
			current.Close()
			return nil, err
		}
	}
	return current, nil
}

func checkExistingFile(dir *os.File, name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(int(dir.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 || stat.Nlink != 1 {
		return errors.New("event state file must be an owned private regular file without hard links")
	}
	return nil
}

func privateFile(dir *os.File, name string) (*os.File, error) {
	if err := checkExistingFile(dir, name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var opened, named unix.Stat_t
	if err = unix.Fstat(fd, &opened); err == nil {
		err = unix.Fstatat(int(dir.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != nil || opened.Dev != named.Dev || opened.Ino != named.Ino || opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Uid != uint32(os.Geteuid()) || opened.Mode&0077 != 0 || opened.Nlink != 1 {
		f.Close()
		return nil, errors.Join(errors.New("event state file changed or is not private"), err)
	}
	if err = checkExistingFile(dir, name); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
