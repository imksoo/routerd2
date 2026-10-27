package runtimeplan

import (
	"encoding/json"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// A protocol plan belongs to one immutable source configuration. Observations
// select its workers and supply live data; they do not rebuild worker identity.
type protocolPlan struct {
	dhcp6Servers map[string]dhcp6ServerPlan
	instances    map[string]binding
	leaseHA      map[string]leaseHAPlan
	dhcp6RA      map[string]string
	dns          map[string]dnsPlan
	bgp          map[string]bgpconfig.Config
	vrrp         map[string]vrrp.Config
	lanRA        map[string]ra.Config
	aftr         map[string]aftrDefinition
	serviceLinks map[string]bool
}

type leaseHAPlan struct {
	owner   string
	serving binding
}

// The complete Native source has passed config.Compile, including generated
// worker-name collisions and the service/instance limits.
func prepareProtocols(doc config.Native, runtimeDir, stateDir string) (protocolPlan, error) {
	plan := protocolPlan{
		dhcp6Servers: make(map[string]dhcp6ServerPlan),
		instances:    make(map[string]binding),
		leaseHA:      make(map[string]leaseHAPlan),
		dhcp6RA:      make(map[string]string),
		dns:          make(map[string]dnsPlan),
		bgp:          make(map[string]bgpconfig.Config),
		vrrp:         make(map[string]vrrp.Config),
		lanRA:        make(map[string]ra.Config),
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
		if service.DHCP6Server != nil {
			refs := func(names []string) []string {
				out := make([]string, len(names))
				for i, name := range names {
					out[i] = "service." + name
				}
				return out
			}
			c := service.DHCP6Server
			plan.dhcp6Servers[name] = dhcp6ServerPlan{iface: c.Interface, dns: refs(c.DNSFrom), sntp: refs(c.SNTPFrom), search: refs(c.SearchFrom)}
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
			cfg := service.DHCPServerConfig(runtimeDir, stateDir)
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
		if service.DHCP6 == nil {
			continue
		}
		for _, lan := range service.DHCP6.Delegations {
			if !lan.Advertise {
				continue
			}
			resource := service.Name + "-" + lan.Name + "-ra"
			module := "service." + resource
			lifetime := 1800
			if lan.RouterLifetimeSeconds != nil {
				lifetime = *lan.RouterLifetimeSeconds
			}
			cfg := ra.Config{Resource: resource, Interface: lan.Interface, OtherConfig: lan.OtherConfig, RouterLifetimeSeconds: lifetime, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10, RDNSS: unique(lan.RDNSS), DNSSL: unique(lan.DNSSL)}
			plan.lanRA[module] = cfg
			// All prefix and DNS data travels through the live RA input. Its
			// immutable worker contains only the interface/template identity.
			cfg.RDNSS, cfg.DNSSL = nil, nil
			body, err := json.Marshal(cfg)
			if err != nil {
				return protocolPlan{}, err
			}
			if err := add(serviceconfig.Instance{Name: module, Role: "ra", Config: body, DynamicRA: true}); err != nil {
				return protocolPlan{}, err
			}
		}
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
