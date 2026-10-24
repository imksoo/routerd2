package events

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd/pkg/daemonapi"
)

func TestTypedHistoricalMetadataRoundTripsWithoutBecomingAuthority(t *testing.T) {
	s, err := Open(Options{Directory: filepath.Join(t.TempDir(), "events")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := testEvent("typed")
	event.Detail = &Detail{Daemon: daemonapi.DaemonRef{Name: "fixture", Kind: "fixture", Instance: "worker"}, Resource: &daemonapi.ResourceRef{APIVersion: "net/v1", Kind: "Health", Name: "wan"}, Reason: "Observed", Message: "historical observation"}
	r, err := s.Append(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if r.Event.Detail == nil || r.Event.DaemonEvent().Resource.Name != "wan" || r.Event.DaemonEvent().Daemon.Instance != "worker" {
		t.Fatal(r)
	}
	event.Detail.Daemon.Kind = "different"
	if _, err = s.Append(context.Background(), event); err == nil {
		t.Fatal("contradictory source identity accepted")
	}
}
