package config

import (
	"crypto/rand"
	"encoding/hex"
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

// AdoptStartup requires exclusive configuration-writer ownership. It is not an
// online mutation API and never writes the kernel or changes boot selection.
func (s *Store) AdoptStartup(selection configref.Selection) (Committed, error) {
	if err := selection.Validate(); err != nil {
		return Committed{}, err
	}
	c, err := s.Load()
	if err != nil {
		return c, err
	}
	if selection.Snapshot.StoreID != c.StoreID {
		return c, ErrStartupConflict
	}
	if c.Startup != nil {
		old := c.Startup.Selection
		if selection.Generation < old.Generation || selection.Generation == old.Generation && selection != old {
			return c, ErrStartupConflict
		}
		if selection == old {
			// Journal is now authoritative, including later normal commits and
			// any unconfirmed recovery record. Never reimport the old snapshot.
			if err := syncDir(s.Dir); err != nil {
				return c, errors.Join(ErrStartupUncertain, err)
			}
			return c, nil
		}
	}
	if c.Revision != selection.ExpectedRevision || c.Confirmation != nil {
		return c, ErrStartupConflict
	}
	if len(c.EventOutbox) >= MaxEventOutbox {
		return c, ErrStartupCapacity
	}
	snapshot, err := s.ResolveSnapshot(selection.Snapshot)
	if err != nil {
		return c, err
	}
	var eventID [16]byte
	if _, err := rand.Read(eventID[:]); err != nil {
		return c, err
	}
	next := Committed{StoreID: c.StoreID, Revision: c.Revision + 1, Config: snapshot.Config, Startup: &configref.Adoption{Selection: selection, Revision: c.Revision + 1}}
	next.EventOutbox = append([]ChangeEvent(nil), c.EventOutbox...)
	next.EventOutbox = append(next.EventOutbox, ChangeEvent{ID: hex.EncodeToString(eventID[:]), Type: "startup_selected", Revision: next.Revision, ObservedAt: time.Now().UTC()})
	if err := s.Save(next); err != nil {
		return c, errors.Join(ErrStartupUncertain, err)
	}
	return next, nil
}
