package runtimeconfig

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/configref"
)

func TestStartupSelectionRequiresExplicitValidReference(t *testing.T) {
	b := Default()
	b.Platform = "host"
	b.Startup = &configref.Selection{Generation: 1, ExpectedRevision: 2, Snapshot: configref.Ref{StoreID: strings.Repeat("a", 32), Revision: 1, Digest: strings.Repeat("b", 64)}}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*configref.Selection){func(s *configref.Selection) { s.Generation = 0 }, func(s *configref.Selection) { s.ExpectedRevision = 0 }, func(s *configref.Selection) { s.Snapshot.Revision = 3 }, func(s *configref.Selection) { s.Snapshot.Digest = "../config" }} {
		bad := *b.Startup
		mutate(&bad)
		b.Startup = &bad
		if err := b.Validate(); err == nil {
			t.Fatal("accepted invalid explicit startup selection")
		}
		b.Startup = &configref.Selection{Generation: 1, ExpectedRevision: 2, Snapshot: configref.Ref{StoreID: strings.Repeat("a", 32), Revision: 1, Digest: strings.Repeat("b", 64)}}
	}
	b.Platform = "appliance"
	b.Release = &Release{Root: "/var/lib/routerd/releases", TrustFile: "/etc/routerd/trust.json"}
	if err := b.Validate(); err == nil || !strings.Contains(err.Error(), "atomic runtime/configuration") {
		t.Fatal("uncoupled signed runtime/config selection was accepted", err)
	}
}
