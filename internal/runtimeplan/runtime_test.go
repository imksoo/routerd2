package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	old4 "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

const testSupervisorInstance = "11111111111111111111111111111111"

type memoryNetwork struct {
	mu      sync.Mutex
	applied network.Desired
	fail    error
	calls   int
}

func (n *memoryNetwork) Apply(_ context.Context, _ uint64, d network.Desired) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.fail != nil {
		e := n.fail
		n.fail = nil
		return e
	}
	b, _ := json.Marshal(d)
	n.applied = network.Desired{}
	return json.Unmarshal(b, &n.applied)
}

func (n *memoryNetwork) SourceAddress(_ context.Context, name, requested string) (network.SourceObservation, error) {
	return network.SourceObservation{Interface: name, RequestedAddress: requested}, nil
}

type memoryManager struct {
	modules         []supervisor.DynamicModule
	before          func([]supervisor.DynamicModule)
	bgpGeneration   uint64
	dhcp4Generation uint64
	vrrpGeneration  uint64
}

func (m *memoryManager) Reconcile(_ context.Context, v []supervisor.DynamicModule) error {
	if m.before != nil {
		m.before(v)
	}
	m.modules = append([]supervisor.DynamicModule(nil), v...)
	return nil
}
func (m *memoryManager) Status(_ context.Context) (supervisor.Status, error) {
	s := supervisor.Status{Lifecycle: supervisor.Running, Ready: true, InstanceID: testSupervisorInstance}
	for _, v := range m.modules {
		generation := uint64(0)
		if v.Role == "aftr" {
			generation = 1
		}
		if v.Role == "dhcp4" {
			generation = max(uint64(1), m.dhcp4Generation)
		}
		if v.Role == "bgp" {
			generation = max(uint64(1), m.bgpGeneration)
		}
		if v.Role == "vrrp" {
			generation = max(uint64(1), m.vrrpGeneration)
		}
		s.Modules = append(s.Modules, supervisor.ModuleStatus{Name: v.Name, Role: v.Role, Ready: true, State: "running", ConfigDigest: v.ConfigDigest, Generation: generation})
	}
	return s, nil
}
func fixture(t *testing.T, doc config.Native) (*Runtime, *memoryNetwork, *memoryManager) {
	t.Helper()
	n := &memoryNetwork{}
	m := &memoryManager{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = m
	if e := r.ApplyConfig(context.Background(), 1, config.FromNative(doc)); e != nil {
		t.Fatal(e)
	}
	return r, n, m
}

// A component test can publish input and explicitly run its application cycle.
// Observe alone intentionally has no kernel-application guarantee.
func observeAndProject(ctx context.Context, r *Runtime, u serviceconfig.Update) error {
	if err := r.Observe(ctx, u); err != nil {
		return err
	}
	for range 2 {
		if err := r.reconcileDynamic(ctx); err != nil {
			return err
		}
	}
	return nil
}

func update(t *testing.T, r *Runtime, name string, seq uint64, snapshot any) serviceconfig.Update {
	t.Helper()
	b := r.bindings[name]
	raw, e := json.Marshal(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	return serviceconfig.Update{SupervisorInstance: r.supervisorInstance, Name: name, Role: b.instance.Role, ConfigDigest: b.digest, Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: seq, Snapshot: raw}
}
func wanDoc() config.Native {
	return config.Native{Services: []serviceconfig.Service{{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}}}}
}
func lease4(expires time.Time) dhcp4.Snapshot {
	clock, err := boottime.Read()
	if err != nil {
		panic(err)
	}
	lifetime := time.Duration(math.Ceil(expires.Sub(clock.WallTime).Seconds())) * time.Second
	if lifetime <= 0 {
		clock.BoottimeNanoseconds -= int64(2 * time.Second)
		clock.WallTime = clock.WallTime.Add(-2 * time.Second)
		lifetime = time.Second
	}
	return bootLease4(clock, lifetime)
}

func TestLeaseObservationSurvivesApplyFailureAndExpiresWithoutRenewal(t *testing.T) {
	ctx := context.Background()
	r, n, _ := fixture(t, wanDoc())
	expires := time.Now().Add(time.Hour)
	u := update(t, r, "service.wan", 1, lease4(expires))
	if e := observeAndProject(ctx, r, u); e != nil {
		t.Fatal(e)
	}
	if len(n.applied.Routes) != 1 || n.applied.Routes[0].Gateway != "192.0.2.1" || n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].BootLifetime.ValidNanos == 0 {
		t.Fatalf("lease was not projected: %+v", n.applied)
	}
	calls := n.calls
	if e := r.Observe(ctx, u); e != nil || n.calls != calls {
		t.Fatal("exact duplicate was not idempotent", e)
	}
	stale := u
	stale.ProducerEpoch = strings.Repeat("b", 32)
	if !errors.Is(r.Observe(ctx, stale), ErrStale) {
		t.Fatal("stale producer was accepted")
	}
	bad := lease4(expires)
	bad.Interface = "other0"
	if r.Observe(ctx, update(t, r, "service.wan", 2, bad)) == nil {
		t.Fatal("identity spoof was accepted")
	}
	renewed := lease4(expires.Add(time.Hour))
	n.fail = errors.New("injected kernel apply failure")
	if err := r.Observe(ctx, update(t, r, "service.wan", 2, renewed)); err != nil {
		t.Fatal("observation acceptance unexpectedly performs kernel IO", err)
	}
	if err := r.reconcileDynamic(ctx); err == nil {
		t.Fatal("coordinator hid kernel application failure")
	}
	if r.records["service.wan"].Sequence != 2 || r.CheckReady(ctx) == nil {
		t.Fatal("failed application lost accepted input or reported ready")
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	if n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].BootLifetime.ValidNanos != renewed.LeaseClock.ExpiresNanos {
		t.Fatal("retry did not apply the accepted original lease deadline")
	}
	expired := lease4(time.Now().Add(time.Hour))
	expired.Active = false
	expired.LeaseClock = dhcp4.LeaseClock{}
	expired.Snapshot = old4.Snapshot{Resource: "wan", Interface: "wan0", State: old4.StateExpired, UpdatedAt: time.Now().UTC()}
	if e := observeAndProject(ctx, r, update(t, r, "service.wan", 3, expired)); e != nil {
		t.Fatal(e)
	}
	if len(n.applied.Routes) != 0 || len(n.applied.Interfaces[0].Addresses) != 0 {
		t.Fatal("expired lease retained kernel state")
	}
}

