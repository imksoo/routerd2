//go:build linux

package dhcp4

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

func independentClock(t *testing.T, c *client) *bfd.Clock {
	t.Helper()
	clock := &bfd.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
	c.readClock = func() (bfd.Clock, error) { return *clock, nil }
	return clock
}

func TestLeaseT1T2AndExpiryIgnoreWallClockCorrections(t *testing.T) {
	for _, step := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(step.String(), func(t *testing.T) {
			c, conn, _, updates := fixture(t)
			clock := independentClock(t, c)
			bind(t, c)
			original := c.snapshot()
			boot, wall := clock.BoottimeNanoseconds, clock.WallTime
			for _, point := range []struct {
				seconds int64
				state   wire.State
				active  bool
			}{{39, wire.StateBound, true}, {40, wire.StateRenewing, true}, {80, wire.StateRebinding, true}, {100, wire.StateDiscovering, false}} {
				clock.BoottimeNanoseconds = boot + point.seconds*int64(time.Second)
				clock.WallTime = wall.Add(step + time.Duration(point.seconds)*time.Second)
				if err := c.tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if c.state != point.state || c.snapshot().Active != point.active {
					t.Fatalf("at boot+%ds and UTC correction %s: state=%s active=%t, want %s/%t", point.seconds, step, c.state, c.snapshot().Active, point.state, point.active)
				}
				if point.active && !c.snapshot().ExpiresAt.Equal(original.ExpiresAt) {
					t.Fatal("clock correction rewrote original display expiry")
				}
				if point.state == wire.StateRenewing && conn.sent[len(conn.sent)-1].to.String() != "192.0.2.1" {
					t.Fatal("T1 did not send an actual unicast renewal")
				}
			}
			found := false
			for _, s := range *updates {
				found = found || s.State == wire.StateExpired && !s.Active && s.CurrentAddress == ""
			}
			if !found {
				t.Fatal("original BOOTTIME deadline did not publish withdrawal")
			}
		})
	}
}

func TestRenewalACKCreatesOneNewBootDeadline(t *testing.T) {
	c, _, _, _ := fixture(t)
	clock := independentClock(t, c)
	bind(t, c)
	boot := clock.BoottimeNanoseconds
	clock.BoottimeNanoseconds = boot + int64(50*time.Second)
	clock.WallTime = clock.WallTime.Add(-24 * time.Hour)
	if err := c.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.state != wire.StateRenewing {
		t.Fatal("original T1 was ignored")
	}
	if err := c.handle(context.Background(), reply(c, wire.MessageACK)); err != nil {
		t.Fatal(err)
	}
	renewed := c.snapshot()
	clock.BoottimeNanoseconds = boot + int64(100*time.Second)
	clock.WallTime = clock.WallTime.Add(48 * time.Hour)
	if err := c.tick(context.Background()); err != nil || !c.snapshot().Active {
		t.Fatal("new server ACK did not extend the original boot lease", err)
	}
	if !c.snapshot().ExpiresAt.Equal(renewed.ExpiresAt) {
		t.Fatal("timer progress changed the accepted server expiry")
	}
	clock.BoottimeNanoseconds = boot + int64(150*time.Second)
	clock.WallTime = clock.WallTime.Add(-72 * time.Hour)
	if err := c.tick(context.Background()); err != nil || c.snapshot().Active {
		t.Fatal("renewed lease survived its own original BOOTTIME deadline", err)
	}
}

func TestRebootRequestTimeoutAndPacketSecondsUseBootClock(t *testing.T) {
	c, _, _, _ := fixture(t)
	independentClock(t, c)
	bind(t, c)
	prior := c.snapshot()
	fresh, conn, _, _ := fixture(t)
	clock := independentClock(t, fresh)
	if err := fresh.start(context.Background(), &prior); err != nil {
		t.Fatal(err)
	}
	boot := clock.BoottimeNanoseconds
	clock.BoottimeNanoseconds = boot + int64(10*time.Second)
	clock.WallTime = clock.WallTime.Add(24 * time.Hour)
	if err := fresh.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh.state != wire.StateRequesting || fresh.snapshot().Active || binary.BigEndian.Uint16(conn.sent[len(conn.sent)-1].data[8:10]) != 10 {
		t.Fatal("wall correction expired an unacknowledged reboot hint or changed DHCP secs")
	}
	clock.BoottimeNanoseconds = boot + int64(60*time.Second)
	clock.WallTime = clock.WallTime.Add(-48 * time.Hour)
	if err := fresh.tick(context.Background()); err != nil || fresh.state != wire.StateDiscovering || fresh.snapshot().Active {
		t.Fatal("reboot timeout depended on UTC", err)
	}
}

func TestLeaseClockFailureIdentityChangeAndRewindAreFatal(t *testing.T) {
	for _, fault := range []string{"read", "boot", "rewind", "wall"} {
		t.Run(fault, func(t *testing.T) {
			c, _, _, _ := fixture(t)
			clock := independentClock(t, c)
			bind(t, c)
			switch fault {
			case "read":
				c.readClock = func() (bfd.Clock, error) { return bfd.Clock{}, errors.New("injected clock failure") }
			case "boot":
				clock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			case "rewind":
				clock.BoottimeNanoseconds--
			case "wall":
				clock.WallTime = time.Time{}
			}
			if err := c.tick(context.Background()); err == nil {
				t.Fatal("invalid lease clock was accepted")
			}
		})
	}
}
