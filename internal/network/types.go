// Package network reconciles a deliberately small, typed Linux network model.
// It does not execute ip, nft, sysctl, or any other external command.
package network

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

const (
	// FirewallTable is the only nftables table this package may modify.
	FirewallTable = "routerd2"
	// RouteProtocol labels our routes. The journal, not this value alone, is
	// required to establish ownership; other routing software can use it too.
	RouteProtocol = 242
)

type Desired struct {
	Interfaces []Interface `json:"interfaces,omitempty"`
	Routes     []Route     `json:"routes,omitempty"`
	Forwarding Forwarding  `json:"forwarding,omitempty"`
	Firewall   *Firewall   `json:"firewall,omitempty"`
	DSLite     []DSLite    `json:"ds_lite,omitempty"`
}

// Interface brings an existing link up. MTU zero leaves its MTU unchanged.
// Addresses are host/prefix values (for example 192.0.2.1/24, not the subnet).
// Only addresses created and recorded by this engine are ever removed.
type Interface struct {
	Name      string   `json:"name"`
	MTU       int      `json:"mtu,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface"`
	// Metric zero uses Linux's default: 0 for IPv4, 1024 for IPv6.
	Metric int `json:"metric,omitempty"`
	// Table zero means the main table (254). Policy routing rules are not yet
	// part of this API; using another table does not install an ip rule.
	Table int `json:"table,omitempty"`
}

// Forwarding is opt-in. Nil leaves an unmanaged sysctl unchanged. When a setting
// previously changed by this engine becomes nil, its recorded original value is
// restored, unless another actor changed the live value in the meantime.
type Forwarding struct {
	IPv4 *bool `json:"ipv4,omitempty"`
	IPv6 *bool `json:"ipv6,omitempty"`
}

// Firewall manages the routerd2 inet table. Its forward base chain has policy
// drop, accepts established/related traffic, and permits the explicit pairs.
// It installs no input/output filter, so this is not a management-plane ACL.
// Nil removes a previously owned table, but never removes an unowned table.
type Firewall struct {
	Forward    []Forward    `json:"forward,omitempty"`
	Masquerade []Masquerade `json:"masquerade,omitempty"`
}

type Forward struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Masquerade is explicitly scoped to a source prefix and egress interface.
// IPv4 and IPv6 prefixes are supported; IPv6 NAT is never implicitly enabled.
type Masquerade struct {
	Source       string `json:"source"`
	OutInterface string `json:"out_interface"`
}

// DSLite creates an IPv4-in-IPv6 Linux ip6tnl device with fixed IPv6 endpoints.
// It does not discover an AFTR, acquire IPv6, install routes, or perform NAT.
// Configure the B4 IPv4 address through Interfaces and routes through Routes.
// MTU zero derives the inner MTU from the underlay MTU minus 40 bytes.
type DSLite struct {
	Name     string `json:"name"`
	Underlay string `json:"underlay"`
	Local    string `json:"local"`
	Remote   string `json:"remote"`
	MTU      int    `json:"mtu,omitempty"`
}

type Options struct {
	// StatePath must be an absolute, persistent path owned by the caller.
	// Losing this journal does not authorize adoption of existing resources.
	StatePath string
}

type AddressState struct {
	Address string `json:"address"`
	Flags   int    `json:"flags,omitempty"`
	Owned   bool   `json:"owned"`
}

type InterfaceState struct {
	Name      string         `json:"name"`
	Index     int            `json:"index"`
	Kind      string         `json:"kind"`
	MTU       int            `json:"mtu"`
	Up        bool           `json:"up"`
	Alias     string         `json:"alias,omitempty"`
	Addresses []AddressState `json:"addresses,omitempty"`
	DSLite    *DSLite        `json:"ds_lite,omitempty"`
}

type RouteState struct {
	Route
	Protocol  int    `json:"protocol"`
	Type      int    `json:"type"`
	Source    string `json:"source,omitempty"`
	Multipath bool   `json:"multipath,omitempty"`
	Advanced  bool   `json:"advanced,omitempty"`
	Owned     bool   `json:"owned"`
}

