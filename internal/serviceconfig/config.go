// Package serviceconfig describes bundled protocol instances. Configuration
// contains protocol intent; private socket, journal and executable paths belong
// to the runtime compiler and cannot be supplied by a remote configuration.
package serviceconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcprelayconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
)

type Service struct {
	Health     *health.Config           `json:"health,omitempty"`
	BFD        *bfd.Config              `json:"bfd,omitempty"`
	DSLite     *DSLite                  `json:"ds_lite,omitempty"`
	RAClient   *RAClient                `json:"ra_client,omitempty"`
	DHCPServer *DHCPServer              `json:"dhcp_server,omitempty"`
	DHCPRelay  *dhcprelayconfig.Config  `json:"dhcp_relay,omitempty"`
	BGP        *BGP                     `json:"bgp,omitempty"`
	Name       string                   `json:"name"`
	DHCP4      *DHCP4                   `json:"dhcp4,omitempty"`
	DHCP6      *DHCP6                   `json:"dhcp6,omitempty"`
	RA         *ra.Config               `json:"ra,omitempty"`
	DNS        *dnsconfig.RuntimeConfig `json:"dns,omitempty"`
	VRRP       *vrrp.Config             `json:"vrrp,omitempty"`
}

type DHCP4 struct {
	Interface        string `json:"interface"`
	Hostname         string `json:"hostname,omitempty"`
	RequestedAddress string `json:"requested_address,omitempty"`
	ClassID          string `json:"class_id,omitempty"`
	ClientID         string `json:"client_id,omitempty"`
	DefaultRoute     *bool  `json:"default_route,omitempty"`
	Metric           int    `json:"metric,omitempty"`
	Table            int    `json:"table,omitempty"`
}

type DHCP6 struct {
	Interface      string       `json:"interface"`
	ClientDUID     string       `json:"client_duid,omitempty"`
	IAID           uint32       `json:"iaid"`
	PrefixLength   int          `json:"prefix_length,omitempty"`
	RequestAddress bool         `json:"request_address,omitempty"`
	RequestPrefix  bool         `json:"request_prefix,omitempty"`
	Delegations    []Delegation `json:"delegations,omitempty"`
}

// Delegation allocates a /64 inside an acquired prefix. SubnetID occupies the
// bits between the delegated prefix length and bit 64; HostID defaults to 1.
// Advertisements use absolute lease deadlines, including deprecated prefixes.
type Delegation struct {
	Name                  string   `json:"name"`
	Interface             string   `json:"interface"`
	SubnetID              uint64   `json:"subnet_id,omitempty"`
	HostID                uint64   `json:"host_id,omitempty"`
	Advertise             bool     `json:"advertise,omitempty"`
	RouterLifetimeSeconds *int     `json:"router_lifetime_seconds,omitempty"`
	RDNSS                 []string `json:"rdnss,omitempty"`
	DNSSL                 []string `json:"dnssl,omitempty"`
	UseLearnedDNS         bool     `json:"use_learned_dns,omitempty"`
}

// Instance is the immutable private configuration received by a child. Its
// exact JSON bytes are addressed by SHA-256 and verified before execution.
type Instance struct {
	// EventDirectory is generated private worker storage, never an operator
	// executable/path capability or a current protocol lease.
	EventDirectory string          `json:"event_directory,omitempty"`
	Name           string          `json:"name"`
	Role           string          `json:"role"`
	Config         json.RawMessage `json:"config"`
	// DynamicRA obtains its prefix/lifetime data from the control service;
	// the immutable config contains only the interface/template identity.
	DynamicRA  bool `json:"dynamic_ra,omitempty"`
	DynamicDNS bool `json:"dynamic_dns,omitempty"`
}

var safeName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

