// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestDHCP6ImportBindsServingAndRAToSelectedPD(t *testing.T) {
	data, err := os.ReadFile("../integration/testdata/dhcp6-pd-import.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("../integration/testdata/dhcp6-pd-import-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(policy))
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompileWithOptions(data, options)
	if err != nil {
		t.Fatal(err)
	}
	pd, server := service(t, result.Document, "dhcp6"), service(t, result.Document, "dhcp6_server")
	if server.DHCP6Server.ServerDUID != "00030001020000000042" {
		t.Fatal("migration changed selected server DUID", server.DHCP6Server.ServerDUID)
	}
	if pd.DHCP6.IAID != 1 || pd.DHCP6.PrefixLength != 60 || len(pd.DHCP6.Delegations) != 3 {
		t.Fatal("changed provider identity or allocation", pd)
	}
	lan := pd.DHCP6.Delegations[0]
	if !lan.Advertise || !lan.OtherConfig || !lan.UseLearnedRDNSS || !lan.UseLearnedDNSSL || lan.RouterLifetimeSeconds == nil || *lan.RouterLifetimeSeconds != 30 || lan.Interface != server.DHCP6Server.Interface {
		t.Fatal("serving and RA did not select the same LAN allocation", lan, server)
	}
	for _, sources := range [][]string{server.DHCP6Server.DNSFrom, server.DHCP6Server.SNTPFrom, server.DHCP6Server.SearchFrom} {
		if !slices.Equal(sources, []string{pd.Name}) {
			t.Fatal("PD/information alias did not retain one source identity", sources, pd.Name)
		}
	}
	// Independent dnsmasq capture has no RA DNS option when none is declared.
	withoutDNS := bytes.Replace(data, []byte("        dnsServerFrom: [{resource: DHCPv6Information/wan-info, field: dnsServers}]\n"), nil, 1)
	dnsAbsent, err := CompileWithOptions(withoutDNS, options)
	if err != nil {
		t.Fatal(err)
	}
	pdAbsent, serverAbsent := service(t, dnsAbsent.Document, "dhcp6"), service(t, dnsAbsent.Document, "dhcp6_server")
	if len(serverAbsent.DHCP6Server.DNSFrom) != 0 || pdAbsent.DHCP6.Delegations[0].UseLearnedRDNSS || !pdAbsent.DHCP6.Delegations[0].UseLearnedDNSSL {
		t.Fatal("absent DNS suppressed independent search or enabled implicit DNS", pdAbsent, serverAbsent)
	}
	// An operator must see the unsupported batch, never a usable prefix-only
	// partial result with its required serving/RA half silently absent.
	for _, input := range [][]byte{data, bytes.Replace(data, []byte("mode: stateless"), []byte("mode: both"), 1)} {
		var rejected Result
		if bytes.Equal(input, data) {
			rejected, err = Compile(input)
		} else {
			rejected, err = CompileWithOptions(input, options)
		}
		if err == nil || !reflect.DeepEqual(rejected, Result{}) {
			t.Fatal("unsupported batch returned partial intent", rejected, err)
		}
	}
}
