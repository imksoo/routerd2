package network

// RoutingDomain is the explicitly declared VRF and its FIB table. The default
// domain has an empty VRF name and uses table 254.
type RoutingDomain struct {
	VRF   string
	Table int
}

// RoutingDomains resolves interface membership in a validated desired plan.
// Its acyclic master graph is checked by Validate; foreign kernel membership
// is not treated as operator intent.
func RoutingDomains(d Desired) map[string]RoutingDomain {
	domains := make(map[string]RoutingDomain, len(d.Interfaces)+len(d.Links))
	masters := make(map[string]string, len(d.Interfaces))
	for _, link := range d.Links {
		if link.Kind == "vrf" {
			domains[link.Name] = RoutingDomain{VRF: link.Name, Table: link.VRFTable}
		}
	}
	for _, iface := range d.Interfaces {
		if iface.Master != nil {
			masters[iface.Name] = *iface.Master
		}
	}
	for _, iface := range d.Interfaces {
		domain := RoutingDomain{Table: 254}
		for name := iface.Name; name != ""; name = masters[name] {
			if inherited, ok := domains[name]; ok {
				domain = inherited
				break
			}
		}
		domains[iface.Name] = domain
	}
	return domains
}
