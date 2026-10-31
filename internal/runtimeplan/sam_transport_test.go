package runtimeplan

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestSAMRRTransportUsesOriginalDeadlineAndWithdraws(t *testing.T) {
	clock := boottime.Clock{BootID: "01234567-89ab-cdef-0123-456789abcdef", BoottimeNanoseconds: int64(10 * time.Second), WallTime: time.Unix(1800000000, 0).UTC()}
	client := samclientconfig.Node{
		ClaimName: "leaf",
		Claim: api.SAMEnrollmentClaimSpec{
			PolicyRef:     "SAMEnrollmentPolicy/leaves",
			RRSetRef:      "SAMRRSet/rrs",
			LeafID:        "leaf-a",
			TunnelAddress: "10.255.0.2/32",
			Endpoint:      "192.0.2.1",
		},
		Endpoints: []string{"http://192.0.2.2:65432"},
	}
	doc := config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}},
		Services: []serviceconfig.Service{
			{Name: "sam", SAMClient: &client},
			{Name: "edge", BGP: &serviceconfig.BGP{Runtime: bgpconfig.AppliedConfig{Global: bgpconfig.AppliedGlobal{ASN: 64512, RouterID: "10.255.0.2", ListenPort: -1}}}},
		},
		SAMTransports: []resourceprogram.SAMTransport{{
			Name:          "leaf",
			Client:        "service.sam",
			ClaimRef:      "SAMEnrollmentClaim/leaf",
			PolicyRef:     "SAMEnrollmentPolicy/leaves",
			RRSetRef:      "SAMRRSet/rrs",
			Self:          "leaf-a",
			InnerPrefix:   netip.MustParsePrefix("10.255.0.0/24"),
			Underlay:      "wan0",
			LocalEndpoint: "192.0.2.1",
			BGP:           "service.edge",
			PeerASN:       64512,
		}},
	}
	protocols := protocolsFor(t, doc, t.TempDir(), t.TempDir())
	snapshot := samclientconfig.Snapshot{
		ClaimRef:      "SAMEnrollmentClaim/leaf",
		ClaimDigest:   "sha256:fixture",
		Phase:         "ready",
		ObservedAt:    clock.WallTime,
		ExpiresAt:     clock.WallTime.Add(time.Minute),
		BootID:        clock.BootID,
		ObservedNanos: uint64(clock.BoottimeNanoseconds),
		ExpiresNanos:  uint64(clock.BoottimeNanoseconds + int64(time.Minute)),
		RRSet: &api.SAMRRSetSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/leaves", Nodes: []api.SAMNodeSpec{
			{NodeRef: "rr-a", RouteReflector: true, SAMEndpoint: "192.0.2.2"},
		}},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]serviceconfig.Update{"service.sam": {Name: "service.sam", Role: "sam_client", ConfigDigest: protocols.instances["service.sam"].digest, Snapshot: raw}}
	p, err := projectWithFacts(doc, protocols, records, clock.WallTime, projectionFacts{bfdClock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.network.Links) != 1 || p.network.Links[0].Kind != "ipip" || p.network.Links[0].Authority.Nanos != snapshot.ExpiresNanos {
		t.Fatalf("RR transport did not retain its original deadline: %+v", p.network.Links)
	}
	if len(p.network.Routes) != 1 || p.network.Routes[0].Destination != "192.0.2.2/32" || p.network.Routes[0].Gateway != "" || p.network.Routes[0].ExpiresNanos != snapshot.ExpiresNanos {
		t.Fatalf("RR endpoint route is missing or unbounded: %+v", p.network.Routes)
	}
	if bindingDigest(p.instances, "service.edge") == protocols.instances["service.edge"].digest {
		t.Fatal("RR peer did not replace the BGP worker configuration")
	}

	snapshot.Phase, snapshot.RRSet = "revoked", nil
	raw, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	records["service.sam"] = serviceconfig.Update{Name: "service.sam", Role: "sam_client", ConfigDigest: protocols.instances["service.sam"].digest, Snapshot: raw}
	p, err = projectWithFacts(doc, protocols, records, clock.WallTime, projectionFacts{bfdClock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.network.Links) != 0 || len(p.network.Routes) != 0 || bindingDigest(p.instances, "service.edge") != protocols.instances["service.edge"].digest {
		t.Fatalf("revoked RR transport retained an effect: links=%+v routes=%+v", p.network.Links, p.network.Routes)
	}
}

func bindingDigest(bindings []binding, name string) string {
	for _, binding := range bindings {
		if binding.instance.Name == name {
			return binding.digest
		}
	}
	return ""
}
