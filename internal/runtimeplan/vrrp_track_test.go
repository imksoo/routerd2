package runtimeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPTrackCountsActualProbesAndRequiresFreshRecoveryAfterExpiry(t *testing.T) {
	n := &memoryNetwork{}
	m := &observedManager{generation: 1}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	cfg := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Priority: 150, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Service: "probe", Penalty: 100, MaxAgeMillis: 100}}}
	probe := health.Config{Interface: "wan0", Target: "192.0.2.1", HealthyThreshold: 1, UnhealthyThreshold: 1}
	if err := r.ApplyConfig(context.Background(), 1, config.FromNative(config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &cfg}, {Name: "probe", Health: &probe}}})); err != nil {
		t.Fatal(err)
	}
	seq := uint64(0)
	publish := func(passed, failed int) serviceconfig.Update {
		t.Helper()
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		phase := old.PhaseHealthy
		if failed != 0 {
			phase = old.PhaseUnhealthy
		}
		seq++
		s := health.Snapshot{Resource: "probe", Interface: "wan0", BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Minute), Active: failed == 0, State: old.State{Phase: phase, ConsecutivePassed: passed, ConsecutiveFailed: failed}}
		u := update(t, r, "service.probe", seq, s)
		u.Generation = m.generation
		if m.generation == 2 {
			u.ProducerEpoch = strings.Repeat("b", 32)
		}
		if err := r.Observe(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	assert := func(penalized, known bool, passed int) {
		t.Helper()
		r.refreshGuardStatus(context.Background())
		r.mu.Lock()
		d, err := r.vrrpDecisionLocked("service.ha", false)
		r.mu.Unlock()
		if err != nil || len(d.Track) != 1 || d.Track[0].Penalized != penalized || d.Track[0].Known != known || d.Track[0].HealthyCount != passed {
			t.Fatal("wrong track decision", d, err)
		}
	}
	assert(true, false, 0)
	u := publish(1, 0)
	for range 5 {
		if err := r.Observe(context.Background(), u); err != nil {
			t.Fatal(err)
		}
		assert(true, true, 1)
	}
	publish(2, 0)
	assert(false, true, 2)
	publish(0, 1)
	assert(false, true, 0)
	publish(0, 2)
	assert(false, true, 0)
	publish(0, 3)
	assert(true, true, 0)
	publish(1, 0)
	assert(true, true, 1)
	publish(2, 0)
	assert(false, true, 2)
	time.Sleep(120 * time.Millisecond)
	assert(true, false, 0)
	publish(3, 0) // Raw source counter continues through a pause.
	assert(true, true, 1)
	publish(4, 0)
	assert(false, true, 2)
	m.generation = 2
	assert(true, false, 0)
	publish(100, 0) // A replacement cannot inherit the old counter authority.
	assert(true, true, 1)
	publish(101, 0)
	assert(false, true, 2)
}