type ForwardingState struct {
	IPv4 bool `json:"ipv4"`
	IPv6 bool `json:"ipv6"`
}

type FirewallState struct {
	Table   string `json:"table"`
	Present bool   `json:"present"`
	Owned   bool   `json:"owned"`
}

type Snapshot struct {
	Revision          uint64           `json:"revision"`
	AttemptedRevision uint64           `json:"attempted_revision"`
	Status            string           `json:"status"`
	Desired           Desired          `json:"desired"`
	LastError         string           `json:"last_error,omitempty"`
	Interfaces        []InterfaceState `json:"interfaces,omitempty"`
	Routes            []RouteState     `json:"routes,omitempty"`
	Forwarding        ForwardingState  `json:"forwarding"`
	Firewall          FirewallState    `json:"firewall"`
	ObservationErrors []string         `json:"observation_errors,omitempty"`
}

// ApplyError reports partial application. Netlink, sysctl and nftables are not
// one transaction. Only each nftables table replacement is atomic. A caller may
// reconcile its previous Desired, even with a lower revision, to roll back.
type ApplyError struct {
	Stage   string
	Partial bool
	Err     error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("network %s: %v (partial=%t; applied changes are retained)", e.Stage, e.Err, e.Partial)
}
func (e *ApplyError) Unwrap() error { return e.Err }

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

func validName(s string) bool { return interfaceName.MatchString(s) && s != "." && s != ".." }

// Validate is side-effect free and does not require Linux or privileges.
func Validate(d Desired) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	links := map[string]Interface{}
	for _, l := range d.Interfaces {
		if !validName(l.Name) {
			add("invalid interface name %q", l.Name)
		}
		if _, ok := links[l.Name]; ok {
			add("duplicate interface %q", l.Name)
		}
		links[l.Name] = l
		if l.MTU != 0 && (l.MTU < 68 || l.MTU > 65535) {
			add("interface %s: MTU must be 68..65535 or zero", l.Name)
		}
		seen := map[string]bool{}
		for _, s := range l.Addresses {
			p, err := netip.ParsePrefix(s)
			if err != nil || !p.IsValid() || p.Addr().Is4In6() || p.Addr().IsUnspecified() || p.Addr().IsMulticast() {
				add("interface %s: invalid unicast host/prefix %q", l.Name, s)
				continue
			}
			if p.Addr().Is6() && l.MTU != 0 && l.MTU < 1280 {
				add("interface %s: IPv6 requires MTU >= 1280", l.Name)
			}
			if seen[p.String()] {
				add("interface %s: duplicate address %s", l.Name, p)
			}
			seen[p.String()] = true
		}
	}
	seenRoutes := map[string]bool{}
	for _, r := range d.Routes {
		p, err := netip.ParsePrefix(r.Destination)
		if err != nil || !p.IsValid() || p.Addr().Is4In6() {
			add("route: invalid destination %q", r.Destination)
			continue
		}
		if p != p.Masked() {
			add("route %s: destination must be a network prefix", r.Destination)
		}
		if !validName(r.Interface) {
			add("route %s: invalid interface %q", r.Destination, r.Interface)
		}
		if r.Metric < 0 || uint64(r.Metric) > uint64(^uint32(0)) {
			add("route %s: invalid metric", r.Destination)
		}
		if r.Table < 0 || uint64(r.Table) > uint64(^uint32(0)) || r.Table == 255 {
			add("route %s: invalid/reserved route table", r.Destination)
		}
		if r.Gateway != "" {
			g, err := netip.ParseAddr(r.Gateway)
			if err != nil || !g.IsValid() || g.Is4In6() || g.Zone() != "" || g.IsUnspecified() || g.IsMulticast() || g.Is4() != p.Addr().Is4() {
				add("route %s: gateway must be a same-family unicast address", r.Destination)
			}
		}
		key := routeSlot(r)
		if seenRoutes[key] {
			add("route %s: duplicate destination/table/metric; ECMP is not implemented", r.Destination)
		}
		seenRoutes[key] = true
	}
	seenTunnels := map[string]bool{}
	for _, t := range d.DSLite {
		if !validName(t.Name) || !validName(t.Underlay) || t.Name == t.Underlay {
			add("DS-Lite %q: invalid name or underlay", t.Name)
		}
		if seenTunnels[t.Name] {
			add("duplicate DS-Lite tunnel %q", t.Name)
		}
		seenTunnels[t.Name] = true
		if t.MTU != 0 && (t.MTU < 68 || t.MTU > 65495) {
			add("DS-Lite %s: invalid MTU", t.Name)
		}
		for _, endpoint := range []string{t.Local, t.Remote} {
			a, err := netip.ParseAddr(endpoint)
			if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() {
				add("DS-Lite %s: endpoint %q must be a non-link-local IPv6 unicast address", t.Name, endpoint)
			}
		}
		if t.Local == t.Remote {
			add("DS-Lite %s: local and remote must differ", t.Name)
		}
		if l, ok := links[t.Name]; ok && l.MTU != 0 && t.MTU != 0 && l.MTU != t.MTU {
			add("DS-Lite %s: conflicting interface/tunnel MTUs", t.Name)
		}
	}
	for _, t := range d.DSLite {
		if seenTunnels[t.Underlay] {
			add("DS-Lite %s: nested DS-Lite underlays are not supported", t.Name)
		}
	}
	if d.Firewall != nil {
		seen := map[Forward]bool{}
		for _, f := range d.Firewall.Forward {
			if !validName(f.From) || !validName(f.To) || f.From == f.To {
				add("firewall: invalid forwarding pair %q -> %q", f.From, f.To)
			}
			if seen[f] {
				add("firewall: duplicate forwarding pair %q -> %q", f.From, f.To)
			}
			seen[f] = true
		}
		nats := map[Masquerade]bool{}
		for _, n := range d.Firewall.Masquerade {
			p, err := netip.ParsePrefix(n.Source)
			if err != nil || !p.IsValid() || p.Addr().Is4In6() || p != p.Masked() {
				add("masquerade: invalid source network %q", n.Source)
			}
			if !validName(n.OutInterface) {
				add("masquerade: invalid output interface %q", n.OutInterface)
			}
			if nats[n] {
				add("masquerade: duplicate rule for %q", n.Source)
			}
			nats[n] = true
		}
	}
	return errors.Join(errs...)
}

