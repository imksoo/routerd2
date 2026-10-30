// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"strings"
	"testing"
)

func TestPersistentConntrackSettingMigration(t *testing.T) {
	source := `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: sysctl-migration}
spec:
  resources:
    - apiVersion: system.routerd.net/v1alpha1
      kind: Sysctl
      metadata: {name: conntrack-window}
      spec:
        key: net.netfilter.nf_conntrack_tcp_be_liberal
        value: "1"
        expectedValue: "1"
        runtime: true
        persistent: true
`
	for _, key := range []string{"net.netfilter.nf_conntrack_tcp_be_liberal", "net.netfilter.nf_conntrack_acct"} {
		input := strings.Replace(source, "net.netfilter.nf_conntrack_tcp_be_liberal", key, 1)
		result, err := Compile([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		forwarding := nativeProgram(t, result.Document).Network.Forwarding
		setting := forwarding.ConntrackTCPLiberal
		if key == "net.netfilter.nf_conntrack_acct" {
			setting = forwarding.ConntrackAccounting
		}
		if setting == nil || !*setting {
			t.Fatal("import lost required conntrack setting")
		}
		if len(result.Mappings) != 1 || len(result.Mappings[0].Notes) != 1 || !strings.Contains(result.Mappings[0].Notes[0], "committed native configuration") {
			t.Fatal("persistence ownership must be disclosed")
		}
		for _, change := range []struct{ old, replacement string }{
			{"runtime: true", "runtime: false"},
			{"persistent: true", "persistent: true\n        optional: true"},
			{`expectedValue: "1"`, `expectedValue: "0"`},
		} {
			if _, err := Compile([]byte(strings.Replace(input, change.old, change.replacement, 1))); err == nil {
				t.Fatalf("unsupported runtime policy accepted: %s", change.replacement)
			}
		}
	}
}
