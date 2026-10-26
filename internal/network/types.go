// Package network reconciles a deliberately small, typed Linux network model.
// It does not execute ip, nft, sysctl, or any other external command.
package network

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/imksoo/routerd2/internal/lifetime"
)

const (
	// FirewallTable is the primary owned nftables table. HA receive guards have
	// their own separately journaled table, VRRPGuardTable.
	FirewallTable = "routerd2"
	// RouteProtocol labels our routes. The journal, not this value alone, is
	// required to establish ownership; other routing software can use it too.
	RouteProtocol = 242
)

type Desired struct {
	Links       []Link       `json:"links,omitempty"`
	Interfaces  []Interface  `json:"interfaces,omitempty"`
	Routes      []Route      `json:"routes,omitempty"`
	PolicyRules []PolicyRule `json:"policy_rules,omitempty"`
	Forwarding  Forwarding   `json:"forwarding,omitempty"`
	Firewall    *Firewall    `json:"firewall,omitempty"`
	VRRPGuards  []VRRPGuard  `json:"vrrp_guards,omitempty"`
	WireGuard   []WireGuard  `json:"wireguard,omitempty"`
}

// Interface brings an existing link up. MTU zero leaves its MTU unchanged.
// Addresses are host/prefix values (for example 192.0.2.1/24, not the subnet).
// Only addresses created and recorded by this engine are ever removed.
type Interface struct {
	Name      string   `json:"name"`
	MTU       int      `json:"mtu,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	// AddressOptions keys are entries in Addresses. Omitted options mean an
	// infinite static address with normal duplicate-address detection.
	AddressOptions map[string]AddressOptions `json:"address_options,omitempty"`
	// Master nil leaves membership unmanaged; an empty value detaches it.
	// Only bridge and VRF masters are supported. Previous membership is restored
	// when the field becomes nil, provided no other actor has changed it.
	Master *string `json:"master,omitempty"`
	// Alias is for preexisting interfaces. A created Link uses Link.Alias so its
	// ownership marker cannot be accidentally overwritten.
	Alias *string `json:"alias,omitempty"`
	// IPv6AcceptRA and IPv6Autoconf control the kernel's competing RA/SLAAC
	// writers. Nil leaves each setting unmanaged. A native RA client requests
	// accept_ra=0 and autoconf=false; 1/2 retain Linux accept_ra semantics.
	// Changed settings are restored only on the same interface instance.
	IPv6AcceptRA *int  `json:"ipv6_accept_ra,omitempty"`
	IPv6Autoconf *bool `json:"ipv6_autoconf,omitempty"`
}

type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"interface,omitempty"`
	// NextHops installs one weighted multipath route. It is mutually exclusive
	// with Gateway/Interface. A single next hop has no relative weight and is
	// normalized to a scalar route, matching Linux's IPv6 representation.
	NextHops []NextHop `json:"next_hops,omitempty"`
	// Metric zero uses Linux's default: 0 for IPv4, 1024 for IPv6.
	Metric int `json:"metric,omitempty"`
	// Table zero means the main table (254). Use PolicyRules to select a different
	// table; creating a route in a table does not install a policy rule implicitly.
	Table int `json:"table,omitempty"`
	// ValidUntil is the original IPv6 route lifetime: operator UTC or learned
	// boot scope. The writer installs a relative kernel timer. Very long finite
	// lifetimes retain their original deadline with a renewable kernel grant.
	ValidUntil lifetime.Deadline `json:"valid_until,omitzero"`
	// BootID/ExpiresNanos carry a protocol's original authority to the writer.
	// They are internal projection metadata, not kernel attributes. The network
	// worker removes expired routes independently of control and protocol workers.
	// An explicit IPv6 ValidUntil also retains its native kernel expiry; both
	// constraints apply when a conditional route has that absolute lifetime.
	BootID       string `json:"boot_id,omitempty"`
	ExpiresNanos uint64 `json:"expires_nanos,omitempty"`
}

