package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func bfdDocument() config.Native {
	return config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, Services: []serviceconfig.Service{{Name: "edge", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2"}}}}}}
}

func bfdBGPDocument() config.Native {
	doc := bfdDocument()
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "router", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 65001, RouterID: "192.0.2.1", ListenPort: -1}, Peers: map[string]bgpconfig.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 65002}}}, FIBInterface: "wan0", PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", BindInterface: "wan0"}}, PeerBFD: map[string]bgpconfig.BFDReference{"192.0.2.2": {Service: "edge"}}}})
	return doc
}

func observedBGPRoutes() bgpconfig.Snapshot {
	clock, err := bfd.ReadClock()
	if err != nil {
		panic(err)
	}
	return observedBGPRoutesAt(clock)
}

func observedBGPRoutesAt(clock bfd.Clock) bgpconfig.Snapshot {
	s := bgpconfig.Snapshot{Resource: "router", Epoch: strings.Repeat("b", 32), Sequence: 1, Active: true, Peers: []bgpconfig.Peer{{Address: "192.0.2.2", ASN: 65002, State: "SESSION_STATE_ESTABLISHED", AdminState: "ADMIN_STATE_UP"}}, Routes: []bgpconfig.Route{{Prefix: "203.0.113.0/24", NextHop: "192.0.2.2", Interface: "wan0", PeerAddress: "192.0.2.2", Table: 254, Metric: 200}}}
	bgpconfig.StampObservation(&s, clock)
	return s
}

func proofOf(g bgpconfig.BFDGrant) bgpconfig.BFDAcknowledgement {
	return bgpconfig.BFDAcknowledgement{ConfigDigest: g.ConfigDigest, Generation: g.Generation, ProducerEpoch: g.ProducerEpoch, DownTransitions: g.DownTransitions, Enabled: true}
}

func TestBFDProjectionFencesRecipientAndFIBRequiresSessionAcknowledgement(t *testing.T) {
	ctx := context.Background()
	doc := bfdBGPDocument()
	r, n, _ := fixture(t, doc)
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	seen := observedBFD(t, doc.Services[0].BFDConfig(), clock)
	if err := observeAndProject(ctx, r, update(t, r, "service.edge", 1, seen)); err != nil {
		t.Fatal(err)
	}
	digest := r.bindings["service.router"].digest
	if _, err := r.BFDProjection("service.router", digest, 1); !errors.Is(err, ErrStale) {
		t.Fatal("BFD projection served before recipient status acquired its generation", err)
	}
	bgp := observedBGPRoutes()
	if err := observeAndProject(ctx, r, update(t, r, "service.router", 1, bgp)); err != nil {
		t.Fatal(err)
	}
	if len(n.applied.Routes) != 0 {
		t.Fatal("BFD-bound route installed without session gate acknowledgement")
	}
	view, err := r.BFDProjection("service.router", digest, 1)
	if err != nil || len(view.Peers) != 1 || !view.Peers["192.0.2.2"].Up {
		t.Fatalf("current typed health missing: %+v, %v", view, err)
	}
	for _, test := range []struct {
		name, digest string
		generation   uint64
	}{{"service.edge", digest, 1}, {"service.router", strings.Repeat("f", 64), 1}, {"service.router", digest, 2}} {
		if _, err := r.BFDProjection(test.name, test.digest, test.generation); !errors.Is(err, ErrStale) {
			t.Fatal("unbound recipient obtained a health grant", test, err)
		}
	}
	bgp.BFD = map[string]bgpconfig.BFDAcknowledgement{"192.0.2.2": proofOf(view.Peers["192.0.2.2"])}
	bgp.Sequence = 2
	if err := observeAndProject(ctx, r, update(t, r, "service.router", 2, bgp)); err != nil || len(n.applied.Routes) != 1 {
		t.Fatal("acknowledged current BFD path not installed", err, n.applied.Routes)
	}
	// Coalescing retains the failure counter even though the latest raw BFD
	// state is already Up again. The old BGP session proof must cease to own
	// its route before a replacement acknowledgement arrives.
	seen.Peers[0].Received += 2
	seen.Peers[0].Transitions = 4
	seen.Peers[0].UpTransitions = 2
	seen.Peers[0].DownTransitions = 1
	if err := observeAndProject(ctx, r, update(t, r, "service.edge", 2, seen)); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil || len(n.applied.Routes) != 0 {
		t.Fatal("coalesced BFD failure kept a pre-reset BGP route", err, n.applied.Routes)
	}
	view, err = r.BFDProjection("service.router", digest, 1)
	if err != nil {
		t.Fatal(err)
	}
	bgp.BFD["192.0.2.2"] = proofOf(view.Peers["192.0.2.2"])
	bgp.Sequence = 3
	if err := observeAndProject(ctx, r, update(t, r, "service.router", 3, bgp)); err != nil || len(n.applied.Routes) != 1 {
		t.Fatal("new session proof did not restore its path", err, n.applied.Routes)
	}
	// Source worker replacement has its own identity fence, independently of
	// whether the new process happens to start with the same counters.
	next := update(t, r, "service.edge", 1, seen)
	next.Generation = 2
	next.ProducerEpoch = strings.Repeat("c", 32)
	if err := observeAndProject(ctx, r, next); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil || len(n.applied.Routes) != 0 {
		t.Fatal("old session proof survived BFD source process replacement", err, n.applied.Routes)
	}
	for _, query := range []string{"", "?config_digest=" + digest, "?config_digest=" + digest + "&generation=0", "?config_digest=" + digest + "&generation=1&generation=1", "?config_digest=" + digest + "&generation=1&extra=1"} {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/services/service.router/bfd"+query, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatal("ambiguous BFD recipient query accepted", query, w.Code)
		}
	}
}

