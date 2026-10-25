package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/configref"
)

var ErrStartupConflict = errors.New("startup selection conflicts with the authoritative configuration")
var ErrStartupCapacity = errors.New("startup adoption needs a durable configuration event outbox slot")
var ErrStartupUncertain = errors.New("startup adoption durability unconfirmed; read journal and retry the same selection")

// OpenSelected is called by the sole control writer before kernel reconciliation.
// Explicit selection never creates a store or uses seed as a substitute for a
// missing/corrupt journal or snapshot. Nil retains normal committed/initial-seed
// semantics. Adoption records intent first; applying and confirming it belong
// to the selected runtime and its boot-control transaction.
func OpenSelected(dir, seed string, selection *configref.Selection) (*Store, Committed, error) {
	if selection == nil {
		return Open(dir, seed)
	}
	s := &Store{Dir: dir}
	c, err := s.AdoptStartup(*selection)
	return s, c, err
}

// StartupInspection is an observation of the existing journal and selected
// immutable input, never permission to write later without rechecking. Snapshot
// is zero when AlreadyAdopted is true: the consumed receipt, including any later
// commit or confirmation, is authoritative and the input need not still exist.
type StartupInspection struct {
	Current        Committed
	Snapshot       Snapshot
	AlreadyAdopted bool
	Recovered      bool
}

// InspectStartup checks the same state, compiler and recovery fences used by
// AdoptStartup without creating state, publishing an event, fsyncing or consuming
// a generation. The caller must exclude independent configuration writers while
// inspecting and making its release decision (engine mutex, then release lock).
// The current compiler validates both the journal and snapshot; this is not an
// inspection of an incoming executable's compiler or other state families.
func (s *Store) InspectStartup(selection configref.Selection) (StartupInspection, error) {
	if err := selection.Validate(); err != nil {
		return StartupInspection{}, err
	}
	dir, err := s.openStateRoot()
	if err != nil {
		return StartupInspection{}, err
	}
	defer dir.Close()
	c, err := readStartupRecord(dir)
	if err != nil {
		return StartupInspection{Current: c}, err
	}
	if selection.Snapshot.StoreID != c.StoreID {
		return StartupInspection{Current: c}, ErrStartupConflict
	}
	if c.Startup != nil {
		old := c.Startup.Selection
		if selection.Generation < old.Generation || selection.Generation == old.Generation && !selection.Equal(old) {
			return StartupInspection{Current: c}, ErrStartupConflict
		}
		if selection.Equal(old) {
			// A consumed receipt is authoritative even after later commits or
			// removal of its input snapshot. Inspection must neither reimport it
			// nor perform the durability sync required when adoption is retried.
			return StartupInspection{Current: c, AlreadyAdopted: true, Recovered: c.Startup.Recovered}, nil
		}
	}
	if c.Confirmation != nil {
		return StartupInspection{Current: c}, ErrStartupConflict
	}
	recovered := false
	if selection.Recovery == nil {
		if c.Revision != selection.ExpectedRevision {
			return StartupInspection{Current: c}, ErrStartupConflict
		}
	} else {
		r := selection.Recovery
		switch {
		case c.Revision == selection.ExpectedRevision:
			if c.Startup != nil && c.Startup.Selection.Generation >= r.FailedGeneration {
				return StartupInspection{Current: c}, ErrStartupConflict
			}
		case c.Revision == selection.ExpectedRevision+1:
			failed := configref.Selection{Generation: r.FailedGeneration, ExpectedRevision: selection.ExpectedRevision, Snapshot: r.FailedSnapshot}
			if c.Startup == nil || !c.Startup.Selection.Equal(failed) || c.Startup.Revision != c.Revision || c.Startup.Recovered {
				return StartupInspection{Current: c}, ErrStartupConflict
			}
			recovered = true
		default:
			return StartupInspection{Current: c}, ErrStartupConflict
		}
	}
	if len(c.EventOutbox) >= MaxEventOutbox {
		return StartupInspection{Current: c}, ErrStartupCapacity
	}
	snapshots, err := snapshotDirectoryAt(dir, false)
	if err != nil {
		return StartupInspection{Current: c}, err
	}
	defer snapshots.Close()
	snapshot, err := readSnapshot(snapshots, selection.Snapshot)
	if err != nil {
		return StartupInspection{Current: c}, err
	}
	if selection.Recovery != nil && !recovered {
		// Before adoption, the live baseline must still match the prepared
		// known-good snapshot. After adoption the exact durable receipt and
		// unchanged public revision are the fence; do not depend on a failed
		// candidate payload remaining readable in order to recover from it.
		before, _ := json.Marshal(c.Config)
		expected, _ := json.Marshal(snapshot.Config)
		if !bytes.Equal(before, expected) {
			return StartupInspection{Current: c}, ErrStartupConflict
		}
	}
	return StartupInspection{Current: c, Snapshot: snapshot, Recovered: recovered}, nil
}

// AdoptStartup requires exclusive configuration-writer ownership. It is not an
// online mutation API and never writes the kernel or changes boot selection.
func (s *Store) AdoptStartup(selection configref.Selection) (Committed, error) {
	check, err := s.InspectStartup(selection)
	c := check.Current
	if err != nil {
		return c, err
	}
	if check.AlreadyAdopted {
		if err := syncDir(s.Dir); err != nil {
			return c, errors.Join(ErrStartupUncertain, err)
		}
		return c, nil
	}
	snapshot, recovered := check.Snapshot, check.Recovered
	var eventID [16]byte
	if _, err := rand.Read(eventID[:]); err != nil {
		return c, err
	}
	next := Committed{StoreID: c.StoreID, Revision: c.Revision + 1, Config: snapshot.Config, Startup: &configref.Adoption{Selection: selection.Clone(), Revision: c.Revision + 1, Recovered: recovered}}
	next.EventOutbox = append([]ChangeEvent(nil), c.EventOutbox...)
	next.EventOutbox = append(next.EventOutbox, ChangeEvent{ID: hex.EncodeToString(eventID[:]), Type: "startup_selected", Revision: next.Revision, ObservedAt: time.Now().UTC()})
	if err := s.Save(next); err != nil {
		return c, errors.Join(ErrStartupUncertain, err)
	}
	return next, nil
}
