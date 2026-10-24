package runtimeplan

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type projection struct {
	routeIssues []RouteIssue
	network     network.Desired
	instances   []serviceconfig.Instance
	liveRA      map[string]ra.Config
}

// project is pure: protocol facts and the committed intent produce a complete
// kernel projection. No producer can submit arbitrary addresses or routes.
func project(doc config.Document, runtimeDir, stateDir string, records map[string]serviceconfig.Update, now time.Time, dynamicInputs ...map[string]aftr.Input) (projection, error) {
	facts := projectionFacts{}
	if len(dynamicInputs) > 0 {
		facts.aftr = dynamicInputs[0]
	}
	return projectWithFacts(doc, runtimeDir, stateDir, records, now, facts)
}

type projectionFacts struct {
	aftr     map[string]aftr.Input
	bfdClock bfd.Clock
}

func (r *Runtime) projectLocked(doc config.Document, records map[string]serviceconfig.Update, now time.Time) (projection, error) {
	facts := projectionFacts{aftr: r.aftrInputs}
	for _, service := range doc.Services {
		if service.BGP != nil && len(service.BGP.PeerBFD) > 0 {
			// A clock read failure withholds BFD-dependent paths. It must not
			// manufacture a new lease or disturb unrelated routing intent.
			facts.bfdClock, _ = bfd.ReadClock()
			break
		}
	}
	return projectWithFacts(doc, r.dir, r.stateDir, records, now, facts)
}

