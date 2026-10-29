// SPDX-License-Identifier: BSD-3-Clause
package legacyconfig

import (
	"bytes"
	"github.com/imksoo/routerd2/internal/config"
	"os"
	"testing"
)

func TestHomertTimeFragmentsShareClockAndKeepIndependentSourceConditions(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/ntp-import.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("../integration/testdata/ntp-import-policy.yaml")
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
	clocks := 0
	for _, s := range plan.Native.Services {
		if s.NTP == nil {
			continue
		}
		clocks++
		n := s.NTP
		if len(n.Sources) != 2 || n.Sources[0].When != nil || len(n.Sources[0].Servers) != 3 || n.Sources[1].When == nil || len(n.Sources[1].SourcesFrom) != 1 || len(n.Sources[1].Servers) != 2 {
			t.Fatal("client/server source selection changed", n)
		}
		if len(n.DNS) != 1 || n.DNS[0].String() != "198.51.100.1" || len(n.Listen) != 1 || len(n.Allow) != 1 {
			t.Fatal("lost resolver/listener/access intent", n)
		}
	}
	if clocks != 1 {
		t.Fatal("multiple or missing clock writers", clocks)
	}
}
