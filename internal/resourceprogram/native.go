package resourceprogram

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/imksoo/routerd2/internal/network"
)

// FromNative constructs a program for trusted internal builders such as the
// original-routerd importer and tests. It is not a decoder for any previous
// routerd2 format. Compile still validates the result; invalid input is not
// repaired by dropping fields. No host state is read, and input is not mutated.
// Interface and service IDs are readable; identities containing IP literals
// use a short SHA-256 suffix. IDs do not depend on mutable route next hops.
func FromNative(native Native) Program {
	program := Program{SchemaVersion: SchemaVersion, Resources: []Resource{}}
	add := func(r Resource) { program.Resources = append(program.Resources, r) }
	for _, v := range native.DerivedEvents {
		spec := v.Spec
		add(Resource{ID: v.ID, DerivedEvent: &spec})
	}
	for _, rule := range native.EventRules {
		spec := rule.Spec
		add(Resource{ID: rule.ID, EventRule: &spec})
	}
	for _, original := range native.Network.Links {
		v := original
		add(Resource{ID: "link/" + v.Name, Link: &v})
	}
	for _, original := range native.Network.Interfaces {
		v := original
		v.Addresses = nil
		v.AddressOptions = nil
		optionKeys := map[string][]string{}
		for key := range original.AddressOptions {
			canonical := canonicalPrefix(key)
			optionKeys[canonical] = append(optionKeys[canonical], key)
		}
		usedOptions := map[string]bool{}
		previousAddress := ""
		for _, cidr := range original.Addresses {
			address := Address{Interface: v.Name, CIDR: cidr}
			if keys := optionKeys[canonicalPrefix(cidr)]; len(keys) == 1 {
				option := original.AddressOptions[keys[0]]
				address.Options = &option
				usedOptions[keys[0]] = true
			}
			id := "address/" + v.Name + "/" + identityHash(canonicalPrefix(cidr))
			r := Resource{ID: id, Address: &address}
			// Preserve the native builder's primary-address order explicitly;
			// resource list order alone is not an execution-order contract.
			if previousAddress != "" {
				r.DependsOn = []string{previousAddress}
			}
			add(r)
			previousAddress = id
		}
		for key, option := range original.AddressOptions {
			if usedOptions[key] {
				continue
			}
			// A dangling or duplicated option must fail compilation, not become
			// an implicitly created address or silently disappear.
			if v.AddressOptions == nil {
				v.AddressOptions = map[string]network.AddressOptions{}
			}
			v.AddressOptions[key] = option
		}
		add(Resource{ID: "interface/" + v.Name, Interface: &v})
	}
	for _, original := range native.Network.Routes {
		v := original
		add(Resource{ID: "route/" + identityHash(routeKey(v)), Route: &v})
	}
	for _, original := range native.Network.PolicyRules {
		v := original
		add(Resource{ID: fmt.Sprintf("policy_rule/%d/%d", v.Family, v.Priority), PolicyRule: &v})
	}
	if native.Network.Forwarding.IPv4 != nil || native.Network.Forwarding.IPv6 != nil || native.Network.Forwarding.IPv4RPFilter != nil || native.Network.Forwarding.ConntrackTCPLiberal != nil || native.Network.Forwarding.ConntrackAccounting != nil || native.Network.Forwarding.IPv4ECMPHash != "" {
		v := native.Network.Forwarding
		add(Resource{ID: "forwarding", Forwarding: &v})
	}
	if native.Network.Firewall != nil {
		v := *native.Network.Firewall
		if len(v.NAT) > 0 {
			add(Resource{ID: "nat", NAT: slices.Clone(v.NAT)})
			v.NAT = nil
		}
		add(Resource{ID: "firewall", Firewall: &v})
	}
	for _, original := range native.Network.VRRPGuards {
		v := original
		addresses := slices.Clone(v.Addresses)
		for i, raw := range addresses {
			if ip, err := netip.ParseAddr(raw); err == nil {
				addresses[i] = ip.String()
			}
		}
		slices.Sort(addresses)
		key := fmt.Sprintf("%d/%s/%s", v.Family, v.Interface, strings.Join(addresses, ","))
		add(Resource{ID: "vrrp_guard/" + identityHash(key), VRRPGuard: &v})
	}
	for _, original := range native.Network.WireGuard {
		v := original
		add(Resource{ID: "wireguard/" + v.Interface, WireGuard: &v})
	}
	for _, original := range native.Services {
		v := original
		add(Resource{ID: "service/" + v.Name, Service: &v})
	}
	dependencies := inferredDependencies(program.Resources)
	for i := range program.Resources {
		r := &program.Resources[i]
		r.DependsOn = append(r.DependsOn, dependencies[r.ID]...)
		slices.Sort(r.DependsOn)
		r.DependsOn = slices.Compact(r.DependsOn)
	}
	// This is presentation ordering. Compile uses the dependency graph, so an
	// operator may freely move declarations without changing their projection.
	slices.SortFunc(program.Resources, func(a, b Resource) int { return strings.Compare(a.ID, b.ID) })
	return program
}

func identityHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:12])
}
