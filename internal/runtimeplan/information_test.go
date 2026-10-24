package runtimeplan

import (
	"testing"
	"time"

	old6 "github.com/imksoo/routerd/pkg/pdclient"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
)

func TestInformationDeadlineRequiresFreshExplicitInformation(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	base := dhcp6pd.Snapshot{Active: true, InformationOnly: true, InformationRefreshAt: now.Add(30 * time.Second), Snapshot: old6.Snapshot{InfoUpdatedAt: now, ExpiresAt: now.Add(60 * time.Second)}}
	for _, check := range []struct {
		name   string
		change func(*dhcp6pd.Snapshot)
		until  time.Time
		usable bool
	}{
		{"finite info", func(*dhcp6pd.Snapshot) {}, now.Add(30 * time.Second), true},
		{"not received", func(s *dhcp6pd.Snapshot) { s.InfoUpdatedAt = time.Time{} }, time.Time{}, false},
		{"inactive", func(s *dhcp6pd.Snapshot) { s.Active = false }, time.Time{}, false},
		{"stale", func(s *dhcp6pd.Snapshot) { s.InformationStale = true }, time.Time{}, false},
		{"missing deadline", func(s *dhcp6pd.Snapshot) { s.InformationRefreshAt = time.Time{} }, time.Time{}, false},
		{"finite seconds without deadline", func(s *dhcp6pd.Snapshot) { s.InformationRefreshAt = time.Time{}; s.InformationRefreshSeconds = 600 }, time.Time{}, false},
		{"explicit infinity", func(s *dhcp6pd.Snapshot) {
			s.InformationRefreshAt = time.Time{}
			s.InformationRefreshSeconds = ^uint32(0)
		}, time.Time{}, true},
		{"infinity before first reply", func(s *dhcp6pd.Snapshot) {
			s.InformationRefreshAt = time.Time{}
			s.InformationRefreshSeconds = ^uint32(0)
			s.InfoUpdatedAt = time.Time{}
		}, time.Time{}, false},
		{"infinity bounded by PD", func(s *dhcp6pd.Snapshot) {
			s.InformationOnly = false
			s.InformationRefreshAt = time.Time{}
			s.InformationRefreshSeconds = ^uint32(0)
		}, now.Add(60 * time.Second), true},
		{"missing PD lifetime", func(s *dhcp6pd.Snapshot) { s.InformationOnly = false; s.ExpiresAt = time.Time{} }, time.Time{}, false},
		{"PD earlier", func(s *dhcp6pd.Snapshot) { s.InformationOnly = false; s.ExpiresAt = now.Add(10 * time.Second) }, now.Add(10 * time.Second), true},
		{"information earlier", func(s *dhcp6pd.Snapshot) { s.InformationOnly = false }, now.Add(30 * time.Second), true},
		{"information expires now", func(s *dhcp6pd.Snapshot) { s.InformationRefreshAt = now }, now, false},
		{"infinite refresh with expired PD", func(s *dhcp6pd.Snapshot) {
			s.InformationOnly = false
			s.InformationRefreshAt = time.Time{}
			s.InformationRefreshSeconds = ^uint32(0)
			s.ExpiresAt = now
		}, now, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			snapshot := base
			check.change(&snapshot)
			until, usable := informationDeadline(snapshot, now)
			if !until.Equal(check.until) || usable != check.usable {
				t.Fatalf("got %v %v, want %v %v", until, usable, check.until, check.usable)
			}
		})
	}
}
