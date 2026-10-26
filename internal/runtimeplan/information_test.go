package runtimeplan

import (
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"testing"
	"time"
)

func TestInformationDeadlineRequiresFreshReplyAndSurvivingAssignments(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	s := dhcp6config.Snapshot{State: "information", InfoUpdatedAt: now, InformationRefreshAt: now.Add(30 * time.Second)}
	if until, ok := s.InformationUntil(now); !ok || !until.Equal(s.InformationRefreshAt) {
		t.Fatal(until, ok)
	}
	if _, ok := s.InformationUntil(now.Add(30 * time.Second)); ok {
		t.Fatal("expired information survived")
	}
	s.InformationRefreshAt = time.Time{}
	if _, ok := s.InformationUntil(now); ok {
		t.Fatal("missing deadline became infinite")
	}
	s.InformationRefreshSeconds = ^uint32(0)
	if until, ok := s.InformationUntil(now); !ok || !until.IsZero() {
		t.Fatal("explicit infinity was lost")
	}
	s.State = "bound"
	s.Addresses = []dhcp6config.Address{{Address: "2001:db8::1", PreferredUntil: now.Add(60 * time.Second), ValidUntil: now.Add(90 * time.Second)}}
	if until, ok := s.InformationUntil(now); !ok || !until.Equal(s.Addresses[0].ValidUntil) {
		t.Fatal("IA_NA did not bound information", until, ok)
	}
	s.Prefixes = []dhcp6config.Prefix{{Prefix: "2001:db8:100::/56", PreferredUntil: now.Add(100 * time.Second), ValidUntil: now.Add(180 * time.Second)}}
	if until, ok := s.InformationUntil(now.Add(91 * time.Second)); !ok || !until.Equal(s.Prefixes[0].ValidUntil) {
		t.Fatal("expired IA_NA discarded surviving IA_PD", until, ok)
	}
	s.InfoUpdatedAt = time.Time{}
	if _, ok := s.InformationUntil(now); ok {
		t.Fatal("infinity authorized information before a Reply")
	}
}