func TestBFDFIBUsesExplicitMonotonicFactsDespiteWallClockRollback(t *testing.T) {
	doc := bfdBGPDocument()
	clock := bfd.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Unix(1800000000, 0).UTC()}
	bfdSnapshot := observedBFD(t, doc.Services[0].BFDConfig(), clock)
	bfdBytes, _ := json.Marshal(bfdSnapshot)
	source := serviceconfig.Update{ConfigDigest: strings.Repeat("d", 64), Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: 1, Snapshot: bfdBytes}
	bgp := observedBGPRoutesAt(clock)
	bgp.BFD = map[string]bgpconfig.BFDAcknowledgement{"192.0.2.2": {ConfigDigest: source.ConfigDigest, Generation: source.Generation, ProducerEpoch: source.ProducerEpoch, Enabled: true}}
	bgpBytes, _ := json.Marshal(bgp)
	records := map[string]serviceconfig.Update{"service.edge": source, "service.router": {Snapshot: bgpBytes}}
	p, err := projectWithFacts(doc, "/run/routerd", "/var/lib/routerd", records, clock.WallTime, projectionFacts{bfdClock: clock})
	if err != nil || len(p.network.Routes) != 1 {
		t.Fatal("fresh proof not projected", err, p.network.Routes)
	}
	if route := p.network.Routes[0]; route.BootID != clock.BootID || route.ExpiresNanos != uint64(bfdSnapshot.Peers[0].DeadlineBootNanoseconds) {
		t.Fatal("BGP heartbeat extended its required BFD receive deadline", route)
	}
	clock.BoottimeNanoseconds += int64(time.Second)
	p, err = projectWithFacts(doc, "/run/routerd", "/var/lib/routerd", records, clock.WallTime.Add(-time.Hour), projectionFacts{bfdClock: clock})
	if err != nil || len(p.network.Routes) != 0 {
		t.Fatal("wall-clock rollback prolonged BFD FIB ownership", err, p.network.Routes)
	}
	p, err = project(doc, "/run/routerd", "/var/lib/routerd", records, clock.WallTime)
	if err != nil || len(p.network.Routes) != 0 {
		t.Fatal("missing kernel-clock evidence granted BFD FIB ownership", err, p.network.Routes)
	}
}

