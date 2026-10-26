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
	record := journal{Version: 2, Identity: s.identity, Snapshot: s.snapshot(testClock(10))}
	for _, jump := range []time.Duration{-24 * time.Hour, 24 * time.Hour, 0} {
		now := testClock(40)
		now.WallTime = now.WallTime.Add(jump)
		restarted := testState(t, testConfig())
		if err := restarted.restoreRecord(record, now); err != nil {
			t.Fatal(err)
		}
		snap := restarted.snapshot(now)
		if len(snap.Routes) != 1 || snap.Routes[0].ValidUntil.Remaining(now) != 60*time.Second || snap.Addresses[0].ValidUntil.Remaining(now) != 260*time.Second || snap.DNSServers[0].ValidUntil.Remaining(now) != 160*time.Second {
			t.Fatalf("clock step extended/shortened lifetime: %+v", snap)
		}
		// Same protection survives restart: a forged PIO zero cannot shorten
		// the remaining 260-second address lifetime, nor reset it to 300.
		got := mustHandle(t, restarted, advert("fe80::2", 0, 0, pio("2001:db8:1::/64", 0xc0, 0, 0)), now)
		if got.Addresses[0].ValidUntil.Remaining(now) != 260*time.Second || !got.Addresses[0].Deprecated {
			t.Fatal("restart lost the two-hour state")
		}
	}
	restarted := testState(t, testConfig())
	if err := restarted.restoreRecord(record, testClock(301)); err != nil {
		t.Fatal(err)
	}
	if len(restarted.learned.Addresses)+len(restarted.learned.Routes)+len(restarted.learned.DNSServers) != 0 {
		t.Fatal("expired state resurrected after restart")
	}
}

func TestJournalRetainsOriginalDeadlinesWithoutClockMapping(t *testing.T) {
	s := testState(t, testConfig())
	first := mustHandle(t, s, advert("fe80::2", 100, 0, pio("2001:db8:1::/64", 0xc0, 300, 150), rdnss(200, "fe80::53")), testClock(0))
	path := filepath.Join(t.TempDir(), "ra.json")
	for _, jump := range []time.Duration{-24 * time.Hour, 24 * time.Hour, 500 * time.Millisecond} {
		now := testClock(40)
		now.WallTime = now.WallTime.Add(jump)
		record := journal{Version: 2, Identity: s.identity, Snapshot: s.snapshot(now)}
		if !reflect.DeepEqual(learnedData(first), learnedData(record.Snapshot)) {
			t.Fatal("status save rewrote original RA deadlines")
		}
		if err := saveState(path, record); err != nil {
			t.Fatal(err)
		}
		next := testState(t, testConfig())
		if err := next.restore(path, now); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(learnedData(first), learnedData(next.snapshot(now))) {
			t.Fatal("restart rebased RA authority")
		}
	}
}

func TestRebootOrIdentityChangeDropsLearningExplicitly(t *testing.T) {
	s := testState(t, testConfig())
	mustHandle(t, s, advert("fe80::2", 100, 0, pio("2001:db8:1::/64", 0xc0, ^uint32(0), ^uint32(0))), testClock(0))
	record := journal{Version: 2, Identity: s.identity, Snapshot: s.snapshot(testClock(1))}
	for _, change := range []string{"boot", "identity"} {
		cfg := testConfig()
		now := testClock(2)
		if change == "boot" {
			now.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
		} else {
			cfg.InterfaceID = "::5678"
		}
		s := testState(t, cfg)
		if err := s.restoreRecord(record, now); err != nil {
			t.Fatal(err)
		}
		if len(s.learned.Addresses)+len(s.learned.Routes) != 0 || s.learned.LastError == "" {
			t.Fatal("stale state adopted after ownership epoch changed")
		}
	}
	record.Snapshot.ObservedNanos = uint64(testClock(100).BoottimeNanoseconds)
	if err := testState(t, testConfig()).restoreRecord(record, testClock(2)); err == nil {
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
	record := journal{Version: 2, Identity: s.identity, Snapshot: s.snapshot(testClock(0))}
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