func projectWithFacts(doc config.Document, runtimeDir, stateDir string, records map[string]serviceconfig.Update, now time.Time, facts projectionFacts) (projection, error) {
	b, _ := json.Marshal(doc.Network)
	p := projection{liveRA: map[string]ra.Config{}}
	if err := json.Unmarshal(b, &p.network); err != nil {
		return p, err
	}
	names := map[string]bool{}
	addInstance := func(v serviceconfig.Instance) error {
		if names[v.Name] {
			return fmt.Errorf("generated service name collision: %s", v.Name)
		}
		names[v.Name] = true
		p.instances = append(p.instances, v)
		return nil
	}
	for _, s := range doc.Services {
		instance, err := s.Instance(runtimeDir, stateDir)
		if err != nil {
			return p, err
		}
		if err = addInstance(instance); err != nil {
			return p, err
		}
		if s.Interface() != "" {
			ensureInterface(&p.network, s.Interface())
		}
		record, has := records[s.ModuleName()]
		switch s.Role() {
		case "ra_client":
			var snapshot raclient.Snapshot
			if has {
				if err := json.Unmarshal(record.Snapshot, &snapshot); err != nil {
					return p, err
				}
			}
			if err := projectRAClient(&p, s, snapshot, has, now); err != nil {
				return p, err
			}
		case "dhcp4":
			if !has {
				continue
			}
			var snap dhcp4.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snap); err != nil {
				return p, err
			}
			if !snap.Active || !now.Before(snap.ExpiresAt) {
				continue
			}
			a, err := netip.ParseAddr(snap.CurrentAddress)
			if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || snap.PrefixLength < 0 || snap.PrefixLength > 32 {
				return p, errors.New("invalid active DHCPv4 address")
			}
			prefix := netip.PrefixFrom(a, snap.PrefixLength)
			if err := addAddressWithOptions(&p.network, s.Interface(), prefix.String(), network.AddressOptions{PreferredUntil: snap.ExpiresAt, ValidUntil: snap.ExpiresAt}); err != nil {
				return p, err
			}
			if (s.DHCP4.DefaultRoute == nil || *s.DHCP4.DefaultRoute) && snap.DefaultGateway != "" {
				gw, err := netip.ParseAddr(snap.DefaultGateway)
				if err != nil || !gw.Is4() || gw.IsUnspecified() || gw.IsMulticast() {
					return p, errors.New("invalid DHCPv4 gateway")
				}
				if !prefix.Contains(gw) {
					return p, errors.New("DHCPv4 off-subnet gateway requires an explicit on-link route; no implicit unsafe route is installed")
				}
				p.network.Routes = append(p.network.Routes, network.Route{Destination: "0.0.0.0/0", Gateway: gw.String(), Interface: s.Interface(), Metric: s.DHCP4.Metric, Table: s.DHCP4.Table})
			}
		case "dhcp6pd":
			for _, lan := range s.DHCP6.Delegations {
				ensureInterface(&p.network, lan.Interface)
			}
			if !has {
				continue
			}
			var snap dhcp6pd.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snap); err != nil {
				return p, err
			}
			for _, lan := range s.DHCP6.Delegations {
				var prefixes []ra.Prefix
				if snap.Active && now.Before(snap.ExpiresAt) {
					prefix, addr, err := delegated(snap.CurrentPrefix, lan.SubnetID, lan.HostID)
					if err != nil {
						return p, fmt.Errorf("%s/%s: %w", s.Name, lan.Name, err)
					}
					preferred := snap.AcquiredAt.Add(time.Duration(snap.Preferred) * time.Second)
					if preferred.After(snap.ExpiresAt) {
						return p, errors.New("PD preferred deadline exceeds valid deadline")
					}
					if err := addAddressWithOptions(&p.network, lan.Interface, addr, network.AddressOptions{PreferredUntil: preferred, ValidUntil: snap.ExpiresAt}); err != nil {
						return p, err
					}
					prefixes = append(prefixes, ra.Prefix{Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetimeSeconds: boundedSeconds(snap.Valid), PreferredLifetimeSeconds: boundedSeconds(snap.Preferred), ValidUntil: snap.ExpiresAt, PreferredUntil: preferred})
				}
				for _, old := range snap.PreviousPrefixes {
					if !now.Before(old.ExpiresAt) || old.Prefix == snap.CurrentPrefix {
						continue
					}
					prefix, addr, err := delegated(old.Prefix, lan.SubnetID, lan.HostID)
					if err != nil {
						return p, err
					}
					// A fixed past deadline keeps the generated identity stable.
					if err := addAddressWithOptions(&p.network, lan.Interface, addr, network.AddressOptions{PreferredUntil: time.Unix(1, 0).UTC(), ValidUntil: old.ExpiresAt}); err != nil {
						return p, err
					}
					// The absolute deadline bounds the retained prefix. A zero
					// relative cap would instead withdraw it immediately on wire.
					prefixes = append(prefixes, ra.Prefix{Prefix: prefix, OnLink: true, Autonomous: true, ValidLifetimeSeconds: ^uint32(0), ValidUntil: old.ExpiresAt, PreferredUntil: time.Unix(1, 0).UTC()})
				}
				if !lan.Advertise || len(prefixes) == 0 {
					continue
				}
				name := s.Name + "-" + lan.Name + "-ra"
				module := "service." + name
				lifetime := 1800
				if lan.RouterLifetimeSeconds != nil {
					lifetime = *lan.RouterLifetimeSeconds
				}
				c := ra.Config{Resource: name, Interface: lan.Interface, Prefixes: prefixes, RouterLifetimeSeconds: lifetime, DNSSLifetimeSeconds: 1800, MinIntervalSeconds: 3, MaxIntervalSeconds: 10, RDNSS: append([]string(nil), lan.RDNSS...), DNSSL: append([]string(nil), lan.DNSSL...)}
				if lan.UseLearnedDNS {
					if until, usable := informationDeadline(snap, now); usable {
						for _, server := range unique(snap.DNSServers) {
							c.LearnedRDNSS = append(c.LearnedRDNSS, ra.DNSServer{Address: server, ValidUntil: until})
						}
						for _, name := range unique(snap.DomainSearch) {
							c.LearnedDNSSL = append(c.LearnedDNSSL, ra.SearchDomain{Domain: name, ValidUntil: until})
						}
					}
				}
				c.RDNSS = unique(c.RDNSS)
				c.DNSSL = unique(c.DNSSL)
				if err := ra.Validate(c); err != nil {
					return p, err
				}
				p.liveRA[module] = c
				template := c
				template.Prefixes = nil
				template.RDNSS = nil
				template.DNSSL = nil
				template.LearnedRDNSS = nil
				template.LearnedDNSSL = nil
				b, _ := json.Marshal(template)
				if err := addInstance(serviceconfig.Instance{Name: module, Role: "ra", Config: b, DynamicRA: true}); err != nil {
					return p, err
				}
			}
		case "vrrp":
			if !has {
				continue
			}
			var snap vrrp.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snap); err != nil {
				return p, err
			}
			if !snap.Active || !now.Before(snap.ValidUntil) {
				continue
			}
			c := s.VRRPConfig()
			p.network.Links = append(p.network.Links, network.Link{Name: c.VIPInterface, Kind: "macvlan", Parent: c.Interface, MACVLANMode: "bridge", HardwareAddr: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), IPv6AddrGenMode: "none"})
			ensureInterface(&p.network, c.VIPInterface)
			if c.VRF != "" {
				for i := range p.network.Interfaces {
					if p.network.Interfaces[i].Name == c.VIPInterface {
						master := c.VRF
						p.network.Interfaces[i].Master = &master
					}
				}
			}
			for _, address := range c.Addresses {
				if err := addAddressWithOptions(&p.network, c.VIPInterface, address, network.AddressOptions{NoDAD: c.Family == "ipv6", PreferredUntil: snap.ValidUntil, ValidUntil: snap.ValidUntil}); err != nil {
					return p, err
				}
			}
			if !c.AcceptMode && !c.AddressOwner {
				family := 4
				if c.Family == "ipv6" {
					family = 6
				}
				guard := network.VRRPGuard{Family: family, Interface: c.VIPInterface}
				for _, address := range c.Addresses {
					guard.Addresses = append(guard.Addresses, netip.MustParsePrefix(address).Addr().String())
				}
				p.network.VRRPGuards = append(p.network.VRRPGuards, guard)
			}
		}
	}
	if err := projectBGP(&p, doc, records, now, facts.bfdClock); err != nil {
		return p, err
	}
	if facts.aftr != nil {
		if err := projectDSLite(&p, doc, runtimeDir, stateDir, records, facts.aftr, now); err != nil {
			return p, err
		}
	}
	if len(p.instances) > 128 {
		return p, errors.New("runtime instance limit exceeded")
	}
	sort.Slice(p.instances, func(i, j int) bool { return p.instances[i].Name < p.instances[j].Name })
	if err := network.Validate(p.network); err != nil {
		return p, err
	}
	return p, nil
}

