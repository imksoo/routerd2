package network

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/imksoo/routerd2/internal/boottime"
)

// Link creates a virtual interface. Interface configures its administrative
// state, addresses, MTU and bridge/VRF membership. Parent links are created first.
// Fields belonging to a different kind are rejected, never silently ignored.
// Identity fields (kind, parent, IDs, VXLAN port and macvlan mode) cannot change
// in place: remove the old declaration and apply before creating its replacement.
type Link struct {
	// Internal original authority. The writer fences this owned link DOWN at
	// expiry; these fields describe no kernel link attribute.
	BootID       string `json:"boot_id,omitempty"`
	ExpiresNanos uint64 `json:"expires_nanos,omitempty"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Parent       string `json:"parent,omitempty"`
	Alias        string `json:"alias,omitempty"`
	HardwareAddr string `json:"hardware_addr,omitempty"`
	// IPv6AddrGenMode is applied before bringing a new link up. Empty preserves
	// the kernel default; none disables automatic link-local generation so HA
	// can install its explicit virtual link-local address without a second one.
	IPv6AddrGenMode         string `json:"ipv6_addr_gen_mode,omitempty"`
	VLANID                  int    `json:"vlan_id,omitempty"`
	VRFTable                int    `json:"vrf_table,omitempty"`
	VXLANID                 int    `json:"vxlan_id,omitempty"`
	Local                   string `json:"local,omitempty"`
	Remote                  string `json:"remote,omitempty"`
	Group                   string `json:"group,omitempty"`
	Port                    int    `json:"port,omitempty"`
	TTL                     int    `json:"ttl,omitempty"`
	Learning                *bool  `json:"learning,omitempty"`
	MACVLANMode             string `json:"macvlan_mode,omitempty"`
	BridgeVLANFiltering     *bool  `json:"bridge_vlan_filtering,omitempty"`
	BridgeMulticastSnooping *bool  `json:"bridge_multicast_snooping,omitempty"`
}

type linkAnnouncement struct {
	Index        int
	MAC          net.HardwareAddr
	BootID       string
	ExpiresNanos uint64
}

const linkAliasPrefix = "routerd2:link:v1:"

func validAlias(s string, maximum int) bool {
	return len(s) <= maximum && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}

func validateLinks(d Desired) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	created := map[string]Link{}
	for _, l := range d.Links {
		if (l.BootID != "" || l.ExpiresNanos != 0) && (!boottime.ValidBootID(l.BootID) || l.ExpiresNanos == 0 || l.ExpiresNanos > math.MaxInt64) {
			add("link %s: invalid original boot deadline", l.Name)
		}
		if !validName(l.Name) || l.Name == "lo" {
			add("link %q: invalid virtual interface name", l.Name)
		}
		if _, ok := created[l.Name]; ok {
			add("duplicate created link %q", l.Name)
		}
		created[l.Name] = l
		if l.IPv6AddrGenMode != "" && l.IPv6AddrGenMode != "none" && l.IPv6AddrGenMode != "eui64" {
			add("link %s: ipv6_addr_gen_mode must be none or eui64", l.Name)
		}
		if !validAlias(l.Alias, 192) {
			add("link %s: alias must be UTF-8, at most 192 bytes and contain no NUL or newlines", l.Name)
		}
		if l.HardwareAddr != "" {
			a, err := net.ParseMAC(l.HardwareAddr)
			if err != nil || len(a) != 6 || a[0]&1 != 0 || bytesAllZero(a) {
				add("link %s: hardware_addr must be a nonzero, unicast, 48-bit MAC address", l.Name)
			}
		}
		if l.Kind != "vlan" && l.VLANID != 0 {
			add("link %s: vlan_id is valid only for vlan", l.Name)
		}
		if l.Kind != "vrf" && l.VRFTable != 0 {
			add("link %s: vrf_table is valid only for vrf", l.Name)
		}
		if l.Kind != "vxlan" && (l.VXLANID != 0 || l.Local != "" || l.Remote != "" || l.Group != "" || l.Port != 0 || l.TTL != 0 || l.Learning != nil) {
			add("link %s: VXLAN options require kind vxlan", l.Name)
		}
		if l.Kind != "macvlan" && l.MACVLANMode != "" {
			add("link %s: macvlan_mode requires kind macvlan", l.Name)
		}
		if l.Kind != "bridge" && (l.BridgeVLANFiltering != nil || l.BridgeMulticastSnooping != nil) {
			add("link %s: bridge options require kind bridge", l.Name)
		}
		switch l.Kind {
		case "dummy":
			if l.Parent != "" {
				add("dummy %s: parent is not supported", l.Name)
			}
		case "wireguard":
			if l.Parent != "" || l.HardwareAddr != "" {
				add("wireguard %s: parent and hardware_addr are not supported", l.Name)
			}
		case "bridge":
			if l.Parent != "" {
				add("bridge %s: use Interface.Master for membership, not Parent", l.Name)
			}
		case "vrf":
			if l.Parent != "" || l.HardwareAddr != "" {
				add("VRF %s: parent and hardware_addr are not supported", l.Name)
			}
			if l.VRFTable < 1 || uint64(l.VRFTable) > uint64(^uint32(0)) || l.VRFTable >= 253 && l.VRFTable <= 255 {
				add("VRF %s: vrf_table must be a non-reserved routing table", l.Name)
			}
		case "vlan", "vxlan", "macvlan":
			if !validName(l.Parent) || l.Parent == l.Name {
				add("link %s: %s requires a valid, distinct parent", l.Name, l.Kind)
			}
			if l.Kind == "vlan" && (l.VLANID < 1 || l.VLANID > 4094) {
				add("VLAN %s: vlan_id must be 1..4094 (802.1Q)", l.Name)
			}
			if l.Kind == "macvlan" && l.MACVLANMode != "" && l.MACVLANMode != "bridge" && l.MACVLANMode != "private" {
				add("macvlan %s: macvlan_mode must be bridge or private", l.Name)
			}
			if l.Kind == "vxlan" {
				if l.VXLANID < 1 || l.VXLANID > 16777215 {
					add("VXLAN %s: vxlan_id must be 1..16777215", l.Name)
				}
				if l.Port < 0 || l.Port > 65535 || l.TTL < 0 || l.TTL > 255 {
					add("VXLAN %s: invalid UDP port or TTL", l.Name)
				}
				if l.Remote != "" && l.Group != "" {
					add("VXLAN %s: remote and group are mutually exclusive", l.Name)
				}
				var family int
				for _, endpoint := range []struct {
					name, value string
					multicast   bool
				}{{"local", l.Local, false}, {"remote", l.Remote, false}, {"group", l.Group, true}} {
					if endpoint.value == "" {
						continue
					}
					a, err := netip.ParseAddr(endpoint.value)
					if err != nil || a.Is4In6() || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() != endpoint.multicast {
						add("VXLAN %s: invalid %s endpoint %q", l.Name, endpoint.name, endpoint.value)
						continue
					}
					if family != 0 && family != a.BitLen() {
						add("VXLAN %s: endpoints must use the same address family", l.Name)
					}
					family = a.BitLen()
				}
			}
		default:
			add("link %s: unsupported kind %q", l.Name, l.Kind)
		}
	}
	for _, t := range d.DSLite {
		if _, ok := created[t.Name]; ok {
			add("link %s: cannot be both a Link and a DS-Lite tunnel", t.Name)
		}
	}
	for _, i := range d.Interfaces {
		if i.Master != nil {
			if *i.Master != "" && (!validName(*i.Master) || *i.Master == i.Name) {
				add("interface %s: invalid master", i.Name)
			}
			if m, ok := created[*i.Master]; ok && m.Kind != "bridge" && m.Kind != "vrf" {
				add("interface %s: master %s must be a bridge or VRF", i.Name, m.Name)
			}
		}
		if i.Alias != nil {
			if !validAlias(*i.Alias, 255) || strings.HasPrefix(*i.Alias, "routerd2:") {
				add("interface %s: invalid or reserved alias", i.Name)
			}
			if _, ok := created[i.Name]; ok {
				add("interface %s: use Link.Alias for created links", i.Name)
			}
			for _, t := range d.DSLite {
				if t.Name == i.Name {
					add("interface %s: DS-Lite ownership alias cannot be overridden", i.Name)
				}
			}
		}
	}
	// A parent/member dependency cycle would otherwise fail part way through a
	// candidate. Catch cycles involving existing interfaces as well as new links.
	edges := map[string][]string{}
	for _, l := range d.Links {
		if l.Parent != "" {
			edges[l.Name] = append(edges[l.Name], l.Parent)
		}
	}
	for _, i := range d.Interfaces {
		if i.Master != nil && *i.Master != "" {
			edges[i.Name] = append(edges[i.Name], *i.Master)
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) bool
	visit = func(n string) bool {
		if visiting[n] {
			return true
		}
		if visited[n] {
			return false
		}
		visiting[n] = true
		for _, p := range edges[n] {
			if visit(p) {
				return true
			}
		}
		visiting[n] = false
		visited[n] = true
		return false
	}
	for n := range edges {
		if visit(n) {
			add("link dependency cycle involving %s", n)
			break
		}
	}
	return errors.Join(errs...)
}

func bytesAllZero(a []byte) bool {
	for _, b := range a {
		if b != 0 {
			return false
		}
	}
	return true
}
func boolPointer(v bool) *bool { return &v }

func normalizeLink(l Link) Link {
	if l.HardwareAddr != "" {
		a, _ := net.ParseMAC(l.HardwareAddr)
		l.HardwareAddr = a.String()
	}
	if l.Kind == "macvlan" && l.MACVLANMode == "" {
		l.MACVLANMode = "bridge"
	}
	if l.Kind == "bridge" {
		if l.BridgeVLANFiltering == nil {
			l.BridgeVLANFiltering = boolPointer(false)
		}
		if l.BridgeMulticastSnooping == nil {
			l.BridgeMulticastSnooping = boolPointer(true)
		}
	}
	if l.Kind == "vxlan" {
		if l.Port == 0 {
			l.Port = 4789
		}
		if l.TTL == 0 {
			l.TTL = 64
		}
		if l.Learning == nil {
			l.Learning = boolPointer(true)
		}
		for _, p := range []*string{&l.Local, &l.Remote, &l.Group} {
			if *p != "" {
				a, _ := netip.ParseAddr(*p)
				*p = a.String()
			}
		}
	}
	return l
}

func sameLinkIdentity(a, b Link) bool {
	return a.Name == b.Name && a.Kind == b.Kind && a.Parent == b.Parent && a.VLANID == b.VLANID && a.VRFTable == b.VRFTable && a.VXLANID == b.VXLANID && a.Port == b.Port && a.MACVLANMode == b.MACVLANMode && (a.Kind != "vxlan" || vxlanFamily(a) == vxlanFamily(b))
}

func vxlanFamily(l Link) int {
	for _, s := range []string{l.Local, l.Remote, l.Group} {
		if a, err := netip.ParseAddr(s); err == nil && a.Is6() {
			return 6
		}
	}
	return 4
}

func sameManagedLink(live, want Link) bool {
	live.BootID, want.BootID = "", ""
	live.ExpiresNanos, want.ExpiresNanos = 0, 0
	if want.HardwareAddr == "" {
		live.HardwareAddr = ""
	}
	if want.IPv6AddrGenMode == "" {
		live.IPv6AddrGenMode = ""
	}
	return reflect.DeepEqual(live, normalizeLink(want))
}

// orderedLinks retains declaration order between independent devices.
func orderedLinks(links []Link) []Link {
	byName := map[string]Link{}
	for _, l := range links {
		byName[l.Name] = l
	}
	seen := map[string]bool{}
	out := make([]Link, 0, len(links))
	var visit func(Link)
	visit = func(l Link) {
		if seen[l.Name] {
			return
		}
		seen[l.Name] = true
		if p, ok := byName[l.Parent]; ok {
			visit(p)
		}
		out = append(out, l)
	}
	for _, l := range links {
		visit(l)
	}
	return out
}

func virtualLinkAlias(token string, l Link) string { return linkAliasPrefix + token + ":" + l.Alias }
func userLinkAlias(raw string) string {
	if strings.HasPrefix(raw, linkAliasPrefix) {
		tail := strings.TrimPrefix(raw, linkAliasPrefix)
		if len(tail) >= 33 && tail[32] == ':' {
			return tail[33:]
		}
	}
	return raw
}
