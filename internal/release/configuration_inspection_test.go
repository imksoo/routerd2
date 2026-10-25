//go:build linux

package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/network"
)

// Actual signed releases plus actual configuration/snapshot files. This proves
// the preflight transaction and configuration input checks, not paired boot or
// target executable/compiler, other state, engine or kernel qualification.
func TestCheckedPrepareInspectsBothConfigurationInputsBeforeReservingAuthority(t *testing.T) {
	for _, failure := range []string{"valid", "candidate missing", "fallback missing", "baseline changed", "revision advanced"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			root := newTestStore(t)
			initializeFixture(t, root)
			target := stageFixture(t, root, "fixture-r2", 2)
			seed := filepath.Join(t.TempDir(), "seed.json")
			if err := config.AtomicJSON(seed, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
				t.Fatal(err)
			}
			cfg, current, err := config.Open(filepath.Join(t.TempDir(), "state"), seed)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := cfg.Snapshot(1)
			if err != nil {
				t.Fatal(err)
			}
			current.Revision++
			current.Config = config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lo", MTU: 1400}}}})
			if err := cfg.Save(current); err != nil {
				t.Fatal(err)
			}
			baseline, err := cfg.Snapshot(2)
			if err != nil {
				t.Fatal(err)
			}
			selection := configref.Selection{Generation: 10, ExpectedRevision: 2, Snapshot: candidate}
			fallback := configref.Selection{Generation: 11, ExpectedRevision: 2, Snapshot: baseline, Recovery: &configref.Recovery{FailedGeneration: 10, FailedSnapshot: candidate}}
			want := config.ErrStartupConflict
			switch failure {
			case "candidate missing", "fallback missing":
				ref := candidate
				if failure == "fallback missing" {
					ref = baseline
				}
				if err := os.Remove(filepath.Join(cfg.Dir, "snapshots", ref.Digest+".json")); err != nil {
					t.Fatal(err)
				}
				want = os.ErrNotExist
			case "baseline changed":
				current.Config = config.Document{SchemaVersion: config.SchemaVersion}
				if err := cfg.Save(current); err != nil {
					t.Fatal(err)
				}
			case "revision advanced":
				current.Revision++
				if err := cfg.Save(current); err != nil {
					t.Fatal(err)
				}
			}
			before := mustState(t, root)
			journal, err := os.ReadFile(filepath.Join(cfg.Dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			checks := 0
			got, err := root.PrepareChecked(ctx, target, before.Generation, PrepareOptions{Startup: &selection, KnownGood: &baseline}, func(_ context.Context, _ State, image *Image) error {
				if image.Release != target {
					t.Fatal("wrong executable selected for inspection")
				}
				for _, input := range []configref.Selection{selection, fallback} {
					checked, err := cfg.InspectStartup(input)
					if err != nil {
						return err
					}
					if checked.AlreadyAdopted || checked.Recovered {
						return config.ErrStartupConflict
					}
					checks++
				}
				return nil
			})
			if failure != "valid" {
				if !errors.Is(err, want) || !reflect.DeepEqual(before, mustState(t, root)) {
					t.Fatal("invalid input reserved or changed release authority", err)
				}
			} else if err != nil || checks != 2 || got.Pending == nil || !got.Pending.Startup.Equal(selection) || !got.Pending.Fallback.Equal(fallback) {
				t.Fatal("valid inspected pair was not prepared", err)
			}
			after, readErr := os.ReadFile(filepath.Join(cfg.Dir, "config.json"))
			if readErr != nil || string(journal) != string(after) {
				t.Fatal("preparation adopted or rewrote configuration", readErr)
			}
		})
	}
}
