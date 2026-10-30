// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"strings"
	"testing"
)

func TestNAT44SyncUsesItsNATRulesSingleIPv4Election(t *testing.T) {
	options := Options{SchemaVersion: OptionsSchemaVersion, VRRP: map[string]VRRPPolicy{"VirtualAddress/gateway": {SourceAddress: "198.18.0.2", VIPInterface: "lan-ha"}}}
	input := fixture(t, "conntrack")
	result, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, service := range nativeProgram(t, result.Document).Services {
		if service.VRRP == nil || service.VRRP.ConntrackSync == nil {
			continue
		}
		count++
		cfg := *service.VRRP.ConntrackSync
		if cfg.Interface != "sync0" || cfg.LocalAddress.String() != "169.254.42.2" || cfg.PeerAddress.String() != "169.254.42.3" || cfg.Port != 3781 || len(cfg.IgnoreIPv4) != 1 || cfg.IgnoreIPv4[0].String() != "198.18.0.2" {
			t.Fatal("lost conntrack peer intent", cfg)
		}
	}
	if count != 1 {
		t.Fatal("expected one engine owned by the election, got", count)
	}
	// Losing the master condition in both the NAT and its source must not pick
	// the only available election merely because it happens to be present.
	unconditional := strings.ReplaceAll(string(input), "        when: {state: {VirtualAddress/gateway.role: {equals: master}}}\n", "")
	if _, err := CompileWithOptions([]byte(unconditional), options); !hasIssue(err, "natRules", "unsupported_semantics") {
		t.Fatal("missing NAT owner was guessed", err)
	}
	// Two required elections cannot both own one conntrack engine.
	ambiguous := strings.ReplaceAll(string(input), "when: {state: {VirtualAddress/gateway.role: {equals: master}}}", "when: {all: [{state: {VirtualAddress/gateway.role: {equals: master}}}, {state: {VirtualAddress/other.role: {equals: master}}}]}") + `    - apiVersion: net.routerd.net/v1alpha1
      kind: VirtualAddress
      metadata: {name: other}
      spec:
        family: ipv4
        interface: lan
        address: 198.18.0.10/24
        mode: vrrp
        vrrp: {virtualRouterID: 43, priority: 150, peers: [198.18.0.3]}
`
	options.VRRP["VirtualAddress/other"] = VRRPPolicy{SourceAddress: "198.18.0.2", VIPInterface: "other-ha"}
	if _, err := CompileWithOptions([]byte(ambiguous), options); !hasIssue(err, "natRules", "unsupported_semantics") {
		t.Fatal("ambiguous NAT election owner accepted", err)
	}
	delete(options.VRRP, "VirtualAddress/other")
	duplicate := string(input) + `    - apiVersion: net.routerd.net/v1alpha1
      kind: NAT44SessionSync
      metadata: {name: other}
      spec:
        mode: conntrackd
        natRules: [NAT44Rule/outgoing]
        conntrackd: {interface: sync, localAddress: 169.254.42.2, peerAddress: 169.254.42.3}
`
	if _, err := CompileWithOptions([]byte(duplicate), options); !hasIssue(err, "", "unsupported_semantics") {
		t.Fatal("multiple kernel sync owners accepted", err)
	}
}
