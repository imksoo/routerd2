package legacyconfig

import (
	"bytes"
	"github.com/imksoo/routerd2/internal/config"
	"os"
	"strings"
	"testing"
)

func TestLearnedDNSUpstreamElectionImport(t *testing.T) {
	input, err := os.ReadFile("../integration/testdata/vrrp-import-wan.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rawPolicy, err := os.ReadFile("../integration/testdata/vrrp-import-wan-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DecodeOptions(bytes.NewReader(rawPolicy))
	if err != nil {
		t.Fatal(err)
	}
	result := compiledPolicy(t, input, policy)
	plan, err := config.Compile(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range plan.Native.Services {
		if s.DNS == nil {
			continue
		}
		if len(s.DNS.UpstreamOwners) != 1 {
			t.Fatal("missing learned upstream owner", s.DNS.UpstreamOwners)
		}
		owner := s.DNS.UpstreamOwners[0]
		if owner.Source != "wan-dns" || owner.Provider == "" || owner.VRRP == "" {
			t.Fatal(owner)
		}
		found = true
	}
	if !found {
		t.Fatal("DNS service missing")
	}
	// A fallback literal cannot silently bypass the election condition.
	bad := strings.Replace(string(input), "protocol: udp\n        addressFrom:", "protocol: udp\n        address: 192.0.2.53\n        addressFrom:", 1)
	if _, err := CompileWithOptions([]byte(bad), policy); !hasIssue(err, "when", "unsupported_semantics") {
		t.Fatal(err)
	}
}
