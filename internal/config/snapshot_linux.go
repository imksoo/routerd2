//go:build linux

package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/imksoo/routerd2/internal/configref"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Snapshots are explicit immutable copies, not another running configuration
// authority. A future boot transaction must select a verified reference and
// durably adopt it into the authoritative journal before applying its intent.
const MaxSnapshots = 128
const SnapshotVersion = 1
const MaxSnapshotBytes = MaxDocumentSize + (4 << 10)

var ErrSnapshotConflict = errors.New("snapshot requires the current confirmed configuration revision")
var ErrSnapshotCapacity = errors.New("configuration snapshot capacity reached; existing snapshots are retained")
var ErrSnapshotUncertain = errors.New("snapshot published but directory durability unconfirmed; retry the same revision")

type SnapshotRef = configref.Ref

type Snapshot struct {
	Version  int      `json:"version"`
	StoreID  string   `json:"store_id"`
	Revision uint64   `json:"revision"`
	Config   Document `json:"config"`
}

func validStoreID(id string) bool { return configref.ValidHex(id, 32) }

// Snapshot captures only the current confirmed revision. The control engine
// holds its writer mutex across this operation, including the durable publish.
// Do not call it concurrently with independent writers of the same journal.
func (s *Store) Snapshot(expectedRevision uint64) (SnapshotRef, error) {
	c, err := s.Load()
	if err != nil {
		return SnapshotRef{}, err
	}
	if expectedRevision == 0 || c.Revision != expectedRevision || c.Confirmation != nil {
		return SnapshotRef{}, ErrSnapshotConflict
	}
	payload := Snapshot{Version: SnapshotVersion, StoreID: c.StoreID, Revision: c.Revision, Config: c.Config}
	raw, err := json.Marshal(payload)
	if err != nil {
		return SnapshotRef{}, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxSnapshotBytes {
		return SnapshotRef{}, errors.New("configuration snapshot exceeds byte budget")
	}
	digest := sha256.Sum256(raw)
	ref := SnapshotRef{StoreID: c.StoreID, Revision: c.Revision, Digest: hex.EncodeToString(digest[:])}
	dir, err := s.snapshotDirectory(true)
	if err != nil {
		return SnapshotRef{}, err
	}
	defer dir.Close()
	name := ref.Digest + ".json"
	if _, err := readSnapshot(dir, ref); err == nil {
		// A retry after an uncertain directory sync must establish durability
		// before acknowledging the already published immutable file.
		if err := dir.Sync(); err != nil {
			return SnapshotRef{}, errors.Join(ErrSnapshotUncertain, err)
		}
		return ref, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return SnapshotRef{}, err
	}
	names, err := dir.Readdirnames(MaxSnapshots + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return SnapshotRef{}, err
	}
	if len(names) >= MaxSnapshots {
		return SnapshotRef{}, ErrSnapshotCapacity
	}
	var nonce [16]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return SnapshotRef{}, err
	}
	tmp := ".snapshot-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return SnapshotRef{}, err
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer unix.Unlinkat(int(dir.Fd()), tmp, 0)
	_, err = f.Write(raw)
	if err == nil {
		err = f.Chmod(0400)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return SnapshotRef{}, err
	}
	if err = unix.Renameat2(int(dir.Fd()), tmp, int(dir.Fd()), name, unix.RENAME_NOREPLACE); errors.Is(err, unix.EEXIST) {
		// Never overwrite a competing or corrupted object, even if its name
		// equals our digest. Inspect the actual complete payload first.
		_, err = readSnapshot(dir, ref)
	}
	if err != nil {
		return SnapshotRef{}, err
	}
	if err = dir.Sync(); err != nil {
		return SnapshotRef{}, errors.Join(ErrSnapshotUncertain, err)
	}
	return ref, nil
}

func (s *Store) ResolveSnapshot(ref SnapshotRef) (Snapshot, error) {
	if err := ref.Validate(); err != nil {
		return Snapshot{}, err
	}
	c, err := s.Load()
	if err != nil {
		return Snapshot{}, err
	}
	if ref.StoreID != c.StoreID {
		return Snapshot{}, errors.New("snapshot belongs to a different configuration store")
	}
	dir, err := s.snapshotDirectory(false)
	if err != nil {
		return Snapshot{}, err
	}
	defer dir.Close()
	return readSnapshot(dir, ref)
}

func readSnapshot(dir *os.File, ref SnapshotRef) (Snapshot, error) {
	fd, err := unix.Openat(int(dir.Fd()), ref.Digest+".json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return Snapshot{}, err
	}
	f := os.NewFile(uintptr(fd), ref.Digest+".json")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Snapshot{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0400 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size > MaxSnapshotBytes {
		return Snapshot{}, errors.New("snapshot must be an owned private read-only bounded regular file without links")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxSnapshotBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(raw) > MaxSnapshotBytes {
		return Snapshot{}, errors.New("snapshot exceeds byte budget")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != ref.Digest {
		return Snapshot{}, errors.New("configuration snapshot digest mismatch")
	}
	var snapshot Snapshot
	if err := decodeStrictJSON(raw, &snapshot, maxDocumentNodes+32); err != nil {
		return Snapshot{}, err
	}
	if snapshot.Version != SnapshotVersion || snapshot.StoreID != ref.StoreID || snapshot.Revision != ref.Revision {
		return Snapshot{}, errors.New("snapshot provenance differs from its reference")
	}
	canonical, err := json.Marshal(snapshot)
	if err != nil || !bytes.Equal(raw, append(canonical, '\n')) {
		return Snapshot{}, errors.New("snapshot is not canonical JSON")
	}
	if err := Validate(snapshot.Config); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s *Store) snapshotDirectory(create bool) (*os.File, error) {
	root, err := s.openStateRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return snapshotDirectoryAt(root, create)
}

func (s *Store) openStateRoot() (*os.File, error) {
	fd, err := unix.Open(s.Dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	root := os.NewFile(uintptr(fd), s.Dir)
	if err := privateStateDirectory(root); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

func snapshotDirectoryAt(root *os.File, create bool) (*os.File, error) {
	fd := int(root.Fd())
	if create {
		if err := unix.Mkdirat(fd, "snapshots", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		// Also sync an existing directory: a previous failed sync may have
		// returned before its creation was durable.
		if err := root.Sync(); err != nil {
			return nil, err
		}
	}
	child, err := unix.Openat(fd, "snapshots", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(child), "snapshots")
	if err := privateStateDirectory(dir); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func privateStateDirectory(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0777 != 0700 {
		return fmt.Errorf("configuration state and snapshots require owned private 0700 directories")
	}
	return nil
}
