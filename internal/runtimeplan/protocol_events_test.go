package runtimeplan

import (
	"context"
	"testing"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestProtocolHistoryRequiresCurrentRegisteredWorkerWithoutRenewingFacts(t *testing.T) {
	ctx := context.Background()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	m := &observedManager{generation: 1}
	r.manager = m
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1", Protocol: "tcp", Port: 80}}}}}
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal(err)
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	s := health.Snapshot{Resource: "wan", Interface: "wan0", BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(time.Minute), Active: true, State: old.State{Phase: old.PhaseHealthy, ConsecutivePassed: 1}}
	u := update(t, r, "service.wan", 1, s)
	if err = r.Accept(ctx, u); err != nil {
		t.Fatal(err)
	}
	before := r.records[u.Name]
	u.Snapshot = nil
	u.Sequence = 0
	calls := 0
	appendHistory := func(name string) error {
		calls++
		if name != "wan" {
			t.Fatal(name)
		}
		return nil
	}
	if err = r.WithProtocolProducer(ctx, u, appendHistory); err != nil || calls != 1 {
		t.Fatal(calls, err)
	}
	if string(before.Snapshot) != string(r.records[u.Name].Snapshot) || before.Sequence != r.records[u.Name].Sequence {
		t.Fatal("historical delivery changed current facts")
	}
	for _, change := range []func(*serviceconfig.Update){func(v *serviceconfig.Update) { v.SupervisorInstance = "old" }, func(v *serviceconfig.Update) { v.Generation++ }, func(v *serviceconfig.Update) { v.ProducerEpoch = "ffffffffffffffffffffffffffffffff" }, func(v *serviceconfig.Update) { v.ConfigDigest = "old" }, func(v *serviceconfig.Update) { v.Role = "bgp" }} {
		v := u
		change(&v)
		if err = r.WithProtocolProducer(ctx, v, appendHistory); err == nil || calls != 1 {
			t.Fatal("stale producer appended history", v, err)
		}
	}
	m.generation = 2 // old producer is still in records; supervisor already replaced it
	if err = r.WithProtocolProducer(ctx, u, appendHistory); err == nil || calls != 1 {
		t.Fatal("old worker survived supervisor replacement", err)
	}
}
