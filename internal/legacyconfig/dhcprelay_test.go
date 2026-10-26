package legacyconfig

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestDHCPRelayCombinesInterfacesAndPreservesTheirUpstreams(t *testing.T) {
	source := `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: relay}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: clients}
      spec: {ifname: lan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: gateway}
      spec: {interface: clients, address: 192.0.2.1/24}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Relay
      metadata: {name: primary}
      spec: {interfaces: [clients], upstream: 198.51.100.2}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Relay
      metadata: {name: second}
      spec: {interfaces: [clients], upstream: 198.51.100.3}
`
	result, err := Compile([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(result.Document)
	if err != nil || len(plan.Native.Services) != 1 {
		t.Fatal(err, plan.Native.Services)
	}
	relay := plan.Native.Services[0].DHCPRelay
	if relay == nil || len(relay.Interfaces) != 1 || relay.Interfaces[0].Name != "lan0" || relay.Interfaces[0].Address != "192.0.2.1" || strings.Join(relay.Interfaces[0].Servers, ",") != "198.51.100.2,198.51.100.3" {
		t.Fatal("relay interface/upstream mapping changed", relay)
	}
	bad := strings.Replace(source, "interfaces: [clients]", "interfaces: [missing]", 1)
	if _, err := Compile([]byte(bad)); err == nil {
		t.Fatal("missing relay interface was accepted")
	}
}
