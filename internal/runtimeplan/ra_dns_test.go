package runtimeplan

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestPDLearnedDNSDeadlineAndStableRAIdentity(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: now}
	doc := config.Native{Services: []serviceconfig.Service{{Name: "pd", DHCP6: &serviceconfig.DHCP6{RequestPrefix: true, Interface: "wan0", IAID: 7, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", SubnetID: 3, Advertise: true, UseLearnedDNS: true, RDNSS: []string{"2001:db8::1"}, DNSSL: []string{"static.example"}}}}}}}
	protocols := protocolsFor(t, doc, "/run/routerd", "/var/lib/routerd")
	snapshot := dhcp6config.Snapshot{BootID: clock.BootID, Resource: "pd", Interface: "wan0", IAID: 7, State: "bound", Prefixes: []dhcp6config.Prefix{{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clock, 400), ValidUntil: lifetime.After(clock, 600)}}, InformationRefreshSeconds: 600, InformationRefreshAt: lifetime.After(clock, 180), InfoReceivedNanos: uint64(clock.BoottimeNanoseconds), DNSServers: []string{"2001:db8::1", "2001:db8::53", "fe80::53"}, DomainSearch: []string{"static.example", "learned.example"}}
	projectAt := func(value dhcp6config.Snapshot, at time.Time) (ra.Config, string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		p, err := projectWithFacts(doc, protocols, map[string]serviceconfig.Update{"service.pd": {Role: "dhcp6", Snapshot: raw}}, at, projectionFacts{bfdClock: boottime.Clock{BootID: clock.BootID, BoottimeNanoseconds: clock.BoottimeNanoseconds + int64(at.Sub(now)), WallTime: at}})
		if err != nil {
			t.Fatal(err)
		}
		live, found := p.liveRA["service.pd-lan-ra"]
		if !found {
			t.Fatal("expected live LAN advertisement")
		}
		for _, bound := range p.instances {
			instance := bound.instance
			if instance.Name == "service.pd-lan-ra" {
				var template ra.Config
				if err := json.Unmarshal(instance.Config, &template); err != nil {
					t.Fatal(err)
				}
				if len(template.RDNSS)+len(template.DNSSL)+len(template.LearnedRDNSS)+len(template.LearnedDNSSL) != 0 {
					t.Fatal("live DNS leaked into immutable worker template")
				}
				return live, bound.digest
			}
		}
		t.Fatal("missing dynamic RA instance")
		return ra.Config{}, ""
	}
	first, identity := projectAt(snapshot, now)
	check := func(c ra.Config, deadline lifetime.Deadline) {
		t.Helper()
		if !reflect.DeepEqual(c.RDNSS, []string{"2001:db8::1"}) || !reflect.DeepEqual(c.DNSSL, []string{"static.example"}) {
			t.Fatal("learned entries contaminated static intent", c)
		}
		if len(c.LearnedRDNSS) != 2 || len(c.LearnedDNSSL) != 2 {
			t.Fatal("learned DNS missing", c)
		}
		for _, server := range c.LearnedRDNSS {
			if server.ValidUntil != deadline {
				t.Fatal("RDNSS lost absolute source deadline", server)
			}
		}
		for _, domain := range c.LearnedDNSSL {
			if domain.ValidUntil != deadline {
				t.Fatal("DNSSL lost absolute source deadline", domain)
			}
		}
	}
	check(first, snapshot.InformationRefreshAt)
	polled, polledIdentity := projectAt(snapshot, now.Add(50*time.Second))
	if !reflect.DeepEqual(first, polled) || polledIdentity != identity {
		t.Fatal("polling changed absolute deadlines or worker identity")
	}
	renewed := snapshot
	renewed.InfoReceivedNanos = uint64(clock.BoottimeNanoseconds) + uint64(60*time.Second)
	renewed.InformationRefreshAt = lifetime.After(clock, 240)
	live, renewedIdentity := projectAt(renewed, now.Add(60*time.Second))
	check(live, renewed.InformationRefreshAt)
	if renewedIdentity != identity {
		t.Fatal("fresh DNS information restarts the RA worker")
	}
	for _, value := range []dhcp6config.Snapshot{
		snapshot,
		func() dhcp6config.Snapshot { v := renewed; v.InformationRefreshAt = lifetime.After(clock, 1); return v }(),
		func() dhcp6config.Snapshot { v := renewed; v.InfoReceivedNanos = 0; return v }(),
		func() dhcp6config.Snapshot { v := renewed; v.InformationRefreshAt = lifetime.Deadline{}; return v }(),
	} {
		live, digest := projectAt(value, now.Add(181*time.Second))
		if len(live.LearnedRDNSS)+len(live.LearnedDNSSL) != 0 || len(live.RDNSS) != 1 || len(live.DNSSL) != 1 || digest != identity {
			t.Fatal("stale/missing DNS was retained or disturbed static/template data", live, digest)
		}
	}
	infinite := snapshot
	infinite.InformationRefreshSeconds = ^uint32(0)
	infinite.InformationRefreshAt = lifetime.After(clock, ^uint32(0))
	live, digest := projectAt(infinite, now)
	until, _ := snapshot.LeaseUntil()
	check(live, until)
	if digest != identity {
		t.Fatal("infinite refresh changed worker template")
	}
	finiteBeyondPD := snapshot
	finiteBeyondPD.InformationRefreshAt = lifetime.After(clock, 3600)
	live, _ = projectAt(finiteBeyondPD, now)
	check(live, until)
}
