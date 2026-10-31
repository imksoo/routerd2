// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func TestHealthImportRetainsProbeScopeAndOriginalTimingDefaults(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/health-import.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../integration/testdata/health-import-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, defaults := range []bool{false, true} {
		source := input
		if defaults {
			source = []byte(strings.ReplaceAll(string(input), "interval: 500ms, timeout: 500ms, ", ""))
		}
		result, err := CompileWithOptions(source, options)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := config.Compile(result.Document)
		if err != nil {
			t.Fatal(err)
		}
		probes := 0
		for _, service := range plan.Native.Services {
			c := service.Health
			if c == nil {
				continue
			}
			probes++
			interval, timeout := 500, 500
			if defaults {
				interval, timeout = 30000, 3000
			}
			if c.Mark != 42 || c.SourceAddress == "" || c.Protocol != "tcp" || c.IntervalMillis != interval || c.TimeoutMillis != timeout || c.HealthyThreshold != 2 || c.UnhealthyThreshold != 3 {
				t.Fatal("migration changed scoped probe intent", c)
			}
		}
		if probes != 2 {
			t.Fatal("migration omitted a WAN probe", probes)
		}
	}
	if result, err := Compile(input); err == nil || len(result.Document.Resources) != 0 {
		t.Fatal("unscoped probe migration must reject the complete batch", err)
	}
}

func TestHealthPolicyCanRequireActiveForConfigurationReadiness(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/health-import.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../integration/testdata/health-import-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	options, err := DecodeOptions(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	policy := options.HealthChecks["HealthCheck/health-a"]
	policy.RequireActiveForReady = true
	options.HealthChecks["HealthCheck/health-a"] = policy
	result, err := CompileWithOptions(input, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range result.Document.Resources {
		if resource.Service == nil || resource.Service.Health == nil || resource.Service.Health.Target != "198.51.100.10" {
			continue
		}
		if resource.ReadyWhen == nil || resource.ReadyWhen.Fact != "service."+resource.Service.Name+".active" {
			t.Fatalf("health readiness was not compiled from explicit policy: %#v", resource.ReadyWhen)
		}
		return
	}
	t.Fatal("configured health probe is missing")
}
