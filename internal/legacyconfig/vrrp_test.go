// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"os"
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