func observedBFD(t *testing.T, cfg bfd.Config, clock bfd.Clock) bfd.Snapshot {
	t.Helper()
	arrival := clock.WallTime.Add(-10 * time.Millisecond)
	monotonic := clock.BoottimeNanoseconds - int64(10*time.Millisecond)
	snapshot := bfd.Snapshot{Resource: cfg.Resource, Interface: cfg.Interface, SourceAddress: cfg.SourceAddress, BootID: clock.BootID, Active: true, TimestampChanges: uint64(clock.BoottimeNanoseconds), ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(bfd.StatusLifetime), UpdatedAt: clock.WallTime, Peers: []bfd.PeerState{{Address: cfg.Peers[0].Address, ObservedSource: cfg.Peers[0].Address, State: bfd.StateUp, RemoteState: bfd.StateUp, LocalDiscriminator: 1, RemoteDiscriminator: 2, SourcePort: 49152, DesiredMinTxMicroseconds: 300000, RequiredMinRxMicroseconds: 300000, RemoteMinTxMicroseconds: 300000, RemoteMinRxMicroseconds: 300000, RemoteDetectMultiplier: 3, TransmitIntervalMicroseconds: 300000, DetectionTimeMicroseconds: 900000, LastReceivedAt: arrival, LastReceivedBootNanoseconds: monotonic, DeadlineBootNanoseconds: monotonic + int64(900*time.Millisecond), ValidUntil: arrival.Add(900 * time.Millisecond), Transitions: 2, UpTransitions: 1, Received: 2, Transmitted: 2}}}
	if err := bfd.ValidateSnapshot(cfg, snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestBFDUpdatesDoNotRunKernelReconciliationAndFenceOldProducers(t *testing.T) {
	doc := bfdDocument()
	r, n, _ := fixture(t, doc)
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observedBFD(t, doc.Services[0].BFDConfig(), clock)
	u := update(t, r, "service.edge", 1, snapshot)
	calls := n.calls
	n.fail = errors.New("kernel operation must not be called by BFD observations")
	if err := observeAndProject(context.Background(), r, u); err != nil {
		t.Fatal(err)
	}
	if n.calls != calls || n.fail == nil {
		t.Fatal("BFD liveness update performed a full kernel reconciliation")
	}
	status := r.State()
	if len(status.Instances) != 1 || status.Instances[0].BFD == nil || len(status.Instances[0].BFD.HealthyPeers) != 1 {
		t.Fatalf("missing calculated BFD health: %+v", status)
	}
	stale := u
	stale.ProducerEpoch = strings.Repeat("b", 32)
	if !errors.Is(observeAndProject(context.Background(), r, stale), ErrStale) {
		t.Fatal("different producer within one generation accepted")
	}
	snapshot.Peers[0].State = bfd.StateDown
	snapshot.Peers[0].DesiredMinTxMicroseconds = 1000000
	snapshot.Peers[0].TransmitIntervalMicroseconds = 1000000
	snapshot.Peers[0].DeadlineBootNanoseconds = 0
	snapshot.Peers[0].ValidUntil = time.Time{}
	snapshot.Peers[0].Transitions = 3
	snapshot.Peers[0].DownTransitions = 1
	down := update(t, r, "service.edge", 2, snapshot)
	if err := observeAndProject(context.Background(), r, down); err != nil {
		t.Fatal(err)
	}
	u.Sequence = 3
	if err := observeAndProject(context.Background(), r, u); err == nil {
		t.Fatal("higher IPC sequence hid a rewound BFD Down counter")
	}
	n.fail = nil
}

func TestBFDHealthUsesKernelDeadlineRatherThanDisplayTime(t *testing.T) {
	doc := bfdDocument()
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	clock.BoottimeNanoseconds -= int64(3 * time.Second)
	clock.WallTime = time.Date(2080, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := doc.Services[0].BFDConfig()
	snapshot := observedBFD(t, cfg, clock)
	instance, err := doc.Services[0].Instance("/run/routerd", "/var/lib/routerd")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snapshot)
	health := bfdHealth(instance, serviceconfig.Update{Snapshot: raw})
	if len(health.HealthyPeers) != 0 || len(health.UnhealthyPeers) != 1 || health.ClockError {
		t.Fatalf("wallclock extended expired BFD health: %+v", health)
	}
}

func TestBFDSnapshotTransitionRejectsFutureReceiveAndReactivation(t *testing.T) {
	cfg := bfdDocument().Services[0].BFDConfig()
	clock := bfd.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Unix(1800000000, 0).UTC()}
	snapshot := observedBFD(t, cfg, clock)
	encode := func(s bfd.Snapshot) serviceconfig.Update {
		b, _ := json.Marshal(s)
		return serviceconfig.Update{Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Snapshot: b}
	}
	initial := encode(snapshot)
	if err := validateBFDTransition(serviceconfig.Update{}, initial, clock); err != nil {
		t.Fatal(err)
	}
	snapshot.Peers[0].LastReceivedBootNanoseconds = clock.BoottimeNanoseconds + 1
	if err := validateBFDTransition(initial, encode(snapshot), clock); err == nil {
		t.Fatal("future receive anchor accepted")
	}
	snapshot = observedBFD(t, cfg, clock)
	snapshot.Active = false
	snapshot.Peers[0].State = bfd.StateAdminDown
	snapshot.Peers[0].DownTransitions = 1
	snapshot.Peers[0].Transitions = 3
	snapshot.Peers[0].DesiredMinTxMicroseconds = 1000000
	snapshot.Peers[0].TransmitIntervalMicroseconds = 1000000
	snapshot.Peers[0].DeadlineBootNanoseconds = 0
	snapshot.Peers[0].ValidUntil = time.Time{}
	if err := bfd.ValidateSnapshot(cfg, snapshot); err != nil {
		t.Fatal(err)
	}
	final := encode(snapshot)
	if err := validateBFDTransition(initial, final, clock); err != nil {
		t.Fatal(err)
	}
	if err := validateBFDTransition(final, initial, clock); err == nil {
		t.Fatal("a finalized producer became active again")
	}
	restarted := initial
	restarted.Generation++
	restarted.ProducerEpoch = strings.Repeat("b", 32)
	if err := validateBFDTransition(final, restarted, clock); err != nil {
		t.Fatal("a fenced new generation could not reacquire its peers", err)
	}
}

func TestBFDReceiveCountFencesRemoteTimingAndDeadline(t *testing.T) {
	cfg := bfdDocument().Services[0].BFDConfig()
	clock := bfd.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Unix(1800000000, 0).UTC()}
	old := observedBFD(t, cfg, clock)
	encode := func(s bfd.Snapshot) serviceconfig.Update {
		b, _ := json.Marshal(s)
		return serviceconfig.Update{Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Snapshot: b}
	}
	previous := encode(old)
	for _, tc := range []struct {
		name string
		edit func(*bfd.PeerState)
	}{
		{"longer-detection", func(p *bfd.PeerState) {
			p.RemoteMinTxMicroseconds = 3000000
			p.DetectionTimeMicroseconds = 9000000
			p.DeadlineBootNanoseconds = p.LastReceivedBootNanoseconds + int64(9*time.Second)
			p.ValidUntil = p.LastReceivedAt.Add(9 * time.Second)
		}},
		{"remote-discriminator", func(p *bfd.PeerState) { p.RemoteDiscriminator++ }},
		{"remote-diagnostic", func(p *bfd.PeerState) { p.RemoteDiagnostic++ }},
		{"remote-tx-suppression", func(p *bfd.PeerState) { p.RemoteMinRxMicroseconds = 0 }},
		{"remote-echo", func(p *bfd.PeerState) { p.RemoteMinEchoRxMicroseconds = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := observedBFD(t, cfg, clock)
			tc.edit(&next.Peers[0])
			if err := bfd.ValidateSnapshot(cfg, next); err != nil {
				t.Fatal("fixture must pass isolated structural validation", err)
			}
			if err := validateBFDTransition(previous, encode(next), clock); err == nil {
				t.Fatal("remote receive evidence changed without another packet")
			}
		})
	}
	// A real subsequent packet may negotiate a longer interval. The transport
	// sequence alone is insufficient; the peer's receive counter must advance.
	next := observedBFD(t, cfg, clock)
	next.Peers[0].Received++
	next.Peers[0].RemoteMinTxMicroseconds = 3000000
	next.Peers[0].DetectionTimeMicroseconds = 9000000
	next.Peers[0].DeadlineBootNanoseconds = next.Peers[0].LastReceivedBootNanoseconds + int64(9*time.Second)
	next.Peers[0].ValidUntil = next.Peers[0].LastReceivedAt.Add(9 * time.Second)
	if err := validateBFDTransition(previous, encode(next), clock); err != nil {
		t.Fatal("fresh receive cannot renegotiate", err)
	}
}
