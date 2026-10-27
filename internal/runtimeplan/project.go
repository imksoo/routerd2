package runtimeplan

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type projection struct {
	observationVersion uint64
	activationUntil    uint64
	vrrpOwners         map[string]vrrpOwnership
	native             config.Native
	resources          map[string]resourceprogram.ResourceState
	routeIssues        []RouteIssue
	bgpStatus          map[string]bgpProjectionStatus
	network            network.Desired
	instances          []binding
	liveRA             map[string]ra.Config
}

type projectionFacts struct {
	aftr     map[string]aftr.Input
	bfdClock bfd.Clock
	// A zero deadline means this election is already admitted.
	vrrpActivation map[string]uint64
}

func (r *Runtime) projectLocked(protocols protocolPlan, doc config.Native, records map[string]serviceconfig.Update, now time.Time, policy resourceprogram.FactSnapshot) (projection, error) {
	facts := projectionFacts{aftr: r.aftrInputs, vrrpActivation: make(map[string]uint64)}
	var err error
	facts.bfdClock, err = bfd.ReadClock()
	if err != nil {
		return projection{}, err
	}
	ownedRecords := make(map[string]serviceconfig.Update, len(records))
	for name, record := range records {
		if record.Role == "pppoe" {
			// The PPP worker owns the link itself. Its last finite observation
			// cannot authorize adding routes after that owner starts stopping.
			module, registered := r.guardStatus[name]
			if !registered || !module.Ready || module.Role != record.Role || module.Generation != record.Generation || module.ConfigDigest != record.ConfigDigest {
				continue
			}
			ownedRecords[name] = record
			continue
		}
		if record.Role != "vrrp" {
			ownedRecords[name] = record
			continue
		}
		binding, bound := r.bindings[name]
		if !bound || binding.digest != record.ConfigDigest {
			continue
		}
		module, registered := r.guardStatus[name]
		if registered && (module.Role != "vrrp" || module.Generation != record.Generation || module.ConfigDigest != record.ConfigDigest) || !registered && !r.guardUntil.IsZero() {
			continue
		}
		ownedRecords[name] = record
	}

	for _, service := range doc.Services {
		if service.VRRP != nil && service.VRRP.GracefulActivation != nil {
			name := service.ModuleName()
			u := ownedRecords[name]
			var election vrrp.Snapshot
			if json.Unmarshal(u.Snapshot, &election) == nil {
				if r.ownsVRRPLocked(u, election) {
					facts.vrrpActivation[name] = 0
				} else {
					ready, until := policy.ConditionAuthority(service.VRRP.GracefulActivation.ReadyWhen)
					if ready == resourceprogram.True && policy.NowNanos != 0 && policy.NowNanos < until {
						facts.vrrpActivation[name] = until
					}
				}
			}
		}

	}
	p, err := projectWithFacts(doc, protocols, ownedRecords, now, facts)
	if err == nil {
		p.network = network.ProjectLifetimes(p.network, boottime.Clock{BootID: facts.bfdClock.BootID, BoottimeNanoseconds: facts.bfdClock.BoottimeNanoseconds, WallTime: facts.bfdClock.WallTime})
	}
	return p, err
}

