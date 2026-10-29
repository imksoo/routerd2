// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestVRRPImportPreservesKeepalivedWireAndPreemptionDefaults(t *testing.T) {
	for _, family := range []string{"ipv4", "ipv6"} {
		input, err := os.ReadFile("../integration/testdata/vrrp-import-" + family + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		policy, err := os.ReadFile("../integration/testdata/vrrp-import-" + family + "-policy.yaml")
		if err != nil {
			t.Fatal(err)
		}
		vip := "192.0.2.254/24"
		if family == "ipv6" {
			vip = "2001:db8:42::254/64"
		}
		input = bytes.Replace(input, []byte("address: "+strings.Split(vip, "/")[0]+"\n"), []byte("address: "+vip+"\n"), 1)
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
		found := false
		for _, service := range plan.Native.Services {
			if service.VRRP == nil {
				continue
			}
			found = true
			c := service.VRRPConfig()
			if c.Addresses[len(c.Addresses)-1] != vip {
				t.Fatal("migration changed the configured VIP prefix", c.Addresses, vip)
			}
			version := 2
			if family == "ipv6" {
				version = 3
			}
			if c.Version != version || c.Preempt == nil || *c.Preempt || c.AdvertIntervalCentiseconds != 100 || c.Priority != 150 || c.VIPInterface != "ha" || len(c.UnicastPeers) != 1 {
				t.Fatal("migration changed original wire/election settings", family, c)
			}
		}
		if !found {
			t.Fatal("migration lost the VRRP election", family)
		}
	}
}

func TestInformationAliasCannotEraseIndependentRoleCondition(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/vrrp-import-wan.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("../integration/testdata/vrrp-import-wan-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(policy))
	if err != nil {
		t.Fatal(err)
	}
	// The PD owner requires master. Serving information only while backup
	// needs a different lifetime and cannot be represented by its alias.
	input = bytes.Replace(input, []byte("VirtualAddress/gateway.status.role: {equals: master}"), []byte("VirtualAddress/gateway.status.role: {equals: backup}"), 1)
	result, err := CompileWithOptions(input, options)
	if !hasIssue(err, "when", "unsupported_semantics") || len(result.Document.Resources) != 0 {
		t.Fatal("independent information condition returned usable intent", result, err)
	}
}
