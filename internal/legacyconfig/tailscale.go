// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"net/netip"
	"strings"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func (c *compiler) tailscale(r resource) {
	c.allow(r, "state", "hostname", "loginServer", "authKey", "authKeyEnv", "authKeyFile", "advertiseExitNode", "advertiseRoutes", "advertiseTags", "acceptRoutes", "acceptDNS", "ssh", "shieldsUp")
	s := r.Spec.(api.TailscaleNodeSpec)
	if s.State == "absent" {
		c.note(r, "No native node is created for an absent declaration. Import does not adopt or stop an external tailscaled or erase its identity.")
		return
	}
	if s.State != "" && s.State != "present" {
		c.reject(r, "state", "invalid_value", "Tailscale state must be present or absent")
	}
	// tailscale up defaults to accept-dns=true, unlike the private native
	// resolver. Do not silently turn an omitted flag into acceptDNS:false.
	if s.AcceptDNS == nil || *s.AcceptDNS {
		c.reject(r, "acceptDNS", "unsupported_semantics", "legacy acceptDNS defaults to true and changes the OS resolver; explicit acceptDNS:false is required, with native DNS forwarding configured separately")
	}
	if s.SSH {
		c.reject(r, "ssh", "unsupported_semantics", "Tailscale SSH requires its own OS/user/authorization contract and is not native management SSH")
	}
	policy, exists := c.options.Tailscale[r.Kind+"/"+r.Metadata.Name]
	if !exists {
		c.reject(r, "", "explicit_policy_required", "tailscale policy must select owned interface, bootstrap DNS/trust and key_file or saved_identity enrollment for the fixed native daemon")
		return
	}
	c.usePolicy(r, "tailscale")
	node := &tailscaleconfig.Node{Interface: policy.Interface, ServerURL: strings.TrimSpace(s.LoginServer), Hostname: strings.TrimSpace(s.Hostname), BootstrapDNS: policy.BootstrapDNS, ControlCAFile: policy.ControlCAFile, AdvertiseExitNode: s.AdvertiseExitNode, AdvertiseTags: append([]string(nil), s.AdvertiseTags...)}
	if node.ServerURL == "" {
		node.ServerURL = "https://controlplane.tailscale.com"
	}
	switch policy.Enrollment {
	case TailscaleKeyFile:
		if policy.AuthKeyFile == "" {
			c.policyIssue(r, "tailscale", "auth_key_file", "policy_required", "key_file enrollment requires an owned raw native auth key file")
		}
		node.AuthKeyFile = policy.AuthKeyFile
	case TailscaleSavedIdentity:
		if policy.AuthKeyFile != "" {
			c.policyIssue(r, "tailscale", "auth_key_file", "policy_conflict", "saved_identity enrollment does not use an authentication key file")
		}
	default:
		c.policyIssue(r, "tailscale", "enrollment", "policy_required", "choose key_file or saved_identity explicitly; legacy inline keys and EnvironmentFile references are replaced, never copied")
	}
	if s.AcceptRoutes != nil {
		node.AcceptRoutes = *s.AcceptRoutes
	}
	if s.ShieldsUp != nil {
		node.ShieldsUp = *s.ShieldsUp
	}
	for _, value := range s.AdvertiseRoutes {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			c.reject(r, "advertiseRoutes", "invalid_value", "Tailscale advertised route must be an IP prefix")
			continue
		}
		node.AdvertiseRoutes = append(node.AdvertiseRoutes, prefix.Masked())
	}
	c.addService(r, serviceconfig.Service{Name: c.serviceName(r, "tail"), Tailscale: node})
	c.note(r, "Explicit replacement: a fixed private daemon owns its TUN/table52 and upstream Tailscale netfilter chains, preserving the legacy Linux netfilter-on and subnet SNAT defaults. Declare WAN routing and operator firewall intent separately; external OS routes are not imported. Legacy inline keys and systemd EnvironmentFile values are replaced by the selected enrollment policy. Provision a stopped daemon's saved identity separately for saved_identity; import neither reads nor changes it. acceptDNS:false is retained and Tailscale SSH is not enabled.")
}
