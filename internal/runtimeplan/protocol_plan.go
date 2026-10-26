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
	instances     map[string]binding
	bgp           map[string]bgpconfig.Config
	vrrp          map[string]vrrp.Config
	lanRA         map[string]ra.Config
	aftr          map[string]aftrDefinition
	pppInterfaces map[string]bool
}

// The complete Native source has passed config.Compile, including generated
// worker-name collisions and the service/instance limits.
func prepareProtocols(doc config.Native, runtimeDir, stateDir string) (protocolPlan, error) {
	plan := protocolPlan{
		instances:     make(map[string]binding),
		bgp:           make(map[string]bgpconfig.Config),
		vrrp:          make(map[string]vrrp.Config),
		lanRA:         make(map[string]ra.Config),
		aftr:          make(map[string]aftrDefinition),
		pppInterfaces: make(map[string]bool),
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
		if service.PPPoE != nil {
			plan.pppInterfaces[service.PPPoE.PPPInterface] = true
		}
		name := service.ModuleName()
		var instance serviceconfig.Instance
		var err error
		switch {
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
			cfg := ra.Config{Resource: resource, Interface: lan.Interface, RouterLifetimeSeconds: lifetime, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10, RDNSS: unique(lan.RDNSS), DNSSL: unique(lan.DNSSL)}
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
