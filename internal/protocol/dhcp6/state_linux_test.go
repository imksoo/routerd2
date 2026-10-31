//go:build linux

package dhcp6

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
)

func TestRebootPDRecoveryUsesWallExpiryAndMatchingIdentity(t *testing.T) {
	before := boottime.Clock{
		BootID:              "11111111-2222-4333-8444-555555555555",
		BoottimeNanoseconds: int64(100 * time.Second),
		WallTime:            time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
	}
	saved := checkpoint{
		Version:    stateVersion,
		ObservedAt: before.WallTime,
		Snapshot: dhcp6config.Snapshot{
			BootID:        before.BootID,
			ObservedNanos: uint64(before.BoottimeNanoseconds),
			Resource:      "wan",
			Interface:     "wan0",
			IAID:          7,
			ClientDUID:    "00030001",
			ServerDUID:    "00030002",
			State:         "bound",
			Addresses: []dhcp6config.Address{{
				Address:        "2001:db8:42::100",
				PreferredUntil: lifetime.After(before, 20),
				ValidUntil:     lifetime.After(before, 30),
			}},
			Prefixes: []dhcp6config.Prefix{{
				Prefix:         "2001:db8:100::/56",
				PreferredUntil: lifetime.After(before, 20),
				ValidUntil:     lifetime.After(before, 30),
			}},
		},
	}
	after := boottime.Clock{
		BootID:              "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		BoottimeNanoseconds: int64(5 * time.Second),
		WallTime:            before.WallTime.Add(10 * time.Second),
	}

	raw, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	saved, err = decodeState(dhcp6config.Config{Resource: "wan"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	recovered := saved.snapshotForBoot(after, "wan0")
	if len(recovered.Prefixes) != 1 || recovered.Addresses != nil || recovered.BootID != after.BootID {
		t.Fatal("saved PD was not rebuilt for the new boot", recovered)
	}
	if got := recovered.Prefixes[0].PreferredUntil.Remaining(after); got != 10*time.Second {
		t.Fatal("preferred PD lifetime", got)
	}
	if got := recovered.Prefixes[0].ValidUntil.Remaining(after); got != 20*time.Second {
		t.Fatal("valid PD lifetime", got)
	}
	c := client{cfg: dhcp6config.Config{Resource: "wan", Interface: "wan0", IAID: 7, RequestPrefix: true}, duid: []byte{0, 3, 0, 1}}
	restored, err := c.restoreLease(recovered, after)
	if err != nil || !restored || c.lease.kind != rebind {
		t.Fatal("saved PD did not start Rebind", restored, err, c.lease)
	}

	for _, tc := range []struct {
		name string
		cfg  dhcp6config.Config
		duid []byte
	}{
		{name: "interface", cfg: dhcp6config.Config{Resource: "wan", Interface: "wan1", IAID: 7, RequestPrefix: true}, duid: []byte{0, 3, 0, 1}},
		{name: "iaid", cfg: dhcp6config.Config{Resource: "wan", Interface: "wan0", IAID: 8, RequestPrefix: true}, duid: []byte{0, 3, 0, 1}},
		{name: "duid", cfg: dhcp6config.Config{Resource: "wan", Interface: "wan0", IAID: 7, RequestPrefix: true}, duid: []byte{0, 3, 0, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client{cfg: tc.cfg, duid: tc.duid}
			restored, err := c.restoreLease(recovered, after)
			if err != nil || restored {
				t.Fatal("mismatched checkpoint was restored", restored, err)
			}
		})
	}

	foreign, err := decodeState(dhcp6config.Config{Resource: "other"}, raw)
	if err != nil || foreign.Version != 0 {
		t.Fatal("checkpoint for another resource did not fall back to discovery", foreign, err)
	}

	expired := after
	expired.WallTime = before.WallTime.Add(30 * time.Second)
	if got := saved.snapshotForBoot(expired, "wan0"); got.BootID != "" {
		t.Fatal("expired checkpoint survived reboot", got)
	}
}
