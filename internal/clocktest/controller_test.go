//go:build linux

package clocktest

import (
	"errors"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

func validGuest() Guest {
	nonce := "0123456789abcdef0123456789abcdef"
	return Guest{PID: 1, Suite: SuiteName, Nonce: nonce, CommandLine: "console=ttyS0 routerd2.clock-test=" + nonce}
}

func TestClockControllerAcceptsOnlyNamedDedicatedClockFixtures(t *testing.T) {
	for _, name := range []string{SuiteName, VRRPSuiteName, DHCP6SuiteName} {
		g := validGuest()
		g.Suite = name
		clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: 1, WallTime: time.Now().UTC()}
		if _, err := NewController(g, func() (boottime.Clock, error) { return clock, nil }, func(time.Time) error { return nil }); err != nil {
			t.Fatal("fixed dedicated clock fixture rejected", name, err)
		}
	}
	for _, name := range []string{"daemon-vrrp", "daemon-vrrp-clock-extra", "network"} {
		g := validGuest()
		g.Suite = name
		called := false
		if _, err := NewController(g, func() (boottime.Clock, error) { called = true; return boottime.Clock{}, nil }, func(time.Time) error { called = true; return nil }); err == nil || called {
			t.Fatal("ordinary suite reached clock I/O", name)
		}
	}
}

func TestClockControllerRefusesHostAndAmbiguousBootBeforeAnyClockIO(t *testing.T) {
	for _, change := range []func(*Guest){
		func(g *Guest) { g.PID = 123 },
		func(g *Guest) { g.Suite = "daemon-dhcp4" },
		func(g *Guest) { g.Nonce = "bad" },
		func(g *Guest) { g.CommandLine = "console=ttyS0" },
		func(g *Guest) { g.CommandLine += " routerd2.clock-test=ffffffffffffffffffffffffffffffff" },
		func(g *Guest) { g.CommandLine += " routerd2.clock-test=" + g.Nonce },
	} {
		g := validGuest()
		change(&g)
		called := 0
		_, err := NewController(g, func() (boottime.Clock, error) { called++; return boottime.Clock{}, nil }, func(time.Time) error { called++; return nil })
		if err == nil || called != 0 {
			t.Fatal("unsafe guest reached clock IO", g, err, called)
		}
	}
}

func TestClockControllerUsesOriginalBootBaselineAndOnlyFixedActions(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
	origin := clock
	writes := 0
	c, err := NewController(validGuest(), func() (boottime.Clock, error) { return clock, nil }, func(t time.Time) error { writes++; clock.WallTime = t; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		action string
		offset time.Duration
	}{{"forward", 24 * time.Hour}, {"backward", -24 * time.Hour}, {"restore", 0}, {"backward", -24 * time.Hour}, {"restore", 0}} {
		clock.BoottimeNanoseconds += int64(7 * time.Second)
		got, err := c.Step(step.action)
		want := origin.WallTime.Add(time.Duration(clock.BoottimeNanoseconds-origin.BoottimeNanoseconds) + step.offset)
		if err != nil || !got.Clock.WallTime.Equal(want) || got.Clock.BootID != origin.BootID || got.Clock.BoottimeNanoseconds != clock.BoottimeNanoseconds {
			t.Fatal(step, got, want, err)
		}
	}
	before := writes
	if _, err := c.Step("set arbitrary time"); err == nil || writes != before {
		t.Fatal("unbounded clock mutation accepted", err, writes)
	}
}

func TestClockControllerRejectsBootChangeRewindAndSetterFailure(t *testing.T) {
	for _, fault := range []string{"read", "boot", "rewind", "set"} {
		t.Run(fault, func(t *testing.T) {
			clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
			readErr, setErr := error(nil), error(nil)
			writes := 0
			c, err := NewController(validGuest(), func() (boottime.Clock, error) { return clock, readErr }, func(time.Time) error { writes++; return setErr })
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "read":
				readErr = errors.New("clock read")
			case "boot":
				clock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			case "rewind":
				clock.BoottimeNanoseconds--
			case "set":
				setErr = errors.New("clock set")
			}
			if _, err := c.Step("forward"); err == nil {
				t.Fatal("clock fault hidden")
			}
			if fault != "set" && writes != 0 {
				t.Fatal("invalid clock reached setter")
			}
		})
	}
}
