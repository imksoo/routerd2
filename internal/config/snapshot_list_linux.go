//go:build linux

package config

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/imksoo/routerd2/internal/configref"
)

type SnapshotList struct {
	Snapshots  []SnapshotRef     `json:"snapshots"`
	Unreadable []SnapshotProblem `json:"unreadable,omitempty"`
}

type SnapshotProblem struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// ListSnapshots discovers immutable saved intent, without another catalog or
// holding the live configuration writer while reading its saved files.
func (s *Store) ListSnapshots() (SnapshotList, error) {
	result := SnapshotList{Snapshots: []SnapshotRef{}}
	current, err := s.Load()
	if err != nil {
		return result, err
	}
	dir, err := s.snapshotDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(MaxSnapshots + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	if len(names) > MaxSnapshots {
		return result, ErrSnapshotCapacity
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.HasPrefix(name, ".snapshot-") {
			continue
		}
		digest, ok := strings.CutSuffix(name, ".json")
		if !ok || !configref.ValidHex(digest, 64) {
			result.Unreadable = append(result.Unreadable, SnapshotProblem{File: name, Error: "unrecognized saved configuration filename"})
			continue
		}
		snapshot, err := readSnapshotFile(dir, digest)
		ref := SnapshotRef{StoreID: snapshot.StoreID, Revision: snapshot.Revision, Digest: digest}
		if err == nil {
			err = ref.Validate()
		}
		if err == nil && ref.StoreID != current.StoreID {
			err = errors.New("snapshot belongs to a different configuration store")
		}
		if err != nil {
			result.Unreadable = append(result.Unreadable, SnapshotProblem{File: name, Error: err.Error()})
			continue
		}
		result.Snapshots = append(result.Snapshots, ref)
	}
	slices.SortFunc(result.Snapshots, func(a, b SnapshotRef) int {
		if a.Revision > b.Revision {
			return -1
		}
		if a.Revision < b.Revision {
			return 1
		}
		return strings.Compare(a.Digest, b.Digest)
	})
	return result, nil
}
