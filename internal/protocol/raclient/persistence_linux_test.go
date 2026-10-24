//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package raclient

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRestartUsesBootElapsedTimeAcrossWallClockSteps(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 100, 0, pio("2001:db8:1::/64", 0xc0, 300, 150), rdnss(200, "fe80::53")), testClock(0))
	record := s.record("boot-a", testClock(10))
	for _, jump := range []time.Duration{-24 * time.Hour, 24 * time.Hour, 0} {
		now := testClock(40)
		now.wall = now.wall.Add(jump)
		restarted := testState(t, testConfig())
		if err := restarted.restoreRecord(record, "boot-a", now); err != nil {
			t.Fatal(err)
		}
		snap := restarted.snapshot(now)
		if len(snap.Routes) != 1 || snap.Routes[0].ValidUntil.Sub(now.wall) != 60*time.Second || snap.Addresses[0].ValidUntil.Sub(now.wall) != 260*time.Second || snap.DNSServers[0].ValidUntil.Sub(now.wall) != 160*time.Second {
			t.Fatalf("clock step extended/shortened lifetime: %+v", snap)
		}
		// Same protection survives restart: a forged PIO zero cannot shorten
		// the remaining 260-second address lifetime, nor reset it to 300.
		got := mustHandle(t, restarted, advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, 0, 0)), now)
		if got.Addresses[0].ValidUntil.Sub(now.wall) != 260*time.Second || !got.Addresses[0].Deprecated {
			t.Fatal("restart lost the two-hour state")
		}
	}
	restarted := testState(t, testConfig())
	if err := restarted.restoreRecord(record, "boot-a", testClock(301)); err != nil {
		t.Fatal(err)
	}
	if len(restarted.learned.Addresses)+len(restarted.learned.Routes)+len(restarted.learned.DNSServers) != 0 {
		t.Fatal("expired state resurrected after restart")
	}
}

func TestJournalPreservesClockAnchorAcrossStatusOnlySavesAndRestart(t *testing.T) {
	s := testState(t, testConfig())
	first := mustHandle(t, s, advert("fe80::2", 100, 0, pio("2001:db8:1::/64", 0xc0, 300, 150), rio("2001:db8:2::/48", 0, 120), rdnss(200, "fe80::53"), dnssl(210)), testClock(0))
	bootFacts := learnedData(s.learned)
	path := filepath.Join(t.TempDir(), "ra.json")
	for i, jitter := range []time.Duration{7521 * time.Nanosecond, -400 * time.Millisecond, 800 * time.Microsecond} {
		now := testClock(10 * (i + 1))
		now.wall = now.wall.Add(jitter)
		s.learned.Solicitations++
		record := s.record("boot-a", now)
		if !record.SavedAt.Equal(record.Snapshot.UpdatedAt) || !record.SavedAt.Equal(testClock(10*(i+1)).wall) || !reflect.DeepEqual(learnedData(record.Snapshot), learnedData(first)) {
			t.Fatal("journal mixed sampled wall time with anchored lifetime timestamps")
		}
		if err := saveState(path, record); err != nil {
			t.Fatal(err)
		}
		restarted := testState(t, testConfig())
		now = testClock(10*(i+1) + 1)
		now.wall = now.wall.Add(-jitter)
		if err := restarted.restore(path, "boot-a", now); err != nil {
			t.Fatal(err)
		}
		after := restarted.snapshot(now)
		if err := ValidateSnapshot(restarted.config, after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(bootFacts, learnedData(restarted.learned)) || !reflect.DeepEqual(learnedData(first), learnedData(after)) {
			t.Fatal("same-boot save/restore accumulated lifetime or UTC deadline drift")
		}
		s = restarted
	}
	// The restored anchor must still detect a true step and preserve remaining
	// protocol lifetime rather than preserving obsolete UTC wall coordinates.
	now := testClock(40)
	now.wall = now.wall.Add(time.Hour)
	after := s.snapshot(now)
	if after.Addresses[0].ValidUntil.Sub(after.UpdatedAt) != 260*time.Second || len(after.Routes) != 2 {
		t.Fatal("persisted anchor hid a true clock step or extended a lease")
	}
	for _, route := range after.Routes {
		want := map[string]time.Duration{"::/0": 60 * time.Second, "2001:db8:2::/48": 80 * time.Second}[route.Destination]
		if want == 0 || route.ValidUntil.Sub(after.UpdatedAt) != want {
			t.Fatal("restored route did not retain its independent boot-clock lifetime")
		}
	}
}

func TestRebootOrIdentityChangeDropsLearningExplicitly(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 100, 0, pio("2001:db8:1::/64", 0xc0, ^uint32(0), ^uint32(0))), testClock(0))
	record := s.record("boot-a", testClock(1))
	for _, change := range []string{"boot", "identity"} {
		cfg := testConfig()
		boot := "boot-a"
		if change == "boot" {
			boot = "boot-b"
		} else {
			cfg.InterfaceID = "::5678"
		}
		s := testState(t, cfg)
		if err := s.restoreRecord(record, boot, testClock(2)); err != nil {
			t.Fatal(err)
		}
		if len(s.learned.Addresses)+len(s.learned.Routes) != 0 || s.learned.LastError == "" {
			t.Fatal("stale state adopted after ownership epoch changed")
		}
	}
	record.SavedBootTime = testClock(100).boot
	if err := testState(t, testConfig()).restoreRecord(record, "boot-a", testClock(2)); err == nil {
		t.Fatal("future monotonic timestamp accepted")
	}
}

func TestDurablePrivateStateAndExclusiveOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first, err := lockState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := lockState(path); err == nil {
		second.Close()
		t.Fatal("second live state writer acquired flock")
	}
	s := testState(t, testConfig())
	record := s.record("boot-a", testClock(0))
	if err := saveState(path, record); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("journal not private")
	}
	got, exists, err := readState(path)
	if err != nil || !exists || got.Identity != record.Identity {
		t.Fatalf("durable roundtrip: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readState(path); err == nil {
		t.Fatal("world-readable state accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "symlink.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readState(link); err == nil {
		t.Fatal("symlink state followed")
	}
	if err := os.WriteFile(path, []byte("{}\n{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readState(path); err == nil {
		t.Fatal("trailing journal content accepted")
	}
}