// Pure projection combines active intent with the source's immutable workers
// and current protocol evidence. Producers cannot submit arbitrary effects.
func projectWithFacts(doc config.Native, protocols protocolPlan, records map[string]serviceconfig.Update, now time.Time, facts projectionFacts) (projection, error) {
	p := projection{network: doc.Network, liveRA: map[string]ra.Config{}, native: doc, vrrpOwners: make(map[string]vrrpOwnership)}
	// Own the collections changed by protocol projection and lifetime removal.
	// Links and guards are only appended; all other intent is read-only.
	p.network.Links = slices.Clip(p.network.Links)
	p.network.VRRPGuards = slices.Clip(p.network.VRRPGuards)
	p.network.Routes = slices.Clone(p.network.Routes)
	p.network.Interfaces = slices.Clone(p.network.Interfaces)
	for i := range p.network.Interfaces {
		iface := &p.network.Interfaces[i]
		iface.Addresses = slices.Clone(iface.Addresses)
		iface.AddressOptions = maps.Clone(iface.AddressOptions)
	}
	for _, s := range doc.Services {
		p.instances = append(p.instances, protocols.instances[s.ModuleName()])
	}
	for _, s := range doc.Services {
		if s.Interface() != "" && !protocols.serviceLinks[s.Interface()] {
			ensureInterface(&p.network, s.Interface())
		}
		if s.DHCPRelay != nil {
			for _, iface := range s.DHCPRelay.Interfaces {
				ensureInterface(&p.network, iface.Name)
			}
		}
		record, has := records[s.ModuleName()]
		switch s.Role() {
		case "pppoe":
			if !has {
				continue
			}
			var snapshot pppoeconfig.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snapshot); err != nil {
				return p, err
			}
			if snapshot.Active && snapshot.ValidUntil.Live(facts.bfdClock) && (s.PPPoE.DefaultRoute == nil || *s.PPPoE.DefaultRoute) {
				for _, address := range []struct{ local, destination string }{{snapshot.Address, "0.0.0.0/0"}, {snapshot.IPv6Address, "::/0"}} {
					if address.local != "" {
						p.network.Routes = append(p.network.Routes, network.Route{Destination: address.destination, Interface: s.PPPoE.PPPInterface, Metric: s.PPPoE.Metric, Table: s.PPPoE.Table, BootID: snapshot.ValidUntil.BootID, ExpiresNanos: snapshot.ValidUntil.Nanos})
					}
				}
			}
		case "ra_client":
			var snapshot raclient.Snapshot
			if has {
				if err := json.Unmarshal(record.Snapshot, &snapshot); err != nil {
					return p, err
				}
			}
			if err := projectRAClient(&p, s, snapshot, has, facts.bfdClock); err != nil {
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
			if !dhcp4.CurrentLease(snap, facts.bfdClock) {
				continue
			}
			a, err := netip.ParseAddr(snap.CurrentAddress)
			if err != nil || !a.Is4() || a.IsUnspecified() || a.IsMulticast() || snap.PrefixLength < 0 || snap.PrefixLength > 32 {
				return p, errors.New("invalid active DHCPv4 address")
			}
			prefix := netip.PrefixFrom(a, snap.PrefixLength)
			if err := addAddressWithOptions(&p.network, s.Interface(), prefix.String(), network.AddressOptions{BootLifetime: network.BootLifetime{BootID: snap.BootID, PreferredNanos: snap.LeaseClock.ExpiresNanos, ValidNanos: snap.LeaseClock.ExpiresNanos}}); err != nil {
				return p, err
			}
			if (s.DHCP4.DefaultRoute == nil || *s.DHCP4.DefaultRoute) && snap.DefaultGateway != "" {
				gw, err := netip.ParseAddr(snap.DefaultGateway)
				if err != nil || !gw.Is4() || gw.IsUnspecified() || gw.IsMulticast() {
					return p, errors.New("invalid DHCPv4 gateway")
				}
				p.network.Routes = append(p.network.Routes, network.Route{Destination: "0.0.0.0/0", Gateway: gw.String(), Interface: s.Interface(), Metric: s.DHCP4.Metric, Table: s.DHCP4.Table})
			}
		case "dhcp6":
			for _, lan := range s.DHCP6.Delegations {
				ensureInterface(&p.network, lan.Interface)
			}
			if !has {
				continue
			}
			var snap dhcp6config.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snap); err != nil {
				return p, err
			}
			for _, address := range snap.Addresses {
				if !address.ValidUntil.Live(facts.bfdClock) {
					continue
				}
				if err := addAddressWithOptions(&p.network, s.DHCP6.Interface, address.Address+"/128", addressLifetimes(address.PreferredUntil, address.ValidUntil)); err != nil {
					return p, err
				}
			}
			for _, lan := range s.DHCP6.Delegations {
				var prefixes []ra.Prefix
				for _, lease := range snap.Prefixes {
					if !lease.ValidUntil.Live(facts.bfdClock) {
						continue
					}
					prefix, address, err := delegated(lease.Prefix, lan.SubnetID, lan.HostID)
					if err != nil {
						return p, fmt.Errorf("%s/%s: %w", s.Name, lan.Name, err)
					}
					if err := addAddressWithOptions(&p.network, lan.Interface, address.String(), addressLifetimes(lease.PreferredUntil, lease.ValidUntil)); err != nil {
						return p, err
					}
					prefixes = append(prefixes, ra.Prefix{Prefix: prefix.String(), OnLink: true, Autonomous: true, ValidLifetimeSeconds: ^uint32(0), PreferredLifetimeSeconds: ^uint32(0), ValidUntil: lease.ValidUntil, PreferredUntil: lease.PreferredUntil})
				}
				if !lan.Advertise || len(prefixes) == 0 {
					continue
				}
				name := s.Name + "-" + lan.Name + "-ra"
				module := "service." + name
				c := protocols.lanRA[module]
				c.Prefixes = prefixes
				c.Authority, _ = snap.LeaseUntil()
				if lan.UseLearnedDNS {
					if until, usable := snap.InformationUntil(facts.bfdClock); usable {
						for _, server := range unique(snap.DNSServers) {
							address, _ := netip.ParseAddr(server)
							if address.IsLinkLocalUnicast() && lan.Interface != s.Interface() {
								continue // The WAN link-local scope is not the LAN link.
							}
							c.LearnedRDNSS = append(c.LearnedRDNSS, ra.DNSServer{Address: server, ValidUntil: until})
						}
						for _, name := range unique(snap.DomainSearch) {
							c.LearnedDNSSL = append(c.LearnedDNSSL, ra.SearchDomain{Domain: name, ValidUntil: until})
						}
					}
				}
				if err := ra.Validate(c); err != nil {
					return p, err
				}
				p.liveRA[module] = c
				p.instances = append(p.instances, protocols.instances[module])
			}
		case "vrrp":
			if !has {
				continue
			}
			var snap vrrp.Snapshot
			if err := json.Unmarshal(record.Snapshot, &snap); err != nil {
				return p, err
			}
			c := protocols.vrrp[s.ModuleName()]
			if c.GracefulActivation != nil {
				until, allowed := facts.vrrpActivation[s.ModuleName()]
				if !allowed || !vrrp.CurrentElection(snap, facts.bfdClock) {
					continue
				}
				if until != 0 && (p.activationUntil == 0 || until < p.activationUntil) {
					p.activationUntil = until
				}
			} else if !vrrp.CurrentOwnership(snap, facts.bfdClock) {
				continue
			}
			p.vrrpOwners[s.ModuleName()] = ownershipFor(record, snap)
			if record.ConfigDigest == protocols.instances[s.ModuleName()].digest && vrrp.CurrentOwnership(snap, facts.bfdClock) {
				for i, bound := range p.instances {
					if ha, paired := protocols.dhcpHA[bound.instance.Name]; paired && ha.owner == s.ModuleName() {
						p.instances[i] = ha.serving
					}
				}
			}
			p.network.Links = append(p.network.Links, network.Link{Authority: lifetime.Deadline{BootID: snap.BootID, Nanos: snap.ExpiresNanos}, Name: c.VIPInterface, Kind: "macvlan", Parent: c.Interface, MACVLANMode: "bridge", HardwareAddr: vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String(), IPv6AddrGenMode: "none"})
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
				if err := addAddressWithOptions(&p.network, c.VIPInterface, address, network.AddressOptions{NoDAD: c.Family == "ipv6", BootLifetime: network.BootLifetime{BootID: snap.BootID, PreferredNanos: snap.ExpiresNanos, ValidNanos: snap.ExpiresNanos}}); err != nil {
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
	if err := projectBGP(&p, doc, protocols.bgp, records, now, facts.bfdClock); err != nil {
		return p, err
	}
	if facts.aftr != nil {
		if err := projectDSLite(&p, doc, protocols, records, facts.aftr, facts.bfdClock); err != nil {
			return p, err
		}
	}
	sort.Slice(p.instances, func(i, j int) bool { return p.instances[i].instance.Name < p.instances[j].instance.Name })
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
func delegated(prefix string, subnet, host uint64) (netip.Prefix, netip.Prefix, error) {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is6() || p.Addr().Is4In6() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() || p.Bits() > 64 || p != p.Masked() {
		return netip.Prefix{}, netip.Prefix{}, errors.New("delegated prefix must be a canonical IPv6 prefix no longer than /64")
	}
	bits := 64 - p.Bits()
	if bits < 64 && subnet >= uint64(1)<<bits {
		return netip.Prefix{}, netip.Prefix{}, errors.New("subnet_id does not fit acquired prefix")
	}
	b := p.Addr().As16()
	binary.BigEndian.PutUint64(b[:8], binary.BigEndian.Uint64(b[:8])|subnet)
	base := netip.AddrFrom16(b)
	if host == 0 {
		host = 1
	}
	binary.BigEndian.PutUint64(b[8:], host)
	return netip.PrefixFrom(base, 64), netip.PrefixFrom(netip.AddrFrom16(b), 64), nil
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

func addressLifetimes(preferred, valid lifetime.Deadline) network.AddressOptions {
	if valid.BootID != "" {
		p, v := preferred.Nanos, valid.Nanos
		if p == 0 {
			p = network.InfiniteNanos
		}
		if v == 0 {
			v = network.InfiniteNanos
		}
		return network.AddressOptions{BootLifetime: network.BootLifetime{BootID: valid.BootID, PreferredNanos: p, ValidNanos: v}}
	}
	return network.AddressOptions{PreferredUntil: preferred.UTC, ValidUntil: valid.UTC}
}
