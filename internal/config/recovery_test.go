package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/network"
)

func recoveryFixture(t *testing.T) (*Store, Committed, configref.Selection, configref.Selection) {
	t.Helper()
	s, c := snapshotFixture(t)
	target, err := s.Snapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = 2
	c.Config = FromNative(Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lo", MTU: 1400}}}})
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	baseline, err := s.Snapshot(2)
	if err != nil {
		t.Fatal(err)
	}
	candidate := configref.Selection{Generation: 10, ExpectedRevision: 2, Snapshot: target}
	recovery := configref.Selection{Generation: 11, ExpectedRevision: 2, Snapshot: baseline, Recovery: &configref.Recovery{FailedGeneration: 10, FailedSnapshot: target}}
	return s, c, candidate, recovery
}

func TestConditionalRecoveryBeforeAndAfterCandidateAdoption(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before adoption", true: "after adoption"}[consumed], func(t *testing.T) {
			s, baseline, candidate, recovery := recoveryFixture(t)
			if consumed {
				if _, err := s.AdoptStartup(candidate); err != nil {
					t.Fatal(err)
				}
			}
			c, err := s.AdoptStartup(recovery)
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(3)
			if consumed {
				want = 4
			}
			if c.Revision != want || c.Startup == nil || c.Startup.Recovered != consumed || !c.Startup.Selection.Equal(recovery) || !reflect.DeepEqual(c.Config, baseline.Config) {
				t.Fatal("recovery used the wrong branch or configuration")
			}
			// Reopening decodes a different pointer; semantic equality must still
			// identify the consumed selection and its actual revision branch.
			_, again, err := OpenSelected(s.Dir, "/missing", &recovery)
			if err != nil || !reflect.DeepEqual(again, c) {
				t.Fatal("recovery replay reapplied snapshot", err)
			}
			c.Revision++
			if err := s.Save(c); err != nil {
				t.Fatal(err)
			}
			_, again, err = OpenSelected(s.Dir, "/missing", &recovery)
			if err != nil || again.Revision != c.Revision {
				t.Fatal("recovery replay replaced later commit", err)
			}
		})
	}
}

func TestConditionalRecoveryCannotOverwriteLaterOrUnrelatedConfiguration(t *testing.T) {
	for _, kind := range []string{"later commit", "different failed selection", "same revision different baseline"} {
		t.Run(kind, func(t *testing.T) {
			s, c, candidate, recovery := recoveryFixture(t)
			switch kind {
			case "later commit":
				var err error
				c, err = s.AdoptStartup(candidate)
				if err != nil {
					t.Fatal(err)
				}
				c.Revision++
				if err := s.Save(c); err != nil {
					t.Fatal(err)
				}
			case "different failed selection":
				var err error
				c, err = s.AdoptStartup(candidate)
				if err != nil {
					t.Fatal(err)
				}
				r := *recovery.Recovery
				r.FailedSnapshot.Digest = strings.Repeat("e", 64)
				recovery.Recovery = &r
			case "same revision different baseline":
				c.Config = Document{SchemaVersion: SchemaVersion}
				if err := s.Save(c); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.AdoptStartup(recovery); !errors.Is(err, ErrStartupConflict) {
				t.Fatal("unrelated configuration accepted as a failed candidate", err)
			}
			got, err := s.Load()
			if err != nil || !reflect.DeepEqual(got, c) {
				t.Fatal("refused recovery changed authoritative journal", err)
			}
		})
	}
}

func TestConditionalRecoveryPublicationFailureRetainsExactReceipt(t *testing.T) {
	s, _, candidate, recovery := recoveryFixture(t)
	if _, err := s.AdoptStartup(candidate); err != nil {
		t.Fatal(err)
	}
	cut := errors.New("after-rename interruption")
	s.checkpoint = func(at string) error {
		if at == "after-rename" {
			return cut
		}
		return nil
	}
	if _, err := s.AdoptStartup(recovery); !errors.Is(err, ErrStartupUncertain) {
		t.Fatal("publication uncertainty was hidden", err)
	}
	s.checkpoint = nil
	c, err := s.AdoptStartup(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if c.Revision != 4 || c.Startup == nil || !c.Startup.Recovered || len(c.EventOutbox) != 2 {
		t.Fatal("replay lost or repeated recovered branch")
	}
	bad := c
	receipt := *c.Startup
	receipt.Recovered = false
	receipt.Revision = 3
	bad.Startup = &receipt
	if err := s.Save(bad); err == nil {
		t.Fatal("ordinary save changed the consumed recovery branch")
	}
}
