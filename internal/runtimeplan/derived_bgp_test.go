package runtimeplan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func bgpDerivedFixture(t *testing.T, conditional bool, hold string) (*Runtime, derived.Program) {
	t.Helper()
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.manager = &observedManager{generation: 1}
	spec := api.DerivedEventSpec{Topic: "routerd.bgp.fixture", EmitInitial: true, Hysteresis: hold, Inputs: []api.ReadyWhenSpec{{Field: "${BGPRouter/service/edge.status.phase}", Equals: "Established"}, {Field: "${BGPRouter/service/edge.status.backend}", Equals: "gobgp"}, {Field: "${BGPRouter/service/edge.status.establishedPeers}", Equals: "1"}, {Field: "${BGPRouter/service/edge.status.observedAt}", NotEmpty: true}}}
	native := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, Services: []serviceconfig.Service{{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002}}}, FIBInterface: "wan0"}}}}
	source := config.FromNative(native)
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "derived", DerivedEvent: &spec})
	if conditional {
		source.Resources = append(source.Resources, resourceprogram.Resource{ID: "conditional", Interface: &network.Interface{Name: "unused0"}, When: &resourceprogram.FactCondition{Fact: "service.edge.active"}})
	}
	if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
		t.Fatal(err)
	}
	reconcileConditions(t, r)
	p, err := derived.Compile("derived", spec)
	if err != nil {
		t.Fatal(err)
	}
	return r, p
}

func TestBGPOriginalDeadlineAndLiveGenerationFence(t *testing.T) {
	r, p := bgpDerivedFixture(t, false, "")
	ctx := context.Background()
	s := bgpDerivedSample(t, 1, true)
	// UTC can be corrected independently; only the original boot clock expires
	// this grant. Do not derive a fresh receipt age from the future UTC value.
	s.ObservedAt = s.ObservedAt.Add(time.Hour)
	s.ValidUntil = s.ObservedAt.Add(bgpconfig.ObservationLifetime)
	s.Routes = []bgpconfig.Route{{Prefix: "203.0.113.0/24", NextHop: "192.0.2.2", Interface: "wan0", PeerAddress: "192.0.2.2", Table: 254, Metric: 200}}
	u := update(t, r, "service.edge", 1, s)
	if err := observeAndProject(ctx, r, u); err != nil {
		t.Fatal(err)
	}
	up, err := p.Advance(nil, bfdDerivedInputs(t, r), false)
	if err != nil || up.Status.Asserted == nil || !*up.Status.Asserted || up.ObservedNanos != s.ObservedNanos {
		t.Fatal(up, err)
	}
	bad := s
	bad.Peers = append([]bgpconfig.Peer(nil), s.Peers...)
	bad.Peers[0].State = "SESSION_STATE_IDLE"
	if err := observeAndProject(ctx, r, update(t, r, "service.edge", 2, bad)); err == nil {
		t.Fatal("changed inner sequence replay accepted")
	}
	u.Sequence = 2
	if err := observeAndProject(ctx, r, u); err != nil {
		t.Fatal("identical inner observation retry rejected", err)
	}
	refreshed, err := p.Advance(up.State, bfdDerivedInputs(t, r), false)
	if err != nil || refreshed.ObservedNanos != up.ObservedNanos || refreshed.ValidUntilNanos > s.ExpiresNanos {
		t.Fatal("transport retry renewed observation proof", refreshed, err)
	}
	time.Sleep(5100 * time.Millisecond)
	r.refreshGuardStatus(ctx)
	expired, err := p.Advance(refreshed.State, bfdDerivedInputs(t, r), false)
	if err != nil || expired.Status.Asserted != nil || len(expired.Emissions) != 0 {
		t.Fatal("old UTC value revived source or fabricated Down", expired, err)
	}
	reconcileConditions(t, r)
	if len(r.network.(*memoryNetwork).applied.Routes) != 0 {
		t.Fatal("expired boot clock retained BGP routes")
	}
	r.manager.(*observedManager).generation = 2
	r.refreshGuardStatus(ctx)
	if err := observeAndProject(ctx, r, update(t, r, "service.edge", 3, bgpDerivedSample(t, 3, true))); !errors.Is(err, ErrStale) {
		t.Fatal("old worker seeded state despite live supervisor generation", err)
	}
}

