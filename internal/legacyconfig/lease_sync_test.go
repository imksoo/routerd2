package legacyconfig

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestLeaseSyncImportKeepsStandbyAndOriginalOwner(t *testing.T) {
	input, err := os.ReadFile("testdata/lease-sync.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("testdata/lease-sync-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(policy))
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	for _, service := range plan.Native.Services {
		if service.VRRP != nil {
			owner = service.Name
		}
	}
	if owner == "" {
		t.Fatal("lost the VRRP owner")
	}
	dhcp, pd := false, false
	for _, resource := range result.Document.Resources {
		service := resource.Service
		if service == nil {
			continue
		}
		if c := service.DHCPServer; c != nil {
			dhcp = true
			if resource.When != nil || c.HA == nil || c.HA.Owner != owner || c.Interface != "lan-ha" || c.ServerAddress != "192.0.2.1" || c.StickyHoldDays != 3 || len(c.Reservations) != 1 || c.Primary.Router != "192.0.2.1" || len(c.Primary.DNSServers) != 1 || c.Primary.DNSServers[0] != "192.0.2.1" {
				t.Fatal("lost DHCP state ownership or standby synchronization")
			}
		}
		if c := service.DHCP6; c != nil {
			pd = true
			if resource.When != nil || c.HA == nil || c.HA.Owner != owner || c.ClientDUID != "0003000102005e000143" || c.IAID != 1 || len(c.Delegations) != 1 {
				t.Fatal("lost PD identity, delegation or standby synchronization")
			}
		}
	}
	if !dhcp || !pd {
		t.Fatal("missing imported lease owners")
	}
	var shared string
	for _, mapping := range result.Mappings {
		if mapping.Resource == "DHCPv4ServerLeaseSync/v4" {
			shared = strings.Join(mapping.Targets, ",")
		}
	}
	for _, mapping := range result.Mappings {
		if mapping.Resource == "DHCPv6ServerLeaseSync/v6" && strings.Join(mapping.Targets, ",") != shared {
			t.Fatal("shared dnsmasq file lost its owner")
		}
	}
	for name, change := range map[string]func(*Options){
		"SSH alone is not TLS": func(o *Options) { o.LeaseSync = nil },
	} {
		t.Run(name, func(t *testing.T) {
			o, err := DecodeOptions(bytes.NewReader(policy))
			if err != nil {
				t.Fatal(err)
			}
			change(&o)
			result, err := CompileWithOptions(input, o)
			if err == nil || len(result.Document.Resources) != 0 {
				t.Fatal("unapproved migration produced usable intent")
			}
		})
	}
	changed := bytes.Replace(input, []byte("equals: master"), []byte("equals: backup"), 1)
	if _, err := CompileWithOptions(changed, options); err == nil {
		t.Fatal("different PD source and synchronization owners accepted")
	}
}