func TestConcurrentRenewalSurvivesConfigurationCutover(t *testing.T) {
	doc := wanDoc()
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "lan", RA: &ra.Config{Interface: "lan0", RouterLifetimeSeconds: 1800}})
	r, n, m := fixture(t, doc)
	expires := time.Now().Add(time.Hour)
	if e := r.Observe(context.Background(), update(t, r, "service.wan", 1, lease4(expires))); e != nil {
		t.Fatal(e)
	}
	next := wanDoc() // removal of RA forces quiescence, while DHCP is unchanged.
	fired := false
	var renewal dhcp4.Snapshot
	m.before = func(modules []supervisor.DynamicModule) {
		if fired {
			return
		}
		fired = true
		renewal = lease4(expires.Add(time.Hour))
		if e := r.Observe(context.Background(), update(t, r, "service.wan", 2, renewal)); e != nil {
			t.Fatal(e)
		}
	}
	if e := r.ApplyConfig(context.Background(), 2, config.FromNative(next)); e != nil {
		t.Fatal(e)
	}
	if !fired || n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].BootLifetime.ValidNanos != renewal.LeaseClock.ExpiresNanos || r.records["service.wan"].Sequence != 2 {
		t.Fatal("cutover restored a lease older than its acknowledged renewal")
	}
}