func routeTable(r Route) int {
	if r.Table == 0 {
		return 254
	}
	return r.Table
}
func routeSlot(r Route) string {
	p, err := netip.ParsePrefix(r.Destination)
	if err == nil {
		r.Destination = p.Masked().String()
		if p.Addr().Is6() && r.Metric == 0 {
			r.Metric = 1024
		}
	}
	return fmt.Sprintf("%s/%d/%d", r.Destination, routeTable(r), r.Metric)
}
func addressIP(s string) string {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return ""
	}
	return p.Addr().String()
}

func normalized(d Desired) Desired {
	for i := range d.Interfaces {
		for j, a := range d.Interfaces[i].Addresses {
			p, _ := netip.ParsePrefix(a)
			d.Interfaces[i].Addresses[j] = p.String()
		}
	}
	for i := range d.Routes {
		r := &d.Routes[i]
		p, _ := netip.ParsePrefix(r.Destination)
		r.Destination = p.Masked().String()
		r.Table = routeTable(*r)
		if p.Addr().Is6() && r.Metric == 0 {
			r.Metric = 1024
		}
		if r.Gateway != "" {
			g, _ := netip.ParseAddr(r.Gateway)
			r.Gateway = g.String()
		}
	}
	for i := range d.DSLite {
		t := &d.DSLite[i]
		l, _ := netip.ParseAddr(t.Local)
		r, _ := netip.ParseAddr(t.Remote)
		t.Local, t.Remote = l.String(), r.String()
	}
	if d.Firewall != nil {
		for i := range d.Firewall.Masquerade {
			p, _ := netip.ParsePrefix(d.Firewall.Masquerade[i].Source)
			d.Firewall.Masquerade[i].Source = p.Masked().String()
		}
	}
	return d
}
