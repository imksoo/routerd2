package runtimeplan

import (
	"encoding/json"
	"net/netip"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// A protocol plan belongs to one immutable source configuration. Observations
// select its workers and supply live data; they do not rebuild worker identity.
type protocolPlan struct {
	ntp          map[string]ntpconfig.Node
	dhcp6Servers map[string]dhcp6ServerPlan
	instances    map[string]binding
	leaseHA      map[string]leaseHAPlan
	dhcp6RA      map[string]string
	dns          map[string]dnsPlan
	bgp          map[string]bgpconfig.Config
	vrrp         map[string]vrrp.Config
	lanRA        map[string]advertisementPlan
	aftr         map[string]aftrDefinition
	serviceLinks map[string]bool
}

type advertisementPlan struct {
	selfDNS    []string
	config     ra.Config
	from       serviceconfig.RADelegation
	allocation string
}

type leaseHAPlan struct {
	owner   string
	serving binding
}

// The complete Native source has passed config.Compile, including worker
// ownership and service limits.
func prepareProtocols(doc config.Native, runtimeDir, stateDir string) (protocolPlan, error) {
	plan := protocolPlan{
		ntp:          make(map[string]ntpconfig.Node),
		dhcp6Servers: make(map[string]dhcp6ServerPlan),
		instances:    make(map[string]binding),
		leaseHA:      make(map[string]leaseHAPlan),
		dhcp6RA:      make(map[string]string),
		dns:          make(map[string]dnsPlan),
		bgp:          make(map[string]bgpconfig.Config),
		vrrp:         make(map[string]vrrp.Config),
		lanRA:        make(map[string]advertisementPlan),
		aftr:         make(map[string]aftrDefinition),
		serviceLinks: make(map[string]bool),
	}
	add := func(instance serviceconfig.Instance) error {
		bound, err := encodeInstance(instance)
		if err != nil {
			return err
		}
		plan.instances[instance.Name] = bound
		return nil
	}
	for _, service := range doc.Services {
		if service.Tailscale != nil {
			plan.serviceLinks[service.Tailscale.Interface] = true
		}
		if service.PPPoE != nil {
			plan.serviceLinks[service.PPPoE.PPPInterface] = true
		}
		if service.DSLite != nil {
			plan.serviceLinks[service.DSLite.Tunnel] = true
		}
		if service.VRRP != nil && service.VRRP.VIPInterface != "" {
			plan.serviceLinks[service.VRRP.VIPInterface] = true
		}
		name := service.ModuleName()
		if service.NTP != nil {
			plan.ntp[name] = *service.NTP
		}
		if service.DHCP6Server != nil {
			refs := func(names []string) []string {
				out := make([]string, len(names))
				for i, name := range names {
					out[i] = "service." + name
				}
				return out
			}
			c := service.DHCP6Server
			serverPlan := dhcp6ServerPlan{iface: c.Interface, dns: refs(c.DNSFrom), sntp: refs(c.SNTPFrom), search: refs(c.SearchFrom)}
			if c.DNSDelegation != nil {
				serverPlan.dnsDelegation = c.DNSDelegation.Service + "/" + c.DNSDelegation.Delegation
			}
			plan.dhcp6Servers[name] = serverPlan
		}
		if service.DHCP6 != nil && service.DHCP6.RAFrom != "" {
			plan.dhcp6RA[name] = "service." + service.DHCP6.RAFrom
		}
		var instance serviceconfig.Instance
		var err error
		switch {
		case service.DHCP6 != nil && service.DHCP6.HA != nil:
			cfg := service.DHCP6Config(stateDir)
			body, encodeErr := json.Marshal(cfg)
			instance, err = serviceconfig.Instance{Name: name, Role: "dhcp6", Config: body}, encodeErr
			cfg.Standby = false
			active, activeErr := json.Marshal(cfg)
			if activeErr != nil {
				return protocolPlan{}, activeErr
			}
			bound, bindErr := encodeInstance(serviceconfig.Instance{Name: name, Role: "dhcp6", Config: active})
			if bindErr != nil {
				return protocolPlan{}, bindErr
			}
			plan.leaseHA[name] = leaseHAPlan{owner: "service." + service.DHCP6.HA.Owner, serving: bound}
		case service.DHCPServer != nil && service.DHCPServer.HA != nil:
			cfg := service.DHCPServerConfig(stateDir)
			body, encodeErr := json.Marshal(cfg)
			instance, err = serviceconfig.Instance{Name: name, Role: "dhcp_server", Config: body}, encodeErr
			cfg.Standby = false
			active, activeErr := json.Marshal(cfg)
			if activeErr != nil {
				return protocolPlan{}, activeErr
			}
			bound, bindErr := encodeInstance(serviceconfig.Instance{Name: name, Role: "dhcp_server", Config: active})
			if bindErr != nil {
				return protocolPlan{}, bindErr
			}
			plan.leaseHA[name] = leaseHAPlan{owner: "service." + service.DHCPServer.HA.Owner, serving: bound}
		case service.DNS != nil:
			cfg := service.DNSConfig(runtimeDir, stateDir)
			prepared, prepareErr := prepareDNS(cfg.Runtime)
			if prepareErr != nil {
				return protocolPlan{}, prepareErr
			}
			prepared.listenInterface = cfg.ListenInterface
			plan.dns[name] = prepared
			body, encodeErr := json.Marshal(cfg)
			instance, err = serviceconfig.Instance{Name: name, Role: "dns", Config: body}, encodeErr
		case service.BGP != nil:
			cfg := service.BGPConfig(runtimeDir, stateDir)
			plan.bgp[name] = cfg
			body, encodeErr := json.Marshal(cfg)
			instance, err = serviceconfig.Instance{Name: name, Role: "bgp", Config: body}, encodeErr
		case service.VRRP != nil:
			cfg := service.VRRPConfig()
			plan.vrrp[name] = cfg
			body, encodeErr := json.Marshal(cfg)
			instance, err = serviceconfig.Instance{Name: name, Role: "vrrp", Config: body}, encodeErr
		default:
			instance, err = service.Instance(runtimeDir, stateDir)
		}
		if err != nil {
			return protocolPlan{}, err
		}
		if err := add(instance); err != nil {
			return protocolPlan{}, err
		}
		if service.DSLite != nil {
			plan.aftr[name] = prepareAFTR(doc, service)
		}
		if service.RA != nil && service.RA.From != nil {
			cfg := service.RA.Config
			cfg.Resource = service.Name
			from := *service.RA.From
			plan.lanRA[name] = advertisementPlan{config: cfg, from: from, allocation: from.Service + "/" + from.Delegation}
		}
	}
	for name, resolver := range plan.dns {
		resolver.vips = make(map[string][]netip.Addr)
		for _, source := range resolver.listeners {
			if source.VRRP == "" {
				continue
			}
			module := "service." + source.VRRP
			if _, prepared := resolver.vips[module]; prepared {
				continue
			}
			for _, value := range plan.vrrp[module].Addresses {
				address, err := netip.ParsePrefix(value)
				if err != nil {
					return protocolPlan{}, err
				}
				resolver.vips[module] = append(resolver.vips[module], address.Addr())
			}
		}
		plan.dns[name] = resolver
	}
	for name, server := range plan.dhcp6Servers {
		if server.dnsDelegation == "" {
			continue
		}
		var err error
		server.selfDNS, err = delegatedDNSProviders(plan.dns, server.dnsDelegation, server.iface)
		if err != nil {
			return protocolPlan{}, err
		}
		plan.dhcp6Servers[name] = server
	}
	for module, advertisement := range plan.lanRA {
		if !advertisement.from.RDNSSRouter {
			continue
		}
		var err error
		advertisement.selfDNS, err = delegatedDNSProviders(plan.dns, advertisement.allocation, advertisement.config.Interface)
		if err != nil {
			return protocolPlan{}, err
		}
		plan.lanRA[module] = advertisement
	}
	return plan, nil
}

func sameInstances(a, b []binding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		// Instances are sorted by name; the digest includes name, role and
		// every configuration byte of that immutable worker.
		if a[i].digest != b[i].digest {
			return false
		}
	}
	return true
}

func delegatedDNSProviders(resolvers map[string]dnsPlan, allocation, iface string) ([]string, error) {
	var providers []string
	for module, resolver := range resolvers {
		selected := false
		for _, source := range resolver.listeners {
			if resolver.runtime.Spec.Listen[source.Listener].Port != 53 {
				continue
			}
			selected = selected || source.Service+"/"+source.Delegation == allocation || source.Interface == iface && source.Family != 4
		}
		if resolver.listenInterface == "" || resolver.listenInterface == iface {
			for _, listener := range resolver.runtime.Spec.Listen {
				if listener.Port != 53 {
					continue
				}
				for _, value := range listener.Addresses {
					address, err := netip.ParseAddr(value)
					if err != nil {
						return nil, err
					}
					selected = selected || address.Is6() && address.IsUnspecified()
				}
			}
		}
		if selected {
			providers = append(providers, module)
		}
	}
	return providers, nil
}
