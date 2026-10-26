package runtimeplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestPreparedWorkerFilesMatchSupervisorIdentities(t *testing.T) {
	doc := config.Native{Services: []serviceconfig.Service{
		{Name: "pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 7, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", Advertise: true}}}},
		{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}}}},
	}}
	runtimeDir, stateDir := t.TempDir(), t.TempDir()
	plan := protocolsFor(t, doc, runtimeDir, stateDir)
	workers := make([]binding, 0, len(plan.instances))
	for _, bound := range plan.instances {
		workers = append(workers, bound)
	}
	set := selectInstances(workers, nil)
	r := &Runtime{dir: runtimeDir}
	if err := r.prepare(set); err != nil {
		t.Fatal(err)
	}
	for _, module := range set.modules {
		body, err := os.ReadFile(filepath.Join(runtimeDir, "instances", module.Name, module.ConfigDigest+".json"))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		var instance serviceconfig.Instance
		if err := json.Unmarshal(body, &instance); err != nil {
			t.Fatal(err)
		}
		if module.ConfigDigest != hex.EncodeToString(sum[:]) || instance.Name != module.Name || instance.Role != module.Role {
			t.Fatal("supervisor identity differs from actual child file", module, instance)
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
	facts := projectionFacts{}
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
