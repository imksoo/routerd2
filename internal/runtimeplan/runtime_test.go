package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	old4 "github.com/imksoo/routerd/pkg/dhcpv4client"
	old6 "github.com/imksoo/routerd/pkg/pdclient"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

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

type memoryManager struct {
	modules []supervisor.DynamicModule
	before  func([]supervisor.DynamicModule)
}

func (m *memoryManager) Reconcile(_ context.Context, v []supervisor.DynamicModule) error {
	if m.before != nil {
		m.before(v)
	}
	m.modules = append([]supervisor.DynamicModule(nil), v...)
	return nil
}
func (m *memoryManager) Status(_ context.Context) (supervisor.Status, error) {
	s := supervisor.Status{Ready: true}
	for _, v := range m.modules {
		s.Modules = append(s.Modules, supervisor.ModuleStatus{Name: v.Name, Role: v.Role, Ready: true, State: "running", ConfigDigest: v.ConfigDigest})
	}
	return s, nil
}
func fixture(t *testing.T, doc config.Native) (*Runtime, *memoryNetwork, *memoryManager) {
	t.Helper()
	n := &memoryNetwork{}
	m := &memoryManager{}
	r := New(t.TempDir(), t.TempDir(), n)
	r.manager = m
	if e := r.ApplyConfig(context.Background(), 1, config.FromNative(doc)); e != nil {
		t.Fatal(e)
	}
	return r, n, m
}
func update(t *testing.T, r *Runtime, name string, seq uint64, snapshot any) serviceconfig.Update {
	t.Helper()
	b := r.bindings[name]
	raw, e := json.Marshal(snapshot)
	if e != nil {
		t.Fatal(e)
	}
	return serviceconfig.Update{Name: name, Role: b.instance.Role, ConfigDigest: b.digest, Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: seq, Snapshot: raw}
}
func wanDoc() config.Native {
	return config.Native{Services: []serviceconfig.Service{{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}}}}
}
func lease4(expires time.Time) dhcp4.Snapshot {
	return dhcp4.Snapshot{Active: true, Snapshot: old4.Snapshot{Resource: "wan", Interface: "wan0", CurrentAddress: "192.0.2.10", PrefixLength: 24, DefaultGateway: "192.0.2.1", ExpiresAt: expires}}
}

func TestLeaseApplyFencingFailureRollbackAndExpiry(t *testing.T) {
	ctx := context.Background()
	r, n, _ := fixture(t, wanDoc())
	expires := time.Now().Add(time.Hour)
	u := update(t, r, "service.wan", 1, lease4(expires))
	if e := r.Accept(ctx, u); e != nil {
		t.Fatal(e)
	}
	if len(n.applied.Routes) != 1 || n.applied.Routes[0].Gateway != "192.0.2.1" || !n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].ValidUntil.Equal(expires) {
		t.Fatalf("lease was not projected: %+v", n.applied)
	}
	calls := n.calls
	if e := r.Accept(ctx, u); e != nil || n.calls != calls {
		t.Fatal("exact duplicate was not idempotent", e)
	}
	stale := u
	stale.ProducerEpoch = strings.Repeat("b", 32)
	if !errors.Is(r.Accept(ctx, stale), ErrStale) {
		t.Fatal("stale producer was accepted")
	}
	bad := lease4(expires)
	bad.Interface = "other0"
	if r.Accept(ctx, update(t, r, "service.wan", 2, bad)) == nil {
		t.Fatal("identity spoof was accepted")
	}
	renewed := lease4(expires.Add(time.Hour))
	n.fail = errors.New("injected kernel apply failure")
	if r.Accept(ctx, update(t, r, "service.wan", 2, renewed)) == nil {
		t.Fatal("kernel failure was acknowledged")
	}
	if r.records["service.wan"].Sequence != 1 || !n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].ValidUntil.Equal(expires) {
		t.Fatal("previous accepted intent was not restored")
	}
	expired := lease4(time.Now().Add(-time.Second))
	if e := r.Accept(ctx, update(t, r, "service.wan", 3, expired)); e != nil {
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
	if e := r.Accept(context.Background(), update(t, r, "service.wan", 1, lease4(expires))); e != nil {
		t.Fatal(e)
	}
	next := wanDoc() // removal of RA forces quiescence, while DHCP is unchanged.
	fired := false
	m.before = func(modules []supervisor.DynamicModule) {
		if fired {
			return
		}
		fired = true
		if e := r.Accept(context.Background(), update(t, r, "service.wan", 2, lease4(expires.Add(time.Hour)))); e != nil {
			t.Fatal(e)
		}
	}
	if e := r.ApplyConfig(context.Background(), 2, config.FromNative(next)); e != nil {
		t.Fatal(e)
	}
	if !fired || !n.applied.Interfaces[0].AddressOptions["192.0.2.10/24"].ValidUntil.Equal(expires.Add(time.Hour)) {
		t.Fatal("cutover restored a lease older than its acknowledged renewal")
	}
}

