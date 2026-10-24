package runtimeplan

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	old6 "github.com/imksoo/routerd/pkg/pdclient"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestPDLearnedDNSDeadlineAndStableRAIdentity(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	doc := config.Native{Services: []serviceconfig.Service{{Name: "pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 7, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", SubnetID: 3, Advertise: true, UseLearnedDNS: true, RDNSS: []string{"2001:db8::1"}, DNSSL: []string{"static.example"}}}}}}}
	snapshot := dhcp6pd.Snapshot{Active: true, InformationRefreshSeconds: 600, InformationRefreshAt: now.Add(180 * time.Second), Snapshot: old6.Snapshot{Resource: "pd", Interface: "wan0", IAID: 7, CurrentPrefix: "2001:db8:100::/56", AcquiredAt: now, Preferred: 400, Valid: 600, ExpiresAt: now.Add(600 * time.Second), InfoUpdatedAt: now, DNSServers: []string{"2001:db8::1", "2001:db8::53"}, DomainSearch: []string{"static.example", "learned.example"}}}
	projectAt := func(value dhcp6pd.Snapshot, at time.Time) (ra.Config, string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		p, err := project(doc, "/run/routerd", "/var/lib/routerd", map[string]serviceconfig.Update{"service.pd": {Role: "dhcp6pd", Snapshot: raw}}, at)
		if err != nil {
			t.Fatal(err)
		}
		live, found := p.liveRA["service.pd-lan-ra"]
		if !found {
			t.Fatal("expected live LAN advertisement")
		}
		for _, instance := range p.instances {
			if instance.Name == "service.pd-lan-ra" {
				var template ra.Config
				if err := json.Unmarshal(instance.Config, &template); err != nil {
					t.Fatal(err)
				}
				if len(template.RDNSS)+len(template.DNSSL)+len(template.LearnedRDNSS)+len(template.LearnedDNSSL) != 0 {
					t.Fatal("live DNS leaked into immutable worker template")
				}
				_, digest, err := encodeInstance(instance)
				if err != nil {
					t.Fatal(err)
				}
				return live, digest
			}
		}
		t.Fatal("missing dynamic RA instance")
		return ra.Config{}, ""
	}
	first, identity := projectAt(snapshot, now)
	check := func(c ra.Config, deadline time.Time) {
		t.Helper()
		if !reflect.DeepEqual(c.RDNSS, []string{"2001:db8::1"}) || !reflect.DeepEqual(c.DNSSL, []string{"static.example"}) {
			t.Fatal("learned entries contaminated static intent", c)
		}
		if len(c.LearnedRDNSS) != 2 || len(c.LearnedDNSSL) != 2 {
			t.Fatal("learned DNS missing", c)
		}
		for _, server := range c.LearnedRDNSS {
			if !server.ValidUntil.Equal(deadline) {
				t.Fatal("RDNSS lost absolute source deadline", server)
			}
		}
		for _, domain := range c.LearnedDNSSL {
			if !domain.ValidUntil.Equal(deadline) {
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
	renewed.InfoUpdatedAt = now.Add(60 * time.Second)
	renewed.InformationRefreshAt = now.Add(240 * time.Second)
	live, renewedIdentity := projectAt(renewed, now.Add(60*time.Second))
	check(live, renewed.InformationRefreshAt)
	if renewedIdentity != identity {
		t.Fatal("fresh DNS information restarts the RA worker")
	}
	for _, value := range []dhcp6pd.Snapshot{
		snapshot,
		func() dhcp6pd.Snapshot { v := renewed; v.InformationStale = true; return v }(),
		func() dhcp6pd.Snapshot { v := renewed; v.InfoUpdatedAt = time.Time{}; return v }(),
		func() dhcp6pd.Snapshot { v := renewed; v.InformationRefreshAt = time.Time{}; return v }(),
	} {
		live, digest := projectAt(value, now.Add(181*time.Second))
		if len(live.LearnedRDNSS)+len(live.LearnedDNSSL) != 0 || len(live.RDNSS) != 1 || len(live.DNSSL) != 1 || digest != identity {
			t.Fatal("stale/missing DNS was retained or disturbed static/template data", live, digest)
		}
	}
	infinite := snapshot
	infinite.InformationRefreshSeconds = ^uint32(0)
	infinite.InformationRefreshAt = time.Time{}
	live, digest := projectAt(infinite, now)
	check(live, snapshot.ExpiresAt)
	if digest != identity {
		t.Fatal("infinite refresh changed worker template")
	}
	finiteBeyondPD := snapshot
	finiteBeyondPD.InformationRefreshAt = now.Add(time.Hour)
	live, _ = projectAt(finiteBeyondPD, now)
	check(live, snapshot.ExpiresAt)
}
