// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"errors"
	"strings"
	"testing"
)

func TestManagementAccessChecksTheDeclaredPathWithoutInventingAnACL(t *testing.T) {
	result := compiled(t, "management")
	found := false
	for _, mapping := range result.Mappings {
		if mapping.Resource == "ManagementAccess/management" {
			found = true
			if len(mapping.Targets) != 1 || mapping.Targets[0] != "configuration-validation" || !strings.Contains(strings.Join(mapping.Notes, " "), "never enforced") {
				t.Fatal("management diagnostic or unenforced source-prefix semantics were lost")
			}
		}
	}
	if !found {
		t.Fatal("ManagementAccess mapping is missing")
	}
	input := string(fixture(t, "management"))
	for _, tc := range []struct {
		name, source, code string
	}{
		{"missing interface", strings.Replace(input, "interfaces: [Interface/management]\n        allowSourceCIDRs", "interfaces: [Interface/missing]\n        allowSourceCIDRs", 1), "reference"},
		{"untrusted zone", strings.Replace(input, "role: mgmt", "role: untrust", 1), "management_access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile([]byte(tc.source))
			var unsupported *UnsupportedError
			if !errors.As(err, &unsupported) {
				t.Fatal("unsafe management declaration accepted", err)
			}
			for _, issue := range unsupported.Issues {
				if issue.Code == tc.code {
					return
				}
			}
			t.Fatalf("management diagnostic missing: %v", err)
		})
	}
}

func TestManagementAccessRequiresConsoleBinding(t *testing.T) {
	input := string(fixture(t, "management")) + `    - apiVersion: system.routerd.net/v1alpha1
      kind: WebConsole
      metadata: {name: console}
      spec: {enabled: true, listenAddress: 0.0.0.0, port: 8080}
`
	if _, err := Compile([]byte(input)); err == nil || !strings.Contains(err.Error(), "management_access") {
		t.Fatal("wildcard management console accepted", err)
	}
	result, err := Compile([]byte(strings.Replace(input, "listenAddress: 0.0.0.0", "listenAddress: 198.18.0.1", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, service := range nativeProgram(t, result.Document).Services {
		if service.WebConsole != nil {
			found = service.WebConsole.Listen.String() == "198.18.0.1:8080"
		}
	}
	if !found {
		t.Fatal("management console listener missing")
	}
}
