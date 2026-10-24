package serviceconfig

import (
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/protocol/health"
)

func TestHealthEventDirectoryIsPrivateGeneratedIntentIndependentOfGeneration(t *testing.T) {
	s := Service{Name: "wan", Health: &health.Config{Interface: "wan0", Target: "192.0.2.1"}}
	i, err := s.Instance("/run/routerd", "/var/lib/routerd")
	if err != nil || i.EventDirectory != filepath.Join("/var/lib/routerd", "services", "wan", "health-events") {
		t.Fatal(i, err)
	}
	// The immutable worker config selects a per-service state directory, not
	// one lost at the next runtime/config digest or supervisor generation.
	s.Health.Target = "192.0.2.2"
	next, err := s.Instance("/run/routerd", "/var/lib/routerd")
	if err != nil || next.EventDirectory != i.EventDirectory {
		t.Fatal(next, err)
	}
}
