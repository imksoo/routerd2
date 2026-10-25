//go:build linux

package events

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const maxSchemaObjects = 64
const maxSchemaBytes = 64 << 10

var ErrFormat = errors.New("foreign or unsupported event store format")

// FormatInspection identifies actual header/catalog state, not event/cursor
// contents, integrity of all rows or migration/revocation authorization.
type FormatInspection struct {
	ApplicationID int    `json:"application_id"`
	Version       int    `json:"version"`
	JournalMode   string `json:"journal_mode"`
	Objects       int    `json:"objects"`
	SchemaSHA256  string `json:"schema_sha256"`
}

type schemaObject struct {
	Type string `json:"type"`
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func expectedSchema() ([]schemaObject, error) {
	var objects []schemaObject
	for _, line := range strings.Split(schema, "\n") {
		line = strings.TrimSuffix(strings.TrimSpace(line), ";")
		kind := ""
		if strings.HasPrefix(line, "CREATE TABLE ") {
			kind = "table"
		} else if strings.HasPrefix(line, "CREATE INDEX ") {
			kind = "index"
		} else {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, ErrFormat
		}
		name, _, _ := strings.Cut(fields[2], "(")
		objects = append(objects, schemaObject{Type: kind, Name: name, SQL: line})
	}
	if len(objects) == 0 || len(objects) > maxSchemaObjects {
		return nil, ErrFormat
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Type != objects[j].Type {
			return objects[i].Type < objects[j].Type
		}
		return objects[i].Name < objects[j].Name
	})
	return objects, nil
}

// Each pragma and catalog row belongs to the same SELECT/read snapshot. Do not
// BeginTx here: this connection's write operations use _txlock=immediate.
const formatQuery = `
WITH header AS (SELECT (SELECT application_id FROM pragma_application_id) AS app,
 (SELECT user_version FROM pragma_user_version) AS version,
 (SELECT journal_mode FROM pragma_journal_mode) AS mode)
SELECT h.app,h.version,h.mode,s.type,s.name,s.sql FROM header h
LEFT JOIN (SELECT type,name,sql FROM main.sqlite_schema WHERE name NOT GLOB 'sqlite_*') s ON 1
ORDER BY s.type,s.name LIMIT ?`

// Operational open authenticates every required table/index, but keeps its
// existing behavior with ancillary objects used by failure-recovery fixtures.
// Update qualification additionally requires the entire catalog to match.
func inspectDatabaseFormat(ctx context.Context, db *sql.DB, allowEmpty, exact, requireWAL bool) (FormatInspection, error) {
	if err := ctx.Err(); err != nil {
		return FormatInspection{}, err
	}
	rows, err := db.QueryContext(ctx, formatQuery, maxSchemaObjects+1)
	if err != nil {
		return FormatInspection{}, err
	}
	defer rows.Close()
	var result FormatInspection
	var objects []schemaObject
	seenHeader := false
	bytes := 0
	for rows.Next() {
		var header FormatInspection
		var kind, name, ddl sql.NullString
		if err := rows.Scan(&header.ApplicationID, &header.Version, &header.JournalMode, &kind, &name, &ddl); err != nil {
			return FormatInspection{}, err
		}
		if seenHeader && (header.ApplicationID != result.ApplicationID || header.Version != result.Version || header.JournalMode != result.JournalMode) {
			return FormatInspection{}, ErrFormat
		}
		result = header
		seenHeader = true
		if !name.Valid {
			continue
		}
		if !kind.Valid || !ddl.Valid || len(name.String) > 128 || len(kind.String) > 16 || len(ddl.String) > 8192 || len(objects) >= maxSchemaObjects {
			return FormatInspection{}, ErrFormat
		}
		bytes += len(name.String) + len(kind.String) + len(ddl.String)
		if bytes > maxSchemaBytes {
			return FormatInspection{}, ErrFormat
		}
		objects = append(objects, schemaObject{Type: kind.String, Name: name.String, SQL: ddl.String})
	}
	if err := rows.Err(); err != nil {
		return FormatInspection{}, err
	}
	if !seenHeader {
		return FormatInspection{}, ErrFormat
	}
	if allowEmpty && result.ApplicationID == 0 && result.Version == 0 && len(objects) == 0 {
		return result, nil
	}
	if result.ApplicationID != applicationID || result.Version != SchemaVersion || requireWAL && result.JournalMode != "wal" {
		return FormatInspection{}, ErrFormat
	}
	expected, err := expectedSchema()
	if err != nil {
		return FormatInspection{}, err
	}
	if exact && len(objects) != len(expected) {
		return FormatInspection{}, ErrFormat
	}
	actual := make(map[string]schemaObject, len(objects))
	for _, object := range objects {
		key := object.Type + "/" + object.Name
		if _, ok := actual[key]; ok {
			return FormatInspection{}, ErrFormat
		}
		actual[key] = object
	}
	for _, object := range expected {
		if actual[object.Type+"/"+object.Name] != object {
			return FormatInspection{}, ErrFormat
		}
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		return FormatInspection{}, err
	}
	digest := sha256.Sum256(raw)
	result.Objects = len(objects)
	result.SchemaSHA256 = hex.EncodeToString(digest[:])
	return result, nil
}

// InspectFormat reads the already-owned live store; it never opens/initializes a
// replacement database or runs migration/checkpoint/compaction/registration.
func (s *Store) InspectFormat(ctx context.Context) (FormatInspection, error) {
	if err := ctx.Err(); err != nil {
		return FormatInspection{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return FormatInspection{}, ErrClosed
	}
	if err := s.checkDatabaseIdentity(); err != nil {
		return FormatInspection{}, err
	}
	result, err := inspectDatabaseFormat(ctx, s.db, false, true, true)
	if err != nil {
		return FormatInspection{}, err
	}
	if err := s.checkDatabaseIdentity(); err != nil {
		return FormatInspection{}, err
	}
	return result, nil
}

func (s *Store) checkDatabaseIdentity() error {
	for _, name := range []string{"store.lock", "journal.sqlite3", "journal.sqlite3-wal", "journal.sqlite3-shm", "journal.sqlite3-journal"} {
		if err := checkExistingFile(s.dir, name); err != nil {
			return err
		}
	}
	var main, lock, held unix.Stat_t
	if err := unix.Fstatat(int(s.dir.Fd()), "journal.sqlite3", &main, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if main.Dev != s.databaseDevice || main.Ino != s.databaseInode {
		return fmt.Errorf("%w: database pathname no longer identifies the owned file", ErrFormat)
	}
	if err := unix.Fstatat(int(s.dir.Fd()), "store.lock", &lock, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if err := unix.Fstat(int(s.lock.Fd()), &held); err != nil {
		return err
	}
	if lock.Dev != held.Dev || lock.Ino != held.Ino {
		return fmt.Errorf("%w: store lock pathname changed", ErrFormat)
	}
	return nil
}
