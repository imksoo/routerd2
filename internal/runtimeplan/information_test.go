package runtimeplan

import (
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
)

func TestInformationUsesOriginalBootBudgetAndSurvivingAssignments(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0)}
	s := dhcp6config.Snapshot{BootID: clock.BootID, State: "information", InfoReceivedNanos: uint64(clock.BoottimeNanoseconds), InformationRefreshAt: lifetime.After(clock, 30)}
	for _, step := range []time.Duration{24 * time.Hour, -24 * time.Hour} {
		c := clock
		c.WallTime = c.WallTime.Add(step)
		if until, ok := s.InformationUntil(c); !ok || until != s.InformationRefreshAt {
			t.Fatal(until, ok)
		}
	}
	c := clock
	c.BoottimeNanoseconds += int64(30 * time.Second)
	if _, ok := s.InformationUntil(c); ok {
		t.Fatal("expired information survived")
	}
	s.InformationRefreshAt = lifetime.Deadline{}
	if _, ok := s.InformationUntil(clock); ok {
		t.Fatal("missing deadline became infinity")
	}
	s.InformationRefreshAt = lifetime.After(clock, ^uint32(0))
	if until, ok := s.InformationUntil(clock); !ok || until.BootID != clock.BootID || until.Nanos != 0 {
		t.Fatal("explicit scoped infinity lost")
	}
	s.State = "bound"
	s.Addresses = []dhcp6config.Address{{Address: "2001:db8::1", PreferredUntil: lifetime.After(clock, 60), ValidUntil: lifetime.After(clock, 90)}}
	if until, ok := s.InformationUntil(clock); !ok || until != s.Addresses[0].ValidUntil {
		t.Fatal("IA_NA did not bound information")
	}
	s.Prefixes = []dhcp6config.Prefix{{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clock, 100), ValidUntil: lifetime.After(clock, 180)}}
	c = clock
	c.BoottimeNanoseconds += int64(91 * time.Second)
	if until, ok := s.InformationUntil(c); !ok || until != s.Prefixes[0].ValidUntil {
		t.Fatal("IA_NA expiry discarded a surviving PD")
	}
	c.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, ok := s.InformationUntil(c); ok {
		t.Fatal("old boot revived information")
	}
	s.InfoReceivedNanos = 0
	if _, ok := s.InformationUntil(clock); ok {
		t.Fatal("information authorized before Reply")
	}
}
