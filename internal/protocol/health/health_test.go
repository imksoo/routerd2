package health

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

func fixture() Config {
	return Normalize(Config{Resource: "wan", Interface: "wan0", Target: "192.0.2.1", SourceAddress: "192.0.2.2", Mark: 42, Protocol: "tcp", Port: 443, IntervalMillis: 100, TimeoutMillis: 50, HealthyThreshold: 2, UnhealthyThreshold: 2})
}
func TestValidateScopeAndBounds(t *testing.T) {
	c := fixture()
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Config){func(c *Config) { c.Interface = "" }, func(c *Config) { c.Target = "example.com" }, func(c *Config) { c.Target = "224.0.0.1" }, func(c *Config) { c.Target = "::ffff:192.0.2.1" }, func(c *Config) { c.SourceAddress = "2001:db8::1" }, func(c *Config) { c.Port = 65536 }, func(c *Config) { c.Protocol = "shell" }, func(c *Config) { c.IntervalMillis = 1 }, func(c *Config) { c.UnhealthyThreshold = 1001 }}
	for i, modify := range cases {
		x := c
		modify(&x)
		if err := Validate(x); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
	if c.Freshness() != 2*time.Minute {
		t.Fatal("legacy freshness floor lost")
	}
	c.IntervalMillis = 60000
	c.TimeoutMillis = 60000
	if c.Freshness() != 4*time.Minute {
		t.Fatal("freshness formula lost")
	}
}
func TestRunThresholdRecoveryDeadlineAndShutdown(t *testing.T) {
	c := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := []bool{true, true, false, false, true, true}
	var phases []string
	var active []bool
	var observed []uint64
	ready, stopped := false, false
	calls := 0
	effects := func(_ context.Context, s Snapshot) error {
		clock, err := bfd.ReadClock()
		if err != nil {
			return err
		}
		if err := ValidateSnapshot(c, s, clock); err != nil {
			return err
		}
		if s.Stopped {
			stopped = true
			return nil
		}
		if !ready {
			t.Fatal("result before worker initialization")
		}
		phases = append(phases, s.State.Phase)
		active = append(active, s.Active)
		observed = append(observed, s.ObservedNanos)
		if len(phases) == len(results) {
			cancel()
		}
		return nil
	}
	probe := func(ctx context.Context, got Config) old.ProbeResult {
		if !reflect.DeepEqual(c, got) {
			t.Fatal("scope changed", got)
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 60*time.Millisecond {
			t.Fatal("probe missing timeout")
		}
		result := old.ProbeResult{OK: results[calls]}
		calls++
		return result
	}
	if err := RunWithProbe(ctx, c, effects, func() { ready = true }, probe); err != nil {
		t.Fatal(err)
	}
	want := []string{"Passing", "Healthy", "Failing", "Unhealthy", "Passing", "Healthy"}
	if !reflect.DeepEqual(phases, want) || !reflect.DeepEqual(active, []bool{false, true, true, false, false, true}) {
		t.Fatal(phases, active)
	}
	if !stopped {
		t.Fatal("no finite shutdown withdrawal")
	}
	for i := 1; i < len(observed); i++ {
		if observed[i] <= observed[i-1] {
			t.Fatal("non-increasing sample clock")
		}
	}
}
func TestSnapshotFencing(t *testing.T) {
	c := fixture()
	clock := bfd.Clock{BootID: "boot", BoottimeNanoseconds: 100}
	s := Snapshot{Resource: c.Resource, Interface: c.Interface, BootID: "boot", ObservedNanos: 90, ExpiresNanos: 200, Active: true, State: old.State{Phase: old.PhaseHealthy, ConsecutivePassed: 2}}
	if err := ValidateSnapshot(c, s, clock); err != nil {
		t.Fatal(err)
	}
	cases := []func(*Snapshot){func(s *Snapshot) { s.BootID = "old" }, func(s *Snapshot) { s.Resource = "other" }, func(s *Snapshot) { s.Interface = "other0" }, func(s *Snapshot) { s.ExpiresNanos = 100 }, func(s *Snapshot) { s.ObservedNanos = 101 }, func(s *Snapshot) { s.ExpiresNanos = 90 + uint64(c.Freshness()) + 1 }, func(s *Snapshot) { s.Active = false }, func(s *Snapshot) { s.State.ConsecutivePassed = 1 }, func(s *Snapshot) { s.Stopped = true }}
	for i, modify := range cases {
		x := s
		modify(&x)
		if err := ValidateSnapshot(c, x, clock); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
func TestPublicationFailureStopsProbes(t *testing.T) {
	failure := errors.New("control unavailable")
	calls := 0
	err := RunWithProbe(context.Background(), fixture(), func(context.Context, Snapshot) error { return failure }, nil, func(context.Context, Config) old.ProbeResult { calls++; return old.ProbeResult{OK: true} })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatal("producer continued after lost acknowledgement", err, calls)
	}
}
