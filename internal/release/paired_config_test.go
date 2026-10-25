//go:build linux

package release

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/network"
)

// Exercise the actual signed release store and actual configuration journal,
// with a round trip through their on-disk records. No executable is launched;
// this is not the coordinated PID 1/health/commit-fence acceptance gate.
func TestPairedReleaseFallbackDrivesExactConfigurationRecovery(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate not adopted", true: "candidate adopted"}[adopted], func(t *testing.T) {
			ctx := context.Background()
			root := newTestStore(t)
			initializeFixture(t, root)
			next := stageFixture(t, root, "fixture-r2", 2)
			seed := filepath.Join(t.TempDir(), "seed.json")
			if err := config.AtomicJSON(seed, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
				t.Fatal(err)
			}
			cfg, c, err := config.Open(filepath.Join(t.TempDir(), "state"), seed)
			if err != nil {
				t.Fatal(err)
			}
			c.Revision = 3
			if err := cfg.Save(c); err != nil {
				t.Fatal(err)
			}
			target, err := cfg.Snapshot(3)
			if err != nil {
				t.Fatal(err)
			}
			c.Revision = 7
			c.Config = config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lo", MTU: 1400}}}})
			if err := cfg.Save(c); err != nil {
				t.Fatal(err)
			}
			baseline, err := cfg.Snapshot(7)
			if err != nil {
				t.Fatal(err)
			}
			plan := configref.Selection{Generation: 20, ExpectedRevision: 7, Snapshot: target}
			if _, err := root.Prepare(ctx, next, 1, PrepareOptions{Startup: &plan, KnownGood: &baseline}); err != nil {
				t.Fatal(err)
			}
			boot, err := root.BeginBoot(ctx, "paired-config-kernel")
			if err != nil {
				t.Fatal(err)
			}
			boot.Image.Close()
			visible := mustState(t, root)
			if adopted {
				if _, err := cfg.AdoptStartup(*visible.Startup); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := root.FailBoot(ctx, "paired-config-kernel", visible.Attempt.Token, "candidate startup failed"); err != nil {
				t.Fatal(err)
			}
			fallback := mustState(t, root)
			current, err := cfg.AdoptStartup(*fallback.Startup)
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(8)
			if adopted {
				want = 9
			}
			if current.Revision != want || current.Startup.Recovered != adopted || !reflect.DeepEqual(current.Config, c.Config) {
				t.Fatal("separate journals disagreed about fallback branch or operator intent")
			}
		})
	}
}
