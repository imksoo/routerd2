package health

import (
	"context"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	old "github.com/imksoo/routerd/pkg/healthcheck"
)

func TestEveryCompletedProbeRetainsItsOriginalTypedEvent(t *testing.T) {
	cfg := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := []old.ProbeResult{{OK: true}, {OK: true}, {Timeout: true, Message: "fixture timeout"}, {Message: "fixture failure"}}
	probes, events := 0, 0
	err := RunWithProbe(ctx, cfg, func(_ context.Context, s Snapshot) error {
		if s.Stopped {
			if s.ProbeEvent != nil {
				t.Fatal("shutdown invented a completed probe")
			}
			return nil
		}
		if s.ProbeEvent == nil {
			t.Fatal("completed probe event was discarded")
		}
		e := s.ProbeEvent
		if e.Type != "routerd.healthcheck.wan."+s.State.LastResult || e.Resource == nil || e.Resource.Name != cfg.Resource || e.Resource.Kind != "HealthCheck" || e.Resource.APIVersion != api.NetAPIVersion || e.Daemon.Kind != old.DaemonKind || e.Daemon.Instance != cfg.Resource || !e.Time.Equal(s.State.LastCheckedAt) || e.Attributes["phase"] != s.State.Phase || e.Attributes["network.interface.name"] != cfg.Interface {
			t.Fatal("probe event lost its original identity, time or evidence", e)
		}
		events++
		if events == len(results) {
			cancel()
		}
		return nil
	}, nil, func(context.Context, Config) old.ProbeResult {
		r := results[probes]
		probes++
		return r
	})
	if err != nil || events != len(results) {
		t.Fatal(events, err)
	}
}
