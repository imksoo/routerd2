package runtimeplan

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestVRRPWallClockAloneCannotAuthorizeOwnership(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, n, _ := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := vrrp.Snapshot{Version: c.Version, Resource: c.Resource, Interface: c.Interface, VIPInterface: c.VIPInterface, Family: c.Family, VirtualRouterID: c.VirtualRouterID, Priority: c.Priority, AcceptMode: true, Addresses: c.Addresses, VirtualMAC: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), Epoch: strings.Repeat("a", 32), Transition: 1, Active: true, State: vrrp.StateActive, ValidUntil: time.Now().Add(3 * time.Second)}
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 1, s)); err == nil {
		t.Fatal("UTC-only VRRP grant acquired VIP/VMAC ownership without boot-scoped evidence")
	}
	if len(n.applied.Links) != 0 {
		t.Fatal("rejected observation changed the network")
	}
}

func bootVRRPSnapshot(t *testing.T, c vrrp.Config) vrrp.Snapshot {
	t.Helper()
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(clock.BoottimeNanoseconds)
	return vrrp.Snapshot{Version: c.Version, Resource: c.Resource, Interface: c.Interface, VIPInterface: c.VIPInterface, VRF: c.VRF, Family: c.Family, SourceAddress: c.SourceAddress, VirtualRouterID: c.VirtualRouterID, Priority: c.Priority, AcceptMode: c.AcceptMode || c.AddressOwner, AddressOwner: c.AddressOwner, Addresses: c.Addresses, VirtualMAC: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), Epoch: strings.Repeat("a", 32), Transition: 1, Active: true, State: vrrp.StateActive, ValidUntil: time.Now().Add(time.Hour), BootID: clock.BootID, ObservedNanos: now, ExpiresNanos: now + uint64(3*time.Duration(c.AdvertIntervalCentiseconds)*10*time.Millisecond)}
}

func TestVRRPBootDeadlineReachesKernelWithoutUTCConversion(t *testing.T) {
	for _, offset := range []time.Duration{-48 * time.Hour, 48 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}
			doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
			r, n, _ := fixture(t, doc)
			c = doc.Services[0].VRRPConfig()
			s := bootVRRPSnapshot(t, c)
			s.ValidUntil = time.Now().Add(offset)
			if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 1, s)); err != nil {
				t.Fatal("UTC metadata controlled boot ownership", err)
			}
			found := false
			for _, iface := range n.applied.Interfaces {
				if iface.Name != c.VIPInterface {
					continue
				}
				found = true
				opts := iface.AddressOptions[c.Addresses[0]]
				if opts.BootLifetime.BootID != s.BootID || opts.BootLifetime.ValidNanos != s.ExpiresNanos || opts.BootLifetime.PreferredNanos != s.ExpiresNanos || !opts.ValidUntil.IsZero() || !opts.PreferredUntil.IsZero() {
					t.Fatal("network projection replaced original BOOTTIME authority", opts)
				}
			}
			if !found {
				t.Fatal("VIP not projected")
			}
			clock, err := boottime.Read()
			if err != nil {
				t.Fatal(err)
			}
			clock.BoottimeNanoseconds = int64(s.ExpiresNanos)
			p, err := projectWithFacts(doc, protocolsFor(t, doc, r.dir, r.stateDir), r.records, time.Now().Add(-48*time.Hour), projectionFacts{bfdClock: clock})
			if err != nil || len(p.network.Links) != 0 {
				t.Fatal("expired boot lease survived UTC rollback", p.network.Links, err)
			}
		})
	}
}

func TestVRRPColdControlRejectsRetiredSupervisorGeneration(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, n, manager := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := bootVRRPSnapshot(t, c)
	manager.vrrpGeneration = 2
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 1, s)); err == nil || len(r.records) != 0 || len(n.applied.Links) != 0 {
		t.Fatal("first message after control restart revived retired VRRP generation", err)
	}
}

func TestVRRPFinalWithdrawalAndClockCannotBeReplayedIntoOwnership(t *testing.T) {
	c := vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}}
	r, _, _ := fixture(t, doc)
	c = doc.Services[0].VRRPConfig()
	s := bootVRRPSnapshot(t, c)
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 1, s)); err != nil {
		t.Fatal(err)
	}
	old := r.records["service.ha"]
	for _, mutate := range []func(*vrrp.Snapshot){
		func(x *vrrp.Snapshot) { x.BootID = "7bbe264e-57c0-4ee6-aa19-67c3a1d244fb" },
		func(x *vrrp.Snapshot) { x.ObservedNanos--; x.ExpiresNanos-- },
		func(x *vrrp.Snapshot) { x.ExpiresNanos++ },
		func(x *vrrp.Snapshot) { x.Epoch = strings.Repeat("b", 32) },
	} {
		bad := s
		mutate(&bad)
		if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 2, bad)); err == nil {
			t.Fatal("invalid VRRP transition accepted", bad)
		}
		if string(r.records["service.ha"].Snapshot) != string(old.Snapshot) {
			t.Fatal("rejection changed remembered observation")
		}
	}
	stopped := s
	stopped.Active, stopped.State, stopped.ValidUntil, stopped.ExpiresNanos = false, vrrp.StateInitialize, time.Time{}, 0
	stopped.Transition++
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 2, stopped)); err != nil {
		t.Fatal(err)
	}
	newer := bootVRRPSnapshot(t, c)
	newer.Transition = stopped.Transition + 1
	if _, err := applyAndDrain(t, r, update(t, r, "service.ha", 3, newer)); err == nil {
		t.Fatal("same worker revived after terminal withdrawal")
	}
	var stored vrrp.Snapshot
	if err := json.Unmarshal(r.records["service.ha"].Snapshot, &stored); err != nil || stored.Active {
		t.Fatal("replayed current ownership restored", err)
	}
}