func TestPDProjectionKeepsDeprecatedAddressesAndStableRAIdentity(t *testing.T) {
	now := time.Now()
	doc := config.Native{Services: []serviceconfig.Service{{Name: "pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 7, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", SubnetID: 3, Advertise: true}}}}}}
	snapshot := dhcp6pd.Snapshot{Active: true, Snapshot: old6.Snapshot{Resource: "pd", Interface: "wan0", IAID: 7, CurrentPrefix: "2001:db8:100::/56", AcquiredAt: now, Preferred: 100, Valid: 200, ExpiresAt: now.Add(200 * time.Second), PreviousPrefixes: []old6.PreviousPrefixSnapshot{{Prefix: "2001:db8:200::/56", ExpiresAt: now.Add(80 * time.Second)}}}}
	raw, _ := json.Marshal(snapshot)
	records := map[string]serviceconfig.Update{"service.pd": {Snapshot: raw}}
	first, e := project(doc, "/run/routerd", "/var/lib/routerd", records, now)
	if e != nil {
		t.Fatal(e)
	}
	if len(first.liveRA["service.pd-lan-ra"].Prefixes) != 2 {
		t.Fatal("renumbering prefixes lost")
	}
	oldAdvertisement := first.liveRA["service.pd-lan-ra"].Prefixes[1]
	if oldAdvertisement.ValidLifetimeSeconds == 0 || !oldAdvertisement.ValidUntil.Equal(snapshot.PreviousPrefixes[0].ExpiresAt) || oldAdvertisement.PreferredLifetimeSeconds != 0 {
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
	if !lan.AddressOptions["2001:db8:200:3::1/64"].PreferredUntil.Before(now) {
		t.Fatal("old prefix is still preferred")
	}
	_, digest, _ := encodeInstance(first.instances[1])
	snapshot.ExpiresAt = snapshot.ExpiresAt.Add(time.Minute)
	snapshot.AcquiredAt = snapshot.AcquiredAt.Add(time.Minute)
	raw, _ = json.Marshal(snapshot)
	records["service.pd"] = serviceconfig.Update{Snapshot: raw}
	renewed, e := project(doc, "/run/routerd", "/var/lib/routerd", records, now)
	if e != nil {
		t.Fatal(e)
	}
	_, next, _ := encodeInstance(renewed.instances[1])
	if digest != next {
		t.Fatal("PD renewal changes RA immutable identity and would restart it")
	}
	expired, e := project(doc, "/run/routerd", "/var/lib/routerd", records, now.Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if len(expired.liveRA) != 0 || len(expired.instances) != 1 {
		t.Fatal("expired PD kept an advertisement worker")
	}
}

func TestVRRPOwnershipExpiresOnMonotonicReceiptAndRejectsPolicySpoof(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv6", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"fe80::42/64", "2001:db8:42::1/64"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, n, _ := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := vrrp.Snapshot{Resource: c.Resource, Interface: c.Interface, VIPInterface: c.VIPInterface, Family: c.Family, VirtualRouterID: c.VirtualRouterID, Priority: c.Priority, AcceptMode: true, Addresses: c.Addresses, VirtualMAC: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), Active: true, State: vrrp.StateActive, ValidUntil: time.Now().Add(3 * time.Second)}
	if e := r.Accept(context.Background(), update(t, r, "service.ha", 1, s)); e != nil {
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
	if r.Accept(context.Background(), update(t, r, "service.ha", 2, bad)) == nil {
		t.Fatal("producer changed configured acceptance policy")
	}
	// Simulate monotonic expiration while the absolute wall deadline is future.
	r.mu.Lock()
	r.leases["service.ha"] = time.Now().Add(-time.Second)
	r.mu.Unlock()
	if e := r.reconcileDynamic(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("expired ownership kept the VMAC interface")
	}
}

func TestDHCPLeaseCannotReplaceStaticAddressOwnership(t *testing.T) {
	doc := wanDoc()
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.10/24"}}}
	r, n, _ := fixture(t, doc)
	before := n.applied
	if err := r.Accept(context.Background(), update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))); err == nil || !strings.Contains(err.Error(), "address owner") {
		t.Fatalf("overlapping lease did not reject conflicting ownership: %v", err)
	}
	if !reflect.DeepEqual(before, n.applied) || len(r.records) != 0 {
		t.Fatal("rejected dynamic lease changed committed ownership")
	}
}
