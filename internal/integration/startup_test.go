//go:build linux && integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
)

func TestDaemonStartupSelectionAndRecovery(t *testing.T) {
	if !isolateFixture(t, "TestDaemonStartupSelectionAndRecovery") {
		return
	}
	// Inherited or Env overrides must never create startup authority.
	t.Setenv(configref.SelectionEnv, `{"generation":999}`)
	dir := t.TempDir()
	document := func(alias string) config.Document {
		return config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lo", Alias: &alias}}}})
	}
	a, b, c, d := document("startup-A"), document("startup-B"), document("startup-C"), document("startup-D")
	f := startFixtureDaemonAt(t, a, dir)
	ctx := context.Background()
	read := func() control.State {
		t.Helper()
		var s control.State
		if err := f.control.Do(ctx, "GET", "/v1/config", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	assert := func(rev uint64, alias string, generation uint64) {
		t.Helper()
		reported := false
		until(t, 20*time.Second, func() bool {
			s := read()
			link, err := netlink.LinkByName("lo")
			if err != nil {
				t.Fatal(err)
			}
			origin := generation == 0 || s.Startup != nil && s.Startup.Selection.Generation == generation && s.Startup.Selection.Snapshot.StoreID == s.StoreID
			projection := s.Services != nil && s.Services.Phase == "ready" && s.Services.Revision == rev
			ok := s.Healthy && s.Revision == rev && link.Attrs().Alias == alias && origin && projection
			if !ok && !reported {
				reported = true
				t.Logf("await full startup readiness: revision=%d want=%d healthy=%v alias=%s want_alias=%s origin=%v last=%q event_journal=%q event_store=%q rule=%q derived=%q protocol=%q services=%+v", s.Revision, rev, s.Healthy, link.Attrs().Alias, alias, origin, s.LastError, s.EventJournalError, s.EventStoreError, s.EventRuleError, s.DerivedError, s.ProtocolEventError, s.Services)
			}
			return ok
		})
	}

	assert(1, "startup-A", 0)
	var savedA, savedB config.SnapshotRef
	if err := f.control.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: 1}, &savedA); err != nil {
		t.Fatal(err)
	}
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "startup-B", Config: b}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.control.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: 2}, &savedB); err != nil {
		t.Fatal(err)
	}
	assert(2, "startup-B", 0)
	f.stop()
	selection := configref.Selection{Generation: 10, ExpectedRevision: 2, Snapshot: savedA}
	f = startFixtureDaemonSelected(t, a, dir, &selection)
	assert(3, "startup-A", 10)
	var status supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	var victim supervisor.ModuleStatus
	for _, m := range status.Modules {
		if m.PID <= 0 {
			if m.Required {
				t.Fatal("missing required worker")
			}
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(m.PID), "environ"))
		if err != nil {
			t.Fatal(err)
		}
		var startup string
		for _, v := range strings.Split(string(raw), "\x00") {
			if strings.HasPrefix(v, configref.SelectionEnv+"=") {
				startup = strings.TrimPrefix(v, configref.SelectionEnv+"=")
			}
		}
		if m.Role == "control" {
			decoded, err := configref.DecodeSelection([]byte(startup))
			if err != nil || decoded == nil || *decoded != selection {
				t.Fatal("control did not receive fixed selected startup", err)
			}
			victim = m
		} else if startup != "" {
			t.Fatal("startup selection leaked into unrelated worker authority")
		}
	}
	if victim.PID <= 0 {
		t.Fatal("missing control worker")
	}
	if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 15*time.Second, func() bool {
		var s supervisor.Status
		if f.supervisor.Do(ctx, "GET", "/v1/status", nil, &s) != nil || !s.Ready {
			return false
		}
		for _, m := range s.Modules {
			if m.Role == "control" {
				return m.PID != victim.PID && m.Generation > victim.Generation
			}
		}
		return false
	})
	assert(3, "startup-A", 10)
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 3, RequestID: "startup-C", Config: c}, nil); err != nil {
		t.Fatal(err)
	}
	f = f.restart()
	assert(4, "startup-C", 10)
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 4, RequestID: "startup-unconfirmed-D", Config: d, ConfirmTimeoutSeconds: 60}, nil); err != nil {
		t.Fatal(err)
	}
	f = f.restart()
	assert(6, "startup-C", 10)
	f.stop()
	next := configref.Selection{Generation: 11, ExpectedRevision: 6, Snapshot: savedB}
	f = startFixtureDaemonSelected(t, a, dir, &next)
	assert(7, "startup-B", 11)
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
		return count == 2
	})
	t.Log("ROUTERD_STARTUP_SELECTION_DAEMON_TEST_PASS: explicit snapshot adoption, control SIGKILL, later-commit preservation across whole daemon restart, confirmed restart rollback, next selection and stable lifecycle events")
}
