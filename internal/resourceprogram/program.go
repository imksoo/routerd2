// Package resourceprogram compiles durable resource declarations into the
// typed intent consumed by the kernel and protocol workers. Compilation is
// side-effect free: it does not inspect the host, read secrets, or launch work.
package resourceprogram

import (
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

const SchemaVersion = 2

// Program is the operator's source of truth. Generated worker configurations
// and kernel projections are derived values, never an alternative input format.
type Program struct {
	SchemaVersion int        `json:"schema_version"`
	Resources     []Resource `json:"resources"`
}

// Provenance records where a declaration came from. It grants no authority and
// is not interpreted as a path to read or an executable to invoke.
type Provenance struct {
	Source   string `json:"source,omitempty"`
	Resource string `json:"resource,omitempty"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Resource has one typed payload. IDs are stable operator identities, separate
// from an interface name or a generated worker's private instance name. Owner
// defaults to ID; another owner must name a resource in this same program.
// Dependencies constrain compilation order, not future protocol readiness.
type Resource struct {
	ID         string      `json:"id"`
	DependsOn  []string    `json:"depends_on,omitempty"`
	Owner      string      `json:"owner,omitempty"`
	Provenance *Provenance `json:"provenance,omitempty"`

	Interface  *network.Interface     `json:"interface,omitempty"`
	Address    *Address               `json:"address,omitempty"`
	Link       *network.Link          `json:"link,omitempty"`
	Route      *network.Route         `json:"route,omitempty"`
	PolicyRule *network.PolicyRule    `json:"policy_rule,omitempty"`
	Forwarding *network.Forwarding    `json:"forwarding,omitempty"`
	Firewall   *network.Firewall      `json:"firewall,omitempty"`
	DSLite     *network.DSLite        `json:"ds_lite,omitempty"`
	VRRPGuard  *network.VRRPGuard     `json:"vrrp_guard,omitempty"`
	WireGuard  *network.WireGuard     `json:"wireguard,omitempty"`
	Service    *serviceconfig.Service `json:"service,omitempty"`
}

// Address owns one host/prefix and its options independently of interface
// settings and other addresses on the same link. An omitted Interface resource
// means only the minimal administrative-UP interface projection is synthesized.
type Address struct {
	Interface string                  `json:"interface"`
	CIDR      string                  `json:"cidr"`
	Options   *network.AddressOptions `json:"options,omitempty"`
}

// Native is an internal engine input, not a serialized configuration accepted
// by the configuration API. It separates source declarations from projections.
type Native struct {
	Network  network.Desired         `json:"network"`
	Services []serviceconfig.Service `json:"services,omitempty"`
}

type Plan struct {
	Native       Native              `json:"native"`
	Order        []string            `json:"order"`
	Dependencies map[string][]string `json:"dependencies"`
	Ownership    []Ownership         `json:"ownership"`
}

// Ownership attributes an exact output to its declaration and lifecycle owner.
// The network worker's existing host journal remains the authority for deciding
// whether a live kernel object may be removed; this table cannot adopt objects.
type Ownership struct {
	ResourceID string      `json:"resource_id"`
	OwnerID    string      `json:"owner_id"`
	Kind       string      `json:"kind"`
	Key        string      `json:"key"`
	Provenance *Provenance `json:"provenance,omitempty"`
}

func (r Resource) Kind() string {
	kind, count := r.kind()
	if count != 1 {
		return ""
	}
	return kind
}

func (r Resource) kind() (string, int) {
	kind, count := "", 0
	for _, v := range []struct {
		name string
		set  bool
	}{
		{"interface", r.Interface != nil}, {"address", r.Address != nil},
		{"link", r.Link != nil}, {"route", r.Route != nil},
		{"policy_rule", r.PolicyRule != nil}, {"forwarding", r.Forwarding != nil},
		{"firewall", r.Firewall != nil}, {"ds_lite", r.DSLite != nil},
		{"vrrp_guard", r.VRRPGuard != nil}, {"wireguard", r.WireGuard != nil},
		{"service", r.Service != nil},
	} {
		if v.set {
			kind, count = v.name, count+1
		}
	}
	return kind, count
}
