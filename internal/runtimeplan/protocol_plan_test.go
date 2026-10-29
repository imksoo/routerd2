package runtimeplan

import "github.com/imksoo/routerd2/internal/protocol/ra"

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/peertls"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestPreparedWorkerFilesMatchSupervisorIdentities(t *testing.T) {
	doc := config.Native{Services: []serviceconfig.Service{
		{Name: "pd", DHCP6: &serviceconfig.DHCP6{RequestPrefix: true, Interface: "wan0", IAID: 7, ClientDUID: "00030001020000000042", Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0"}}, HA: &serviceconfig.LeaseHA{Owner: "ha", Replication: peertls.Config{
			ListenAddress: netip.MustParseAddrPort("192.0.2.2:8444"), PeerAddress: netip.MustParseAddrPort("192.0.2.3:8444"),
			CertificateFile: "/keys/local.crt", PrivateKeyFile: "/keys/local.key", PeerCertificateFile: "/keys/peer.crt",
		}}}},
		{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Addresses: []string{"192.0.2.1/24"}}},
		{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}}}}, {Name: "pd-lan-ra", RA: &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: "lan0", RouterLifetimeSeconds: 1800, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10}, From: &serviceconfig.RADelegation{Service: "pd", Delegation: "lan"}}},
	}}
	runtimeDir, stateDir := t.TempDir(), t.TempDir()
	plan := protocolsFor(t, doc, runtimeDir, stateDir)
	workers := make([]binding, 0, len(plan.instances))
	for _, bound := range plan.instances {
		workers = append(workers, bound)
	}
	for _, ha := range plan.leaseHA {
		workers = append(workers, ha.serving)
	}
	r := &Runtime{dir: runtimeDir}
	if err := r.prepare(plan); err != nil {
		t.Fatal(err)
	}
	for _, bound := range workers {
		body, err := os.ReadFile(filepath.Join(runtimeDir, "instances", bound.instance.Name, bound.digest+".json"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		var instance serviceconfig.Instance
		if err := json.Unmarshal(body, &instance); err != nil {
			t.Fatal(err)
		}
		if bound.digest != hex.EncodeToString(sum[:]) || instance.Name != bound.instance.Name || instance.Role != bound.instance.Role {
			t.Fatal("supervisor identity differs from actual child file", bound.instance.Name, instance)
		}
	}
}

func protocolsFor(t testing.TB, doc config.Native, runtimeDir, stateDir string) protocolPlan {
	t.Helper()
	plan, err := prepareProtocols(doc, runtimeDir, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func projectNative(doc config.Native, runtimeDir, stateDir string, records map[string]serviceconfig.Update, now time.Time, inputs ...map[string]aftr.Input) (projection, error) {
	plan, err := prepareProtocols(doc, runtimeDir, stateDir)
	if err != nil {
		return projection{}, err
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		return projection{}, err
	}
	clock.WallTime = now
	facts := projectionFacts{bfdClock: clock}
	if len(inputs) > 0 {
		facts.aftr = inputs[0]
	}
	return projectWithFacts(doc, plan, records, now, facts)
}

func BenchmarkProjectEightBGPRouters(b *testing.B) {
	doc := config.Native{}
	for i := range 8 {
		peers := make(map[string]bgpconfig.AppliedPeer, 64)
		for j := range 64 {
			address := fmt.Sprintf("198.18.0.%d", j+1)
			peers[address] = bgpconfig.AppliedPeer{Address: address, ASN: 64513}
		}
		doc.Services = append(doc.Services, serviceconfig.Service{Name: fmt.Sprintf("edge%d", i), BGP: &serviceconfig.BGP{Table: 1001 + i, Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}, Peers: peers}}})
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	protocols := protocolsFor(b, doc, "/run/routerd", "/var/lib/routerd")
	b.ReportAllocs()
	for b.Loop() {
		p, err := projectWithFacts(doc, protocols, nil, now, projectionFacts{bfdClock: clock})
		if err != nil {
			b.Fatal(err)
		}
		_ = selectInstances(p.instances, nil)
	}
}