func bgpDerivedSample(t *testing.T, sequence uint64, up bool) bgpconfig.Snapshot {
	t.Helper()
	state := "SESSION_STATE_IDLE"
	if up {
		state = "SESSION_STATE_ESTABLISHED"
	}
	s := bgpconfig.Snapshot{Resource: "edge", Active: true, Epoch: strings.Repeat("b", 32), Sequence: sequence, Peers: []bgpconfig.Peer{{Address: "192.0.2.2", ASN: 65002, State: state, AdminState: "ADMIN_STATE_UP"}}}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	bgpconfig.StampObservation(&s, clock)
	return s
}

func TestBGPRouterPhaseIncludesCurrentUnresolvedFIB(t *testing.T) {
	r, p := bgpDerivedFixture(t, false, "")
	s := bgpDerivedSample(t, 1, true)
	s.Routes = []bgpconfig.Route{{Prefix: "203.0.113.0/24", NextHop: "198.51.100.1", Interface: "wan0", PeerAddress: "192.0.2.2", Table: 254, Metric: 200}}
	if err := observeAndProject(context.Background(), r, update(t, r, "service.edge", 1, s)); err != nil {
		t.Fatal(err)
	}
	degraded, err := p.Advance(nil, bfdDerivedInputs(t, r), false)
	if err != nil || degraded.Status.Asserted == nil || *degraded.Status.Asserted {
		t.Fatal("unresolved current FIB was not known Degraded", degraded, err)
	}
	s.Sequence = 2
	s.Routes[0].NextHop = "192.0.2.2"
	if err = observeAndProject(context.Background(), r, update(t, r, "service.edge", 2, s)); err != nil {
		t.Fatal(err)
	}
	established, err := p.Advance(degraded.State, bfdDerivedInputs(t, r), false)
	if err != nil || established.Status.Asserted == nil || !*established.Status.Asserted || len(established.Emissions) != 1 {
		t.Fatal("resolved current source did not assert", established, err)
	}
}

func TestBGPConditionalApplyFenceAndHeartbeatDoNotInventContinuity(t *testing.T) {
	r, p := bgpDerivedFixture(t, true, "150ms")
	ctx := context.Background()
	if err := r.Observe(ctx, update(t, r, "service.edge", 1, bgpDerivedSample(t, 1, false))); err != nil {
		t.Fatal(err)
	}
	before, err := p.Advance(nil, bfdDerivedInputs(t, r), false)
	if err != nil || before.Status.Asserted != nil {
		t.Fatal("unapplied BGP source became current", before, err)
	}
	reconcileConditions(t, r)
	down, err := p.Advance(before.State, bfdDerivedInputs(t, r), false)
	if err != nil || down.Status.Asserted == nil || *down.Status.Asserted {
		t.Fatal(down, err)
	}
	if err = r.Observe(ctx, update(t, r, "service.edge", 2, bgpDerivedSample(t, 2, true))); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Advance(down.State, bfdDerivedInputs(t, r), false)
	if err != nil || pending.Status.PendingTransition != "Asserting" {
		t.Fatal("unchanged applied route projection was not reusable", pending, err)
	}
	if err = r.Observe(ctx, update(t, r, "service.edge", 3, bgpDerivedSample(t, 3, true))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(170 * time.Millisecond)
	up, err := p.Advance(pending.State, bfdDerivedInputs(t, r), false)
	if err != nil || len(up.Emissions) != 1 {
		t.Fatal("same-projection heartbeat erased the hold", up, err)
	}
	changed := bgpDerivedSample(t, 4, true)
	changed.Routes = []bgpconfig.Route{{Prefix: "203.0.113.0/24", NextHop: "192.0.2.2", Interface: "wan0", PeerAddress: "192.0.2.2", Table: 254, Metric: 200}}
	if err = r.Observe(ctx, update(t, r, "service.edge", 4, changed)); err != nil {
		t.Fatal(err)
	}
	unknown, err := p.Advance(up.State, bfdDerivedInputs(t, r), false)
	if err != nil || unknown.Status.Asserted != nil || len(unknown.Emissions) != 0 {
		t.Fatal("changed unapplied routes inherited current phase", unknown, err)
	}
	reconcileConditions(t, r)
	fresh, err := p.Advance(unknown.State, bfdDerivedInputs(t, r), false)
	if err != nil || fresh.Status.Asserted == nil || !*fresh.Status.Asserted {
		t.Fatal("applied replacement did not recover", fresh, err)
	}
}
