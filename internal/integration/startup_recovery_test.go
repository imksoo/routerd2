//go:build linux && integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/vishvananda/netlink"
)

func TestDaemonConditionalStartupRecovery(t *testing.T) {
	if !isolateFixture(t, "TestDaemonConditionalStartupRecovery") {
		return
	}
	dir := t.TempDir()
	ctx := context.Background()
	doc := func(alias string) config.Document {
		return config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lo", Alias: &alias}}}})
	}
	a, b, c := doc("recovery-A"), doc("recovery-B"), doc("recovery-C")
	f := startFixtureDaemonAt(t, a, dir)
	state := func() control.State {
		t.Helper()
		var s control.State
		if err := f.control.Do(ctx, "GET", "/v1/config", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	check := func(revision uint64, alias string, selection *configref.Selection, recovered bool) {
		t.Helper()
		reported := false
		until(t, 20*time.Second, func() bool {
			s := state()
			link, err := netlink.LinkByName("lo")
			if err != nil {
				t.Fatal(err)
			}
			ok := s.Revision == revision && s.Healthy && s.Services != nil && s.Services.Phase == "ready" && s.Services.Revision == revision && link.Attrs().Alias == alias && s.Startup != nil && s.Startup.Selection.Equal(*selection) && s.Startup.Recovered == recovered
			if !ok && !reported {
				reported = true
				t.Logf("await recovery readiness: revision=%d want=%d healthy=%v alias=%s last=%q journal=%q store=%q rule=%q derived=%q protocol=%q services=%+v", s.Revision, revision, s.Healthy, link.Attrs().Alias, s.LastError, s.EventJournalError, s.EventStoreError, s.EventRuleError, s.DerivedError, s.ProtocolEventError, s.Services)
			}
			return ok
		})
	}
	capture := func(revision uint64) config.SnapshotRef {
		t.Helper()
		var r config.SnapshotRef
		if err := f.control.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: revision}, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	commit := func(revision uint64, id string, d config.Document) {
		t.Helper()
		if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: revision, RequestID: id, Config: d}, nil); err != nil {
			t.Fatal(err)
		}
	}
	target := capture(1)
	commit(1, "recovery-base-B", b)
	baseline := capture(2)
	f.stop()
	trial := configref.Selection{Generation: 20, ExpectedRevision: 2, Snapshot: target}
	f = startFixtureDaemonSelected(t, a, dir, &trial)
	check(3, "recovery-A", &trial, false)
	f.stop()
	if err := os.Remove(filepath.Join(dir, "state", "snapshots", target.Digest+".json")); err != nil {
		t.Fatal(err)
	}
	recovery := configref.Selection{Generation: 21, ExpectedRevision: 2, Snapshot: baseline, Recovery: &configref.Recovery{FailedGeneration: 20, FailedSnapshot: target}}
	f = startFixtureDaemonSelected(t, a, dir, &recovery)
	check(4, "recovery-B", &recovery, true)
	f = f.restart()
	check(4, "recovery-B", &recovery, true)
	commit(4, "recovery-later-C", c)
	f = f.restart()
	check(5, "recovery-C", &recovery, true)
	current := capture(5)
	f.stop()
	// Generation22 never starts: recover the unchanged prepared baseline.
	before := configref.Selection{Generation: 23, ExpectedRevision: 5, Snapshot: current, Recovery: &configref.Recovery{FailedGeneration: 22, FailedSnapshot: target}}
	f = startFixtureDaemonSelected(t, a, dir, &before)
	check(6, "recovery-C", &before, false)
	f = f.restart()
	check(6, "recovery-C", &before, false)
	until(t, 5*time.Second, func() bool {
		var page control.ObservationPage
		if f.control.Do(ctx, "GET", "/v1/observations?after_cursor=0&limit=16", nil, &page) != nil {
			return false
		}
		count := 0
		for _, r := range page.Records {
			if r.Event.Topic == "configuration.startup_selected" {
				count++
			}
		}
		return count == 3
	})
	t.Log("ROUTERD_STARTUP_RECOVERY_DAEMON_TEST_PASS: exact post-adoption recovery, consumed recovery restart, later commit preserved, pre-adoption recovery and three stable lifecycle events")
}