func ensureInterface(d *network.Desired, name string) {
	for _, v := range d.Interfaces {
		if v.Name == name {
			return
		}
	}
	d.Interfaces = append(d.Interfaces, network.Interface{Name: name})
}

// A dynamic owner must not take over a static or another protocol's address.
// In particular, an expiring DHCP lease may not turn a static address finite.
func addAddressWithOptions(d *network.Desired, name, address string, options network.AddressOptions) error {
	prefix, err := netip.ParsePrefix(address)
	if err != nil {
		return err
	}
	ensureInterface(d, name)
	for i := range d.Interfaces {
		l := &d.Interfaces[i]
		if l.Name != name {
			continue
		}
		for _, existing := range l.Addresses {
			p, err := netip.ParsePrefix(existing)
			if err != nil {
				return err
			}
			if p.Addr() == prefix.Addr() {
				return fmt.Errorf("dynamic address %s on %s conflicts with another committed address owner", prefix, name)
			}
		}
		l.Addresses = append(l.Addresses, prefix.String())
		if l.AddressOptions == nil {
			l.AddressOptions = map[string]network.AddressOptions{}
		}
		l.AddressOptions[prefix.String()] = options
		return nil
	}
	return errors.New("dynamic address interface is missing")
}
func delegated(prefix string, subnet, host uint64) (string, string, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Bits() > 64 || p != p.Masked() {
		return "", "", errors.New("delegated prefix must be a canonical IPv6 prefix no longer than /64")
	}
	bits := 64 - p.Bits()
	if bits < 64 && subnet >= uint64(1)<<bits {
		return "", "", errors.New("subnet_id does not fit acquired prefix")
	}
	b := p.Addr().As16()
	binary.BigEndian.PutUint64(b[:8], binary.BigEndian.Uint64(b[:8])|subnet)
	base := netip.AddrFrom16(b)
	if host == 0 {
		host = 1
	}
	binary.BigEndian.PutUint64(b[8:], host)
	return netip.PrefixFrom(base, 64).String(), netip.PrefixFrom(netip.AddrFrom16(b), 64).String(), nil
}
func boundedSeconds(v int64) uint32 {
	if v <= 0 {
		return 0
	}
	if v > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(v)
}
func unique(v []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range v {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
