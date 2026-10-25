package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/configref"
)

func startupFixture(t *testing.T) (*Store, Committed, configref.Selection) {
	t.Helper()
	s, c := snapshotFixture(t)
	ref, err := s.Snapshot(c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision++
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	return s, c, configref.Selection{Generation: 10, ExpectedRevision: c.Revision, Snapshot: ref}
}

func TestStartupAdoptionIsDurableAndDoesNotReplayOverLaterCommits(t *testing.T) {
	s, before, selection := startupFixture(t)
	_, adopted, err := OpenSelected(s.Dir, "/missing-seed", &selection)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.StoreID != before.StoreID || adopted.Revision != 3 || adopted.Startup == nil || adopted.Startup.Selection != selection || adopted.Startup.Revision != 3 {
		t.Fatal("startup adoption lacks its durable generation/reference/revision", adopted)
	}
	if len(adopted.EventOutbox) != 1 || adopted.EventOutbox[0].Type != "startup_selected" || adopted.EventOutbox[0].Revision != 3 {
		t.Fatal("adoption and event were not committed together")
	}
	later := adopted
	later.Revision++
	later.LastRequestID = "later-commit"
	later.LastRequestHash = strings.Repeat("a", 64)
	if err := s.Save(later); err != nil {
		t.Fatal(err)
	}
	_, replay, err := OpenSelected(s.Dir, "/missing-seed", &selection)
	if err != nil || !reflect.DeepEqual(replay, later) {
		t.Fatal("same startup selection replaced a later commit", err)
	}
	// An ordinary unconfirmed change still belongs to normal restart rollback;
	// replay must not erase its recovery record before control sees it.
	trial := later
	trial.Revision++
	trial.Confirmation = &Confirmation{Previous: later.Config, PreviousRevision: later.Revision, RequestID: later.LastRequestID, Deadline: time.Now().Add(time.Minute).UTC()}
	if err := s.Save(trial); err != nil {
		t.Fatal(err)
	}
	_, replay, err = OpenSelected(s.Dir, "/missing-seed", &selection)
	if err != nil || !reflect.DeepEqual(replay, trial) {
		t.Fatal("replay erased unconfirmed recovery", err)
	}
}

func TestStartupRejectsStaleConflictingAndUnconfirmedSelections(t *testing.T) {
	s, _, selection := startupFixture(t)
	if _, _, err := OpenSelected(s.Dir, "/missing", &selection); err != nil {
		t.Fatal(err)
	}
	before, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*configref.Selection){
		"older generation":    func(v *configref.Selection) { v.Generation-- },
		"generation reused":   func(v *configref.Selection) { v.ExpectedRevision++ },
		"different reference": func(v *configref.Selection) { v.Snapshot.Digest = strings.Repeat("b", 64) },
		"new stale revision":  func(v *configref.Selection) { v.Generation++; v.ExpectedRevision = 1 },
		"foreign store": func(v *configref.Selection) {
			v.Generation++
			v.ExpectedRevision = before.Revision
			v.Snapshot.StoreID = strings.Repeat("b", 32)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := selection
			mutate(&bad)
			if _, _, err := OpenSelected(s.Dir, "/missing", &bad); err == nil {
				t.Fatal("invalid selection accepted")
			}
			got, err := s.Load()
			if err != nil || !reflect.DeepEqual(got, before) {
				t.Fatal("rejected selection changed journal", err)
			}
		})
	}
	trial := before
	trial.Revision++
	trial.LastRequestID = "trial"
	trial.Confirmation = &Confirmation{Previous: before.Config, PreviousRevision: before.Revision, RequestID: "trial", Deadline: time.Now().Add(time.Minute).UTC()}
	if err := s.Save(trial); err != nil {
		t.Fatal(err)
	}
	selection.Generation++
	selection.ExpectedRevision = trial.Revision
	if _, _, err := OpenSelected(s.Dir, "/missing", &selection); !errors.Is(err, ErrStartupConflict) {
		t.Fatal("new selection erased unconfirmed commit", err)
	}
}

func TestStartupDoesNotSeedMissingStoreOrReplaceCorruptSnapshot(t *testing.T) {
	s, before, selection := startupFixture(t)
	missing := filepath.Join(t.TempDir(), "absent")
	seed := filepath.Join(t.TempDir(), "seed.json")
	if err := AtomicJSON(seed, before.Config); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenSelected(missing, seed, &selection); err == nil {
		t.Fatal("explicit selection initialized an unrelated store from seed")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("failed selection created a store")
	}
	path := filepath.Join(s.Dir, "snapshots", selection.Snapshot.Digest+".json")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0400); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenSelected(s.Dir, seed, &selection); err == nil {
		t.Fatal("corrupt snapshot was accepted or replaced by seed")
	}
	got, err := s.Load()
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatal("bad snapshot changed authority", err)
	}
}

func TestStartupAtomicPublicationFailureAndReplay(t *testing.T) {
	for _, boundary := range []string{"file-synced", "before-rename", "after-rename", "directory-synced"} {
		t.Run(boundary, func(t *testing.T) {
			s, before, selection := startupFixture(t)
			cut := errors.New("injected publication interruption")
			s.checkpoint = func(at string) error {
				if at == boundary {
					return cut
				}
				return nil
			}
			if _, err := s.AdoptStartup(selection); err == nil {
				t.Fatal("fault did not reach real publication boundary")
			}
			s.checkpoint = nil
			visible, err := s.Load()
			if err != nil {
				t.Fatal("interrupted write left an unreadable journal", err)
			}
			if visible.Revision != before.Revision && visible.Revision != before.Revision+1 {
				t.Fatal("partial revision")
			}
			got, err := s.AdoptStartup(selection)
			if err != nil {
				t.Fatal(err)
			}
			if got.Revision != before.Revision+1 || got.Startup == nil || len(got.EventOutbox) != 1 {
				t.Fatal("retry repeated or lost the adoption")
			}
		})
	}
}

func TestStartupBackpressureAndGenerationMetadataCannotBeForgotten(t *testing.T) {
	s, c, selection := startupFixture(t)
	for i := 0; i < MaxEventOutbox; i++ {
		c.EventOutbox = append(c.EventOutbox, ChangeEvent{ID: fmt.Sprintf("%032x", i+1), Type: "committed", Revision: c.Revision, ObservedAt: time.Now().UTC()})
	}
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdoptStartup(selection); !errors.Is(err, ErrStartupCapacity) {
		t.Fatal("adoption dropped outstanding history to make room", err)
	}
	got, err := s.Load()
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatal("backpressure changed authoritative state", err)
	}
	c.EventOutbox = nil
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	adopted, err := s.AdoptStartup(selection)
	if err != nil {
		t.Fatal(err)
	}
	bad := adopted
	bad.Revision++
	bad.Startup = nil
	if err := s.Save(bad); err == nil {
		t.Fatal("ordinary save forgot consumed selection generation")
	}
	if err := s.Stage(bad); err == nil {
		t.Fatal("ordinary stage forgot consumed selection generation")
	}
}
