package resourceprogram

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

// SAMTransport is the configured half of a learned RR topology. The enrollment
// client owns the remote nodes and its admission deadline; this recipe owns
// only the local underlay, pair-stable addressing and BGP policy.
type SAMTransport struct {
	Name                    string                        `json:"name"`
	Client                  string                        `json:"client"`
	ClaimRef                string                        `json:"claim_ref"`
	PolicyRef               string                        `json:"policy_ref"`
	RRSetRef                string                        `json:"rr_set_ref"`
	Self                    string                        `json:"self"`
	InnerPrefix             netip.Prefix                  `json:"inner_prefix"`
	Underlay                string                        `json:"underlay"`
	LocalEndpoint           string                        `json:"local_endpoint,omitempty"`
	LocalFromDHCP4          string                        `json:"local_from_dhcp4,omitempty"`
	BGP                     string                        `json:"bgp"`
	PeerASN                 uint32                        `json:"peer_asn"`
	EbgpMultihop            int                           `json:"ebgp_multihop,omitempty"`
	RouteReflectorClient    bool                          `json:"route_reflector_client,omitempty"`
	RouteReflectorClusterID string                        `json:"route_reflector_cluster_id,omitempty"`
	TimersProfile           string                        `json:"timers_profile,omitempty"`
	ConvergenceProfile      string                        `json:"convergence_profile,omitempty"`
	ImportPolicy            bgpdaemon.AppliedImportPolicy `json:"import_policy,omitempty"`
	ExportPolicy            bgpdaemon.AppliedExportPolicy `json:"export_policy,omitempty"`
}

func prepareSAMTransport(id string, spec SAMTransport, resources []Resource) (SAMTransport, []string, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	spec.Client = strings.TrimSpace(spec.Client)
	spec.ClaimRef = strings.TrimSpace(spec.ClaimRef)
	spec.PolicyRef = strings.TrimSpace(spec.PolicyRef)
	spec.RRSetRef = strings.TrimSpace(spec.RRSetRef)
	spec.Self = strings.TrimSpace(spec.Self)
	spec.Underlay = strings.TrimSpace(spec.Underlay)
	spec.LocalEndpoint = strings.TrimSpace(spec.LocalEndpoint)
	spec.LocalFromDHCP4 = strings.TrimSpace(spec.LocalFromDHCP4)
	spec.BGP = strings.TrimSpace(spec.BGP)
	spec.RouteReflectorClusterID = strings.TrimSpace(spec.RouteReflectorClusterID)
	spec.TimersProfile = strings.TrimSpace(spec.TimersProfile)
	spec.ConvergenceProfile = strings.TrimSpace(spec.ConvergenceProfile)

	if spec.Name == "" || spec.Client == "" || spec.Self == "" || spec.BGP == "" || spec.Underlay == "" {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s requires name, client, self, underlay and BGP service", id)
	}
	if !strings.HasPrefix(spec.ClaimRef, "SAMEnrollmentClaim/") || strings.TrimPrefix(spec.ClaimRef, "SAMEnrollmentClaim/") == "" ||
		!strings.HasPrefix(spec.PolicyRef, "SAMEnrollmentPolicy/") || strings.TrimPrefix(spec.PolicyRef, "SAMEnrollmentPolicy/") == "" ||
		!strings.HasPrefix(spec.RRSetRef, "SAMRRSet/") || strings.TrimPrefix(spec.RRSetRef, "SAMRRSet/") == "" {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s has incomplete enrollment identity", id)
	}
	if !spec.InnerPrefix.IsValid() || !spec.InnerPrefix.Addr().Is4() || spec.InnerPrefix.Bits() > 31 {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s requires an IPv4 pair-stable prefix with /31 slots", id)
	}
	spec.InnerPrefix = spec.InnerPrefix.Masked()
	if spec.PeerASN == 0 || spec.EbgpMultihop < 0 || spec.EbgpMultihop > 255 {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s has invalid BGP peer settings", id)
	}
	if (spec.LocalEndpoint == "") == (spec.LocalFromDHCP4 == "") {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s requires exactly one local endpoint source", id)
	}

	services := make(map[string]*Resource)
	for i := range resources {
		r := &resources[i]
		if r.Service != nil {
			services[r.Service.ModuleName()] = r
		}
	}
	client := services[spec.Client]
	if client == nil || client.Service.SAMClient == nil {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s client must name a SAM enrollment service", id)
	}
	if spec.ClaimRef != "SAMEnrollmentClaim/"+client.Service.SAMClient.ClaimName || spec.PolicyRef != client.Service.SAMClient.Claim.PolicyRef || spec.RRSetRef != client.Service.SAMClient.Claim.RRSetRef {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s enrollment identity does not match its client", id)
	}
	bgp := services[spec.BGP]
	if bgp == nil || bgp.Service.BGP == nil {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s BGP service is missing", id)
	}
	dependencies := []string{client.ID, bgp.ID}
	if spec.LocalEndpoint != "" {
		address, err := netip.ParseAddr(spec.LocalEndpoint)
		if err != nil || !address.Is4() || address.IsUnspecified() || address.IsMulticast() {
			return SAMTransport{}, nil, fmt.Errorf("SAM transport %s has invalid local endpoint", id)
		}
		spec.LocalEndpoint = address.String()
	} else {
		dhcp := services[spec.LocalFromDHCP4]
		if dhcp == nil || dhcp.Service.DHCP4 == nil || dhcp.Service.DHCP4.Interface != spec.Underlay {
			return SAMTransport{}, nil, fmt.Errorf("SAM transport %s DHCPv4 endpoint source must own its underlay", id)
		}
		dependencies = append(dependencies, dhcp.ID)
	}

	// Validate the exact peer fields in the context of their configured BGP
	// service. The dynamic endpoint itself is checked when the RR snapshot arrives.
	cfg := bgp.Service.BGPConfig("/run/routerd", "/var/lib/routerd")
	cfg.Runtime.Peers = make(map[string]bgpdaemon.AppliedPeer, len(cfg.Runtime.Peers)+1)
	for address, peer := range bgp.Service.BGP.Runtime.Peers {
		cfg.Runtime.Peers[address] = peer
	}
	cfg.Runtime.Peers["198.18.0.1"] = bgpdaemon.AppliedPeer{
		Address:                 "198.18.0.1",
		ASN:                     spec.PeerASN,
		EbgpMultihop:            spec.EbgpMultihop,
		RouteReflectorClient:    spec.RouteReflectorClient,
		RouteReflectorClusterID: spec.RouteReflectorClusterID,
		TimersProfile:           spec.TimersProfile,
		ConvergenceProfile:      spec.ConvergenceProfile,
		ImportPolicy:            spec.ImportPolicy,
		ExportPolicyName:        "sam-transport-validation",
		ExportPolicy:            spec.ExportPolicy,
	}
	if err := bgpconfig.Validate(cfg); err != nil {
		return SAMTransport{}, nil, fmt.Errorf("SAM transport %s BGP profile: %w", id, err)
	}
	return spec, slices.Compact(dependencies), nil
}

func ValidateSAMTransportPeer(spec SAMTransport, endpoint netip.Addr) error {
	if !endpoint.Is4() || endpoint.IsUnspecified() || endpoint.IsMulticast() {
		return errors.New("SAM RR node has an invalid IPv4 endpoint")
	}
	return nil
}