// NextHop is an already resolved, directly reachable next hop. Weight is 1..256
// (Linux rtnh_hops + 1). Gateway may be empty for a directly connected path;
// Interface is always explicit. Recursive routing belongs to the control plane.
type NextHop struct {
	Gateway   string `json:"gateway,omitempty"`
	Interface string `json:"interface"`
	Weight    int    `json:"weight"`
}

// Forwarding is opt-in. Nil leaves an unmanaged sysctl unchanged. When a setting
// previously changed by this engine becomes nil, its recorded original value is
// restored, unless another actor changed the live value in the meantime.
type Forwarding struct {
	IPv4 *bool `json:"ipv4,omitempty"`
	IPv6 *bool `json:"ipv6,omitempty"`
	// IPv4RPFilter sets Linux conf/all and conf/default rp_filter. Zero disables
	// reverse-path filtering for policy-routed/asymmetric router paths. Existing
	// interfaces keep their individual setting; new PPP units inherit default.
	IPv4RPFilter *int `json:"ipv4_rp_filter,omitempty"`
}

// Firewall manages only the routerd2 inet table. A nil Firewall removes a
// previously owned table, but never removes an unowned table. Input and Output
// are opt-in; their nil values install no hooks. The legacy Forward pairs use
// a default-drop forward chain unless ForwardChain overrides its policy.
type Firewall struct {
	Forward      []Forward      `json:"forward,omitempty"`
	Masquerade   []Masquerade   `json:"masquerade,omitempty"`
	Input        *FirewallChain `json:"input,omitempty"`
	Output       *FirewallChain `json:"output,omitempty"`
	ForwardChain *FirewallChain `json:"forward_chain,omitempty"`
	// DisableForward installs no forward hook, for example for NAT-only use.
	// It cannot be combined with Forward pairs or ForwardChain.
	DisableForward bool      `json:"disable_forward,omitempty"`
	NAT            []NATRule `json:"nat,omitempty"`
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

type Options struct {
	// StatePath must be an absolute, persistent path owned by the caller.
	// Losing this journal does not authorize adoption of existing resources.
	StatePath string
}

type AddressState struct {
	Address           string `json:"address"`
	Peer              string `json:"peer,omitempty"`
	Flags             int    `json:"flags,omitempty"`
	Owned             bool   `json:"owned"`
	PreferredLifetime uint32 `json:"preferred_lifetime_seconds"`
	ValidLifetime     uint32 `json:"valid_lifetime_seconds"`
}

type InterfaceState struct {
	Name         string         `json:"name"`
	Index        int            `json:"index"`
	Kind         string         `json:"kind"`
	MTU          int            `json:"mtu"`
	Up           bool           `json:"up"`
	Alias        string         `json:"alias,omitempty"`
	HardwareAddr string         `json:"hardware_addr,omitempty"`
	Parent       string         `json:"parent,omitempty"`
	Master       string         `json:"master,omitempty"`
	MasterIndex  int            `json:"master_index,omitempty"`
	Link         *Link          `json:"link,omitempty"`
	Owned        bool           `json:"owned"`
	Addresses    []AddressState `json:"addresses,omitempty"`
	IPv6AcceptRA *int           `json:"ipv6_accept_ra,omitempty"`
	IPv6Autoconf *bool          `json:"ipv6_autoconf,omitempty"`
}

type RouteState struct {
	kernelExpiry routeTimer
	Route
	Protocol  int    `json:"protocol"`
	Type      int    `json:"type"`
	Source    string `json:"source,omitempty"`
	Multipath bool   `json:"multipath,omitempty"`
	Advanced  bool   `json:"advanced,omitempty"`
	Owned     bool   `json:"owned"`
}

type ForwardingState struct {
	IPv4                bool `json:"ipv4"`
	IPv6                bool `json:"ipv6"`
	IPv4RPFilterAll     int  `json:"ipv4_rp_filter_all"`
	IPv4RPFilterDefault int  `json:"ipv4_rp_filter_default"`
}

type FirewallState struct {
	Table   string `json:"table"`
	Present bool   `json:"present"`
	Owned   bool   `json:"owned"`
}

type Snapshot struct {
	Revision          uint64            `json:"revision"`
	AttemptedRevision uint64            `json:"attempted_revision"`
	Status            string            `json:"status"`
	Desired           Desired           `json:"desired"`
	LastError         string            `json:"last_error,omitempty"`
	Interfaces        []InterfaceState  `json:"interfaces,omitempty"`
	Routes            []RouteState      `json:"routes,omitempty"`
	PolicyRules       []PolicyRuleState `json:"policy_rules,omitempty"`
	Forwarding        ForwardingState   `json:"forwarding"`
	Firewall          FirewallState     `json:"firewall"`
	VRRPGuard         FirewallState     `json:"vrrp_guard"`
	WireGuard         []WireGuardState  `json:"wireguard,omitempty"`
	ObservationErrors []string          `json:"observation_errors,omitempty"`
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
	if v := d.Forwarding.IPv4RPFilter; v != nil && (*v < 0 || *v > 2) {
		errs = append(errs, errors.New("ipv4_rp_filter must be 0 (disabled), 1 (strict) or 2 (loose)"))
	}
	if err := validateLinks(d); err != nil {
		errs = append(errs, err)
	}
	if err := validateWireGuard(d); err != nil {
		errs = append(errs, err)
	}
	if err := validatePolicyRules(d.PolicyRules); err != nil {
		errs = append(errs, err)
	}
	if err := validateAddressOptions(d); err != nil {
		errs = append(errs, err)
	}
	if err := validateVRRPGuards(d); err != nil {
		errs = append(errs, err)
	}
	if err := validateFirewall(d.Firewall); err != nil {
		errs = append(errs, err)
	}
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
		if l.IPv6AcceptRA != nil && (*l.IPv6AcceptRA < 0 || *l.IPv6AcceptRA > 2) {
			add("interface %s: ipv6_accept_ra must be 0, 1 or 2", l.Name)
		}
		if (l.IPv6AcceptRA != nil || l.IPv6Autoconf != nil) && (l.Name == "all" || l.Name == "default") {
			add("interface %s: reserved IPv6 sysctl name", l.Name)
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
	if err := validateRoutes(d.Routes); err != nil {
		errs = append(errs, err)
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
	for i := range d.WireGuard {
		d.WireGuard[i] = normalizeWireGuard(d.WireGuard[i])
	}
	for i := range d.VRRPGuards {
		for j, s := range d.VRRPGuards[i].Addresses {
			a, _ := netip.ParseAddr(s)
			d.VRRPGuards[i].Addresses[j] = a.String()
		}
	}
	for i := range d.PolicyRules {
		d.PolicyRules[i] = normalizePolicyRule(d.PolicyRules[i])
	}
	for i := range d.Links {
		d.Links[i] = normalizeLink(d.Links[i])
	}
	for i := range d.Interfaces {
		if len(d.Interfaces[i].AddressOptions) > 0 {
			options := map[string]AddressOptions{}
			for address, value := range d.Interfaces[i].AddressOptions {
				p, _ := netip.ParsePrefix(address)
				options[p.String()] = normalizeAddressOptions(value)
			}
			d.Interfaces[i].AddressOptions = options
		}
		for j, a := range d.Interfaces[i].Addresses {
			p, _ := netip.ParsePrefix(a)
			d.Interfaces[i].Addresses[j] = p.String()
		}
	}
	for i := range d.Routes {
		d.Routes[i] = normalizeRoute(d.Routes[i])
	}
	if d.Firewall != nil {
		normalizeFirewall(d.Firewall)
		for i := range d.Firewall.Masquerade {
			p, _ := netip.ParsePrefix(d.Firewall.Masquerade[i].Source)
			d.Firewall.Masquerade[i].Source = p.Masked().String()
		}
	}
	return d
}
