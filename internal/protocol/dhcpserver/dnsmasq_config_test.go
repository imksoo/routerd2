// SPDX-License-Identifier: BSD-3-Clause

package dhcpserver

import (
	"strings"
	"testing"
)

func TestDNSMasqConfigBridgesHAIngressToVIP(t *testing.T) {
	got := string(dnsmasqConfig(engineSettings{Intent: Config{
		Resource: "leases", Interface: "lan-vip", IngressInterface: "eth1", ServerAddress: "172.18.0.1",
		Primary: Scope{Prefix: "172.18.0.0/16", Start: "172.18.1.100", End: "172.18.1.200", LeaseSeconds: 120},
		Scopes:  []Scope{{Name: "special", Prefix: "172.17.0.0/16", Start: "172.17.1.100", End: "172.17.1.200", LeaseSeconds: 120}},
	}}))
	if !strings.Contains(got, "bridge-interface=lan-vip,eth1\n") || !strings.Contains(got, "interface=lan-vip\n") {
		t.Fatalf("HA DHCP lost its VIP identity or physical ingress bridge:\n%s", got)
	}
	if !strings.Contains(got, "bind-dynamic\n") || !strings.Contains(got, "interface=eth1\n") {
		t.Fatalf("HA DHCP lost dynamic binding or physical ingress:\n%s", got)
	}
	if !strings.Contains(got, "shared-network=172.18.0.1,172.17.1.100\n") {
		t.Fatalf("HA DHCP lost the secondary LAN scope behind the VIP:\n%s", got)
	}
}

func TestDNSMasqConfigBindsDynamicWithoutIngress(t *testing.T) {
	got := string(dnsmasqConfig(engineSettings{Intent: Config{
		Resource: "leases", Interface: "lan0", ServerAddress: "192.0.2.1",
		Primary: Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.200", LeaseSeconds: 120},
	}}))
	if !strings.Contains(got, "bind-dynamic\n") {
		t.Fatalf("ordinary DHCP lost dynamic interface binding:\n%s", got)
	}
}