func TestPDProjectionKeepsDeprecatedAddressesAndStableRAIdentity(t *testing.T) {
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0)}
	now := clock.WallTime
	doc := config.Native{Services: []serviceconfig.Service{{Name: "pd", DHCP6: &serviceconfig.DHCP6{RequestPrefix: true, Interface: "wan0", IAID: 7, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", SubnetID: 3, Advertise: true}}}}}}
	snapshot := dhcp6config.Snapshot{BootID: clock.BootID, Resource: "pd", Interface: "wan0", IAID: 7, State: "bound", Prefixes: []dhcp6config.Prefix{{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clock, 100), ValidUntil: lifetime.After(clock, 200)}, {Prefix: "2001:db8:200::/56", PreferredUntil: lifetime.After(clock, 0), ValidUntil: lifetime.After(clock, 80)}}}
	protocols := protocolsFor(t, doc, "/run/routerd", "/var/lib/routerd")
	raw, _ := json.Marshal(snapshot)
	records := map[string]serviceconfig.Update{"service.pd": {Snapshot: raw}}
	first, e := projectWithFacts(doc, protocols, records, now, projectionFacts{bfdClock: clock})
	if e != nil {
		t.Fatal(e)
	}
	if len(first.liveRA["service.pd-lan-ra"].Prefixes) != 2 {
		t.Fatal("renumbering prefixes lost")
	}
	oldAdvertisement := first.liveRA["service.pd-lan-ra"].Prefixes[1]
	if oldAdvertisement.ValidLifetimeSeconds == 0 || oldAdvertisement.ValidUntil != snapshot.Prefixes[1].ValidUntil || oldAdvertisement.PreferredUntil.Live(clock) {
		t.Fatal("retained prefix must remain valid but deprecated on the LAN", oldAdvertisement)
	}
	var lan network.Interface
	for _, v := range first.network.Interfaces {
		if v.Name == "lan0" {
			lan = v
		}
	}
	if !reflect.DeepEqual(lan.Addresses, []string{"2001:db8:100:3::1/64", "2001:db8:200:3::1/64"}) {
		t.Fatalf("LAN addresses %+v", lan)
	}
	if lan.AddressOptions["2001:db8:200:3::1/64"].BootLifetime.PreferredNanos > uint64(clock.BoottimeNanoseconds) {
		t.Fatal("old prefix is still preferred")
	}
	digest := first.instances[1].digest
	snapshot.Prefixes[0].ValidUntil.Nanos += uint64(time.Minute)
	snapshot.Prefixes[0].PreferredUntil.Nanos += uint64(time.Minute)
	raw, _ = json.Marshal(snapshot)
	records["service.pd"] = serviceconfig.Update{Snapshot: raw}
	renewed, e := projectWithFacts(doc, protocols, records, now, projectionFacts{bfdClock: clock})
	if e != nil {
		t.Fatal(e)
	}
	next := renewed.instances[1].digest
	if digest != next {
		t.Fatal("PD renewal changes RA immutable identity and would restart it")
	}
	clock.BoottimeNanoseconds += int64(time.Hour)
	expired, e := projectWithFacts(doc, protocols, records, now, projectionFacts{bfdClock: clock})
	if e != nil {
		t.Fatal(e)
	}
	if len(expired.liveRA) != 0 || len(expired.instances) != 1 {
		t.Fatal("expired PD kept an advertisement worker")
	}
}

func TestVRRPVirtualOwnershipUsesNoDADAndRejectsPolicySpoof(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv6", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"fe80::42/64", "2001:db8:42::1/64"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, n, _ := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := bootVRRPSnapshot(t, c)
	if _, e := applyAndDrain(t, r, update(t, r, "service.ha", 1, s)); e != nil {
		t.Fatal(e)
	}
	if len(n.applied.Links) != 1 || n.applied.Links[0].IPv6AddrGenMode != "none" {
		t.Fatal("VMAC interface not created with explicit address generation")
	}
	var vip network.Interface
	for _, v := range n.applied.Interfaces {
		if v.Name == c.VIPInterface {
			vip = v
		}
	}
	if !vip.AddressOptions[c.Addresses[0]].NoDAD {
		t.Fatal("HA virtual link-local address would wait for DAD")
	}
	bad := s
	bad.AcceptMode = false
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 2, bad)); err == nil {
		t.Fatal("producer changed configured acceptance policy")
	}

}

func TestDHCPLeaseCannotReplaceStaticAddressOwnership(t *testing.T) {
	doc := wanDoc()
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.10/24"}}}
	r, n, _ := fixture(t, doc)
	before := n.applied
	if err := r.Observe(context.Background(), update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err == nil || !strings.Contains(err.Error(), "address owner") {
		t.Fatalf("overlapping lease did not reject conflicting ownership: %v", err)
	}
	if !reflect.DeepEqual(before, n.applied) || len(r.records) != 0 {
		t.Fatal("rejected dynamic lease changed committed ownership")
	}
}
