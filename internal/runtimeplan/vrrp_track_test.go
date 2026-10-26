package runtimeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPTrackCountsActualProbesAndRequiresFreshRecoveryAfterExpiry(t *testing.T) {
	n := &memoryNetwork{}
	m := &observedManager{generation: 1}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	cfg := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Priority: 150, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Resource: "service/probe", Penalty: 100, MaxAgeMillis: 100}}}
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
		if err != nil || len(d.Track) != 1 || d.Track[0].Penalized != penalized || (d.Track[0].State != vrrp.TrackUnknown) != known || d.Track[0].HealthyCount != passed {
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

func TestVRRPConditionalTrackIsNeutralUntilItsOriginalConditionExpires(t *testing.T) {
	ctx := context.Background()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	cfg := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Priority: 150, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Resource: "probe", Penalty: 100, MaxAgeMillis: 1500}}}
	probe := health.Config{Interface: "wan0", Target: "192.0.2.1", HealthyThreshold: 1, UnhealthyThreshold: 1}
	program := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "vip", Service: &serviceconfig.Service{Name: "ha", VRRP: &cfg}},
		{ID: "probe", When: &resourceprogram.FactCondition{Fact: "service.gate.active", MaxAge: "500ms"}, Service: &serviceconfig.Service{Name: "probe", Health: &probe}},
		{ID: "gate", Service: &serviceconfig.Service{Name: "gate", Health: &probe}},
	}}
	if err := r.ApplyConfig(ctx, 1, program); err != nil {
		t.Fatal(err)
	}
	sequences := map[string]uint64{}
	publish := func(name string, passed, failed int) {
		t.Helper()
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		sequences[name]++
		phase := old.PhaseHealthy
		if failed > 0 {
			phase = old.PhaseUnhealthy
		}
		sample := health.Snapshot{Resource: name, Interface: "wan0", BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Minute), Active: failed == 0, State: old.State{Phase: phase, ConsecutivePassed: passed, ConsecutiveFailed: failed}}
		if err := r.Observe(ctx, update(t, r, "service."+name, sequences[name], sample)); err != nil {
			t.Fatal(err)
		}
	}
	decision := func() vrrp.TrackStatus {
		t.Helper()
		r.refreshGuardStatus(ctx)
		r.mu.Lock()
		defer r.mu.Unlock()
		result, err := r.vrrpDecisionLocked("service.ha", false)
		if err != nil || len(result.Track) != 1 {
			t.Fatal(result, err)
		}
		return result.Track[0]
	}
	publish("gate", 1, 0)
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	r.refreshGuardStatus(ctx)
	publish("gate", 2, 0)
	publish("probe", 1, 0)
	publish("probe", 2, 0)
	healthy := decision()
	if healthy.State != vrrp.TrackHealthy || healthy.Penalized || healthy.HealthyCount != 2 {
		t.Fatal(healthy)
	}
	publish("gate", 0, 1)
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal("conditional exclusion invalidated track reference", err)
	}
	for range 3 {
		neutral := decision()
		if neutral.State != vrrp.TrackNeutral || neutral.Penalized != healthy.Penalized || neutral.HealthyCount != healthy.HealthyCount || neutral.UnhealthyCount != healthy.UnhealthyCount || neutral.ExpiresNanos == 0 {
			t.Fatal("neutral changed the retained decision", neutral, healthy)
		}
	}
	time.Sleep(520 * time.Millisecond)
	expired := decision()
	if expired.State != vrrp.TrackUnknown || !expired.Penalized || expired.ExpiresNanos != 0 {
		t.Fatal("expired condition retained neutral authority", expired)
	}
}

func TestVRRPBFDAndBGPTracksUseDistinctCurrentStatusObservations(t *testing.T) {
	for _, role := range []string{"bfd", "bgp"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			doc := bfdBGPDocument()
			doc.Services[1].BGP.PeerBFD = nil
			source := "edge"
			if role == "bgp" {
				source = "router"
			}
			cfg := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, Priority: 150, Addresses: []string{"192.0.2.254/32"}, Track: []vrrp.Track{{Resource: "service/" + source, Penalty: 100, MaxAgeMillis: 300}}}
			doc.Services = append(doc.Services, serviceconfig.Service{Name: "ha", VRRP: &cfg})
			writer := &memoryNetwork{}
			r := New(t.TempDir(), t.TempDir(), writer, testSupervisorInstance)
			r.manager = &observedManager{generation: 1}
			if err := r.ApplyConfig(ctx, 1, config.FromNative(doc)); err != nil {
				t.Fatal(err)
			}
			publish := func(sequence uint64) {
				t.Helper()
				var u serviceconfig.Update
				if role == "bfd" {
					clock, err := bfd.ReadClock()
					if err != nil {
						t.Fatal(err)
					}
					sample := observedBFD(t, doc.Services[0].BFDConfig(), clock)
					sample.Peers[0].Received = sequence + 1
					u = update(t, r, "service.edge", sequence, sample)
				} else {
					sample := observedBGPRoutes()
					sample.Sequence = sequence
					sample.Routes = nil
					u = update(t, r, "service.router", sequence, sample)
				}
				if err := observeAndProject(ctx, r, u); err != nil {
					t.Fatal(err)
				}
			}
			decision := func() vrrp.TrackStatus {
				t.Helper()
				r.refreshGuardStatus(ctx)
				r.mu.Lock()
				defer r.mu.Unlock()
				result, err := r.vrrpDecisionLocked("service.ha", false)
				if err != nil || len(result.Track) != 1 {
					t.Fatal(result, err)
				}
				return result.Track[0]
			}
			publish(1)
			for range 3 {
				state := decision()
				if state.State != vrrp.TrackHealthy || !state.Penalized || state.HealthyCount != 1 {
					t.Fatal("polling confirmed a cached status", state)
				}
			}
			publish(2)
			if state := decision(); state.Penalized || state.HealthyCount != 2 {
				t.Fatal("fresh status did not release penalty", state)
			}
			if role == "bgp" {
				blocked, release := blockBFDNetwork(t, r, writer, nil)
				done := make(chan error, 1)
				go func() { done <- r.reconcileDynamic(ctx) }()
				<-blocked.entered
				during := decision()
				release()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if during.Penalized || during.HealthyCount != 2 {
					t.Fatal("unrelated kernel IO revoked matching acknowledged BGP status", during)
				}
			}
			time.Sleep(320 * time.Millisecond)
			if state := decision(); state.State != vrrp.TrackUnknown || !state.Penalized {
				t.Fatal("expired status retained priority", state)
			}
		})
	}
}
