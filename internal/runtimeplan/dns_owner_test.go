package runtimeplan

import (
	"encoding/json"
	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"strings"
	"testing"
	"time"
)

func TestDNSUpstreamOwnerPreservesBothOriginalDeadlines(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	runtime := dnsconfig.RuntimeConfig{Resource: "dns", Spec: api.DNSResolverSpec{Sources: []api.DNSResolverSourceSpec{
		{Name: "owned", Kind: "upstream", UpstreamFrom: []api.StatusValueSourceSpec{{Resource: "DHCPv6Information/wan"}}},
		{Name: "independent", Kind: "upstream", UpstreamFrom: []api.StatusValueSourceSpec{{Resource: "DHCPv6Information/wan"}}},
	}}}
	plan, err := prepareDNS(runtime, []serviceconfig.DNSUpstreamOwner{{Source: "owned", Provider: "wan", VRRP: "ha"}})
	if err != nil {
		t.Fatal(err)
	}
	r := New(t.TempDir(), t.TempDir(), &memoryNetwork{}, testSupervisorInstance)
	r.configured = true
	r.bindings = map[string]binding{"service.dns": {instance: serviceconfig.Instance{Role: "dns"}}}
	r.config.protocols.dns = map[string]dnsPlan{"service.dns": plan}
	original := lifetime.After(clock, 120)
	info := dhcp6config.Snapshot{Resource: "wan", Interface: "wan0", State: "information", BootID: clock.BootID, InfoReceivedNanos: uint64(clock.BoottimeNanoseconds), InformationRefreshAt: original, DNSServers: []string{"fe80::53"}}
	raw, _ := json.Marshal(info)
	r.records["service.wan"] = serviceconfig.Update{Role: "dhcp6", Snapshot: raw}
	election := vrrp.Snapshot{State: vrrp.StateActive, Active: true, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(30*time.Second), Epoch: strings.Repeat("a", 32)}
	check := func(wantOwned bool, wantUntil lifetime.Deadline) {
		t.Helper()
		raw, _ := json.Marshal(election)
		r.records["service.ha"] = serviceconfig.Update{Role: "vrrp", Snapshot: raw}
		got, err := r.DNSInput("service.dns")
		if err != nil {
			t.Fatal(err)
		}
		owned, independent := false, false
		for _, ep := range got.Upstreams {
			if ep.Address.String() != "fe80::53%wan0" {
				t.Fatal("link scope lost", ep)
			}
			if ep.Source == "owned" {
				owned = true
				if ep.ValidUntil != wantUntil {
					t.Fatal("owner/protocol expiry extended", ep, wantUntil)
				}
			}
			if ep.Source == "independent" {
				independent = true
				if ep.ValidUntil != original {
					t.Fatal("unrelated source shortened", ep)
				}
			}
		}
		if owned != wantOwned || !independent {
			t.Fatal("upstream withdrawal crossed source boundary", got.Upstreams)
		}
	}
	check(true, lifetime.Deadline{BootID: clock.BootID, Nanos: election.ExpiresNanos})
	election.ExpiresNanos = original.Nanos + uint64(time.Second)
	check(true, original)
	election.State = vrrp.StateBackup
	check(false, lifetime.Deadline{})
	election.State = vrrp.StateActive
	election.ExpiresNanos = uint64(clock.BoottimeNanoseconds) - 1
	check(false, lifetime.Deadline{})
	election.BootID = "retired-boot"
	election.ExpiresNanos = original.Nanos
	check(false, lifetime.Deadline{})
}