func (s Service) Role() string {
	if s.Health != nil {
		return "health"
	}
	if s.BFD != nil {
		return "bfd"
	}
	if s.DSLite != nil {
		return "aftr"
	}
	if s.RAClient != nil {
		return "ra_client"
	}
	if s.DHCPServer != nil {
		return "dhcp_server"
	}
	if s.DHCPRelay != nil {
		return "dhcp_relay"
	}
	if s.BGP != nil {
		return "bgp"
	}
	if s.DHCP4 != nil {
		return "dhcp4"
	}
	if s.DHCP6 != nil {
		return "dhcp6"
	}
	if s.RA != nil {
		return "ra"
	}
	if s.DNS != nil {
		return "dns"
	}
	if s.VRRP != nil {
		return "vrrp"
	}
	return ""
}
func (s Service) ModuleName() string { return "service." + s.Name }
func (s Service) Interface() string {
	if s.Health != nil {
		return s.Health.Interface
	}
	if s.BFD != nil {
		return s.BFD.Interface
	}
	if s.DSLite != nil {
		return s.DSLite.Interface
	}
	if s.RAClient != nil {
		return s.RAClient.Interface
	}
	if s.DHCPServer != nil {
		return s.DHCPServer.Interface
	}
	if s.DHCP4 != nil {
		return s.DHCP4.Interface
	}
	if s.DHCP6 != nil {
		return s.DHCP6.Interface
	}
	if s.RA != nil {
		return s.RA.Interface
	}
	if s.VRRP != nil {
		return s.VRRP.Interface
	}
	return ""
}

