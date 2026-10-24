//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/resourceprogram"
)

const planSource = `schema_version: 2
resources:
  - id: default-v4
    owner: uplink
    depends_on: [uplink-ip]
    route:
      destination: 0.0.0.0/0
      interface: eth0
      gateway: 192.0.2.1
  - id: uplink-ip
    owner: uplink
    depends_on: [uplink]
    provenance:
      source: routerd
      resource: IPv4StaticAddress/uplink
    address:
      interface: eth0
      cidr: 192.0.2.2/24
  - id: uplink
    interface:
      name: eth0
      mtu: 1500
`

func planFixture(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "startup-config.yaml")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigPlanShowsSourceDependenciesAndOwnedEngineIntent(t *testing.T) {
	path := planFixture(t, planSource)
	var output bytes.Buffer
	if err := planConfig([]string{path}, &output); err != nil {
		t.Fatal(err)
	}
	var plan resourceprogram.Plan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatalf("plan is not a complete JSON object: %v", err)
	}
	if !reflect.DeepEqual(plan.Order, []string{"uplink", "uplink-ip", "default-v4"}) {
		t.Fatal("source dependency order was lost", plan.Order)
	}
	if !reflect.DeepEqual(plan.Dependencies["uplink-ip"], []string{"uplink"}) || !reflect.DeepEqual(plan.Dependencies["default-v4"], []string{"uplink", "uplink-ip"}) {
		t.Fatal("declared and inferred dependencies are missing", plan.Dependencies)
	}
	addressOwner := false
	for _, owner := range plan.Ownership {
		if owner.ResourceID == "uplink-ip" && owner.OwnerID == "uplink" && owner.Kind == "address" && owner.Key == "eth0/192.0.2.2/24" && owner.Provenance != nil && owner.Provenance.Resource == "IPv4StaticAddress/uplink" {
			addressOwner = true
		}
	}
	if !addressOwner {
		t.Fatal("address source identity, owner, or provenance is missing", plan.Ownership)
	}
	if len(plan.Native.Network.Interfaces) != 1 || !reflect.DeepEqual(plan.Native.Network.Interfaces[0].Addresses, []string{"192.0.2.2/24"}) || len(plan.Native.Network.Routes) != 1 || plan.Native.Network.Routes[0].Gateway != "192.0.2.1" {
		t.Fatal("compiled engine intent is incomplete", plan.Native)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != planSource {
		t.Fatal("plan rewrote source configuration", err)
	}
}

func TestConfigPlanRejectsInvalidSourceWithoutPartialOutput(t *testing.T) {
	cycle := strings.Replace(planSource, "- id: uplink\n    interface:", "- id: uplink\n    depends_on: [default-v4]\n    interface:", 1)
	missing := strings.Replace(planSource, "depends_on: [uplink-ip]", "depends_on: [missing-address]", 1)
	unknown := strings.Replace(planSource, "mtu: 1500", "mtu: 1500\n      typo: true", 1)
	for name, source := range map[string]string{"cycle": cycle, "missing": missing, "unknown-field": unknown} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := planConfig([]string{planFixture(t, source)}, &output); err == nil || output.Len() != 0 {
				t.Fatal("invalid source emitted partial executable intent", err, output.String())
			}
		})
	}
	for _, args := range [][]string{nil, {"one", "two"}, {filepath.Join(t.TempDir(), "missing")}} {
		var output bytes.Buffer
		if err := planConfig(args, &output); err == nil || output.Len() != 0 {
			t.Fatal("invalid arguments or unreadable input emitted a plan", args, err, output.String())
		}
	}
}

func TestConfigPlanDoesNotResolveOrGeneratePrivateKeys(t *testing.T) {
	dir := t.TempDir()
	for _, existing := range []bool{false, true} {
		key := filepath.Join(dir, fmt.Sprintf("private-%t.key", existing))
		if existing {
			if err := os.WriteFile(key, []byte("this is deliberately not a WireGuard key\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		source := fmt.Sprintf(`schema_version: 2
resources:
  - id: tunnel
    link: {name: wg0, kind: wireguard}
  - id: tunnel-key
    wireguard:
      interface: wg0
      private_key_file: %q
      generate_private_key: true
`, key)
		var output bytes.Buffer
		if err := planConfig([]string{planFixture(t, source)}, &output); err != nil {
			t.Fatal("plan tried to resolve a runtime private key", err)
		}
		if strings.Contains(output.String(), "deliberately") {
			t.Fatal("plan emitted private key-file contents")
		}
		contents, err := os.ReadFile(key)
		if existing && (err != nil || string(contents) != "this is deliberately not a WireGuard key\n") {
			t.Fatal("plan changed a private key file", err)
		}
		if !existing && !os.IsNotExist(err) {
			t.Fatal("plan generated a private key", err)
		}
	}
}