func Validate(services []Service) error {
	if len(services) > 64 {
		return errors.New("at most 64 configured services are supported")
	}
	names := map[string]bool{}
	sockets := map[string]string{}
	dhcpListeners := map[string]string{}
	count := len(services)
	generatedNames := map[string]string{}
	for _, s := range services {
		if !safeName.MatchString(s.Name) || names[s.Name] {
			return fmt.Errorf("invalid or duplicate service name %q (lowercase letters, digits and hyphens; max 24)", s.Name)
		}
		names[s.Name] = true
		n := 0
		for _, set := range []bool{s.Health != nil, s.BFD != nil, s.DSLite != nil, s.RAClient != nil, s.DHCPServer != nil, s.DHCPRelay != nil, s.DHCP4 != nil, s.DHCP6 != nil, s.RA != nil, s.DNS != nil, s.VRRP != nil, s.BGP != nil} {
			if set {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("service %s must select exactly one protocol", s.Name)
		}
		if s.DNS == nil && s.BGP == nil && s.DHCPRelay == nil && !(s.BFD != nil && s.BFD.Multihop) && !interfaceName.MatchString(s.Interface()) {
			return fmt.Errorf("service %s has an invalid interface", s.Name)
		}
		key := s.Role() + ":" + s.Interface()
		var dhcpInterfaces []string
		if s.DHCPServer != nil {
			dhcpInterfaces = append(dhcpInterfaces, s.DHCPServer.Interface)
		}
		if s.DHCPRelay != nil {
			key = "dhcp_relay:" + s.DHCPRelay.VRF
			for _, iface := range s.DHCPRelay.Interfaces {
				dhcpInterfaces = append(dhcpInterfaces, iface.Name)
			}
		}
		for _, iface := range dhcpInterfaces {
			if previous := dhcpListeners[iface]; previous != "" {
				return fmt.Errorf("services %s and %s compete for DHCP on %s", previous, s.Name, iface)
			}
			dhcpListeners[iface] = s.Name
		}
		if s.DNS != nil || s.BGP != nil || s.DSLite != nil || s.Health != nil {
			key = s.Role() + ":" + s.Name
		}
		if s.VRRP != nil {
			key = fmt.Sprintf("vrrp:%s:%s:%s:%d", s.VRRP.VRF, s.VRRP.Interface, s.VRRP.Family, s.VRRP.VirtualRouterID)
		}
		if s.BFD != nil {
			address, _ := netip.ParseAddr(s.BFD.SourceAddress)
			family := "ipv6"
			if address.Is4() {
				family = "ipv4"
			}
			key = "bfd:" + s.BFD.Interface + ":" + family
			if s.BFD.Multihop {
				key = "bfd-multihop:" + s.BFD.VRF + ":" + s.BFD.SourceAddress
			}
		}
		if other, ok := sockets[key]; ok {
			return fmt.Errorf("services %s and %s compete for %s", other, s.Name, key)
		}
		sockets[key] = s.Name
		switch s.Role() {
		case "health":
			if s.Health.Resource != "" && s.Health.Resource != s.Name {
				return errors.New("health resource is managed by service name")
			}
			if err := health.Validate(s.HealthConfig()); err != nil {
				return err
			}
		case "bfd":
			if s.BFD.Resource != "" && s.BFD.Resource != s.Name {
				return errors.New("BFD resource is managed by the service name")
			}
			if err := bfd.Validate(s.BFDConfig()); err != nil {
				return err
			}
		case "aftr":
			if err := validateDSLite(s); err != nil {
				return err
			}
		case "ra_client":
			if s.RAClient.Metric < 0 || uint64(s.RAClient.Metric) > 1<<32-1 || s.RAClient.Table < 0 || uint64(s.RAClient.Table) > 1<<32-1 {
				return errors.New("invalid RA client route metric/table")
			}
			if err := raclient.Validate(s.RAClientConfig("/var/lib/routerd")); err != nil {
				return err
			}
		case "dhcp_server":
			if err := dhcpserverconfig.Validate(s.DHCPServerConfig("/run/routerd", "/var/lib/routerd")); err != nil {
				return err
			}
		case "dhcp_relay":
			if err := dhcprelayconfig.Validate(*s.DHCPRelay); err != nil {
				return err
			}
		case "bgp":
			if s.BGP.MaxRoutes > 4096 {
				return errors.New("bundled BGP max_routes must not exceed 4096 within the bounded snapshot IPC")
			}
			if err := bgpconfig.Validate(s.BGPConfig("/run/routerd", "/var/lib/routerd")); err != nil {
				return err
			}
		case "dns":
			c := *s.DNS
			if c.Resource != "" && c.Resource != s.Name {
				return errors.New("DNS resource is managed by the service name")
			}
			c.Resource = s.Name
			if c.Spec.QueryLog.Path != "" {
				return errors.New("DNS query-log path is managed below the routerd state directory")
			}
			for _, z := range c.Zones {
				if z.Spec.DHCPDerived.LeaseFile != "" {
					return errors.New("DNS lease files are not a module input; use typed lease updates")
				}
			}
			if err := dnsconfig.Validate(s.DNSConfig("/run/routerd", "/var/lib/routerd")); err != nil {
				return err
			}
		case "vrrp":
			c := s.VRRPConfig()
			if s.VRRP.Resource != "" && s.VRRP.Resource != s.Name {
				return errors.New("VRRP resource is managed by the service name")
			}
			if err := vrrp.Validate(c); err != nil {
				return err
			}
		case "dhcp4":
			c := s.DHCP4
			if c.Metric < 0 || c.Table < 0 || uint64(c.Table) > 1<<32-1 {
				return fmt.Errorf("service %s has an invalid route metric/table", s.Name)
			}
			if err := dhcp4.Validate(s.DHCP4Config("/var/lib/routerd")); err != nil {
				return err
			}
		case "dhcp6":
			if err := dhcp6config.Validate(s.DHCP6Config("/var/lib/routerd")); err != nil {
				return err
			}
			if !s.DHCP6.RequestPrefix && len(s.DHCP6.Delegations) > 0 {
				return errors.New("DHCPv6 delegations require request_prefix")
			}
			lanNames := map[string]bool{}
			for _, lan := range s.DHCP6.Delegations {
				if !safeName.MatchString(lan.Name) || lanNames[lan.Name] || !interfaceName.MatchString(lan.Interface) {
					return fmt.Errorf("service %s has invalid/duplicate delegation name or invalid interface", s.Name)
				}
				lanNames[lan.Name] = true
				if lan.RouterLifetimeSeconds != nil && (*lan.RouterLifetimeSeconds < 0 || *lan.RouterLifetimeSeconds > 9000 || (*lan.RouterLifetimeSeconds > 0 && *lan.RouterLifetimeSeconds < 600)) {
					return errors.New("delegated RA lifetime must be zero or 600..9000 seconds")
				}
				for _, addr := range lan.RDNSS {
					a, e := netip.ParseAddr(addr)
					if e != nil || !a.Is6() || a.Is4In6() || a.IsUnspecified() || a.IsMulticast() {
						return fmt.Errorf("invalid delegated RDNSS %q", addr)
					}
				}
				if lan.Advertise {
					count++
					generated := s.Name + "-" + lan.Name + "-ra"
					if previous, ok := generatedNames[generated]; ok {
						return fmt.Errorf("delegations %s and %s/%s generate the same service %s", previous, s.Name, lan.Name, generated)
					}
					generatedNames[generated] = s.Name + "/" + lan.Name
					key := "ra:" + lan.Interface
					if other, ok := sockets[key]; ok {
						return fmt.Errorf("delegation on %s competes with %s", lan.Interface, other)
					}
					sockets[key] = s.Name + "/" + lan.Name
				}
			}
		case "ra":
			c := *s.RA
			if c.Resource != "" && c.Resource != s.Name {
				return errors.New("ra.resource is managed by the service name")
			}
			c.Resource = s.Name
			if err := ra.Validate(c); err != nil {
				return err
			}
		}
	}
	for generated, owner := range generatedNames {
		if names[generated] {
			return fmt.Errorf("delegation %s generates service %s which is already configured", owner, generated)
		}
	}
	if count > 128 {
		return errors.New("services plus delegated RA instances exceed 128")
	}
	if err := validateDSLiteSources(services); err != nil {
		return err
	}
	if err := validateBFDReferences(services); err != nil {
		return err
	}
	return validateDNSLeaseSources(services)
}

func (s Service) DHCP4Config(stateDir string) dhcp4.Config {
	c := s.DHCP4
	return dhcp4.Config{Resource: s.Name, Interface: c.Interface, StatePath: filepath.Join(stateDir, "leases", s.Name+".dhcp4.json"), Hostname: c.Hostname, RequestedAddress: c.RequestedAddress, ClassID: c.ClassID, ClientID: c.ClientID}
}
func (s Service) DHCP6Config(stateDir string) dhcp6config.Config {
	c := s.DHCP6
	return dhcp6config.Config{Resource: s.Name, Interface: c.Interface, StatePath: filepath.Join(stateDir, "identities", s.Name+".dhcp6.json"), ClientDUID: c.ClientDUID, IAID: c.IAID, PrefixLength: c.PrefixLength, RequestAddress: c.RequestAddress, RequestPrefix: c.RequestPrefix}
}
func (s Service) VRRPConfig() vrrp.Config {
	c := *s.VRRP
	c.Resource = s.Name
	return vrrp.Normalize(c)
}
func (s Service) DNSConfig(runtimeDir, stateDir string) dnsconfig.Config {
	c := *s.DNS
	c.Resource = s.Name
	// Normalization deep-copies slices before assigning module-owned paths.
	state := filepath.Join(stateDir, "services", s.Name, "dns.json")
	c = dnsconfig.NormalizeRuntime(c, state)
	return dnsconfig.Config{Runtime: c, SocketPath: filepath.Join(runtimeDir, "services", s.Name+".dns.sock"), StatePath: state, EventPath: filepath.Join(stateDir, "services", s.Name, "dns-events.jsonl")}
}
func (s Service) Instance(runtimeDir, stateDir string) (Instance, error) {
	var b []byte
	var err error
	switch s.Role() {
	case "health":
		b, err = json.Marshal(s.HealthConfig())
	case "bfd":
		b, err = json.Marshal(s.BFDConfig())
	case "aftr":
		b, err = json.Marshal(DSLiteInstance{Protocol: s.AFTRConfig(), Intent: *s.DSLite})
	case "ra_client":
		b, err = json.Marshal(s.RAClientConfig(stateDir))
	case "dhcp_server":
		b, err = json.Marshal(s.DHCPServerConfig(runtimeDir, stateDir))
	case "dhcp_relay":
		b, err = json.Marshal(*s.DHCPRelay)
	case "bgp":
		b, err = json.Marshal(s.BGPConfig(runtimeDir, stateDir))
	case "dhcp4":
		b, err = json.Marshal(s.DHCP4Config(stateDir))
	case "dhcp6":
		b, err = json.Marshal(s.DHCP6Config(stateDir))
	case "ra":
		v := *s.RA
		v.Resource = s.Name
		b, err = json.Marshal(v)
	case "dns":
		b, err = json.Marshal(s.DNSConfig(runtimeDir, stateDir))
	case "vrrp":
		b, err = json.Marshal(s.VRRPConfig())
	default:
		return Instance{}, errors.New("unsupported service")
	}
	instance := Instance{Name: s.ModuleName(), Role: s.Role(), Config: b, DynamicDNS: s.HasDNSLeaseSources()}
	if s.Health != nil {
		instance.EventDirectory = HealthEventDirectory(stateDir, s.Name)
	}
	return instance, err
}

// HealthEventDirectory is generated ownership, not an operator-selected path.
// Callers must validate the service name before using this reserved location.
func HealthEventDirectory(stateDir, name string) string {
	return filepath.Join(stateDir, "services", name, "health-events")
}

func (s Service) HealthConfig() health.Config {
	c := *s.Health
	c.Resource = s.Name
	return health.Normalize(c)
}
