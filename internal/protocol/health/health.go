// Package health adapts the pinned routerd probe/threshold contracts into a
// supervised finite observation producer. It never writes routes or shells out.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	old "github.com/imksoo/routerd/pkg/healthcheck"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
)

type Config struct {
	Resource           string     `json:"resource"`
	Interface          string     `json:"interface"`
	SourceAddress      string     `json:"source_address,omitempty"`
	Mark               uint32     `json:"mark,omitempty"`
	Target             string     `json:"target"`
	Protocol           string     `json:"protocol"`
	Port               int        `json:"port,omitempty"`
	HTTP               *HTTPCheck `json:"http,omitempty"`
	IntervalMillis     int        `json:"interval_ms,omitempty"`
	TimeoutMillis      int        `json:"timeout_ms,omitempty"`
	HealthyThreshold   int        `json:"healthy_threshold,omitempty"`
	UnhealthyThreshold int        `json:"unhealthy_threshold,omitempty"`
}

// HTTPCheck retains the IngressService HTTP request and response contract.
type HTTPCheck struct {
	Path           string `json:"path,omitempty"`
	Host           string `json:"host,omitempty"`
	ExpectedStatus []int  `json:"expected_status,omitempty"`
	TLSSkipVerify  bool   `json:"tls_skip_verify,omitempty"`
}

func Normalize(c Config) Config {
	if c.Protocol == "" {
		c.Protocol = "icmp"
	}
	if c.IntervalMillis == 0 {
		c.IntervalMillis = 1000
	}
	if c.TimeoutMillis == 0 {
		c.TimeoutMillis = 1000
	}
	if c.HealthyThreshold == 0 {
		c.HealthyThreshold = 1
	}
	if c.UnhealthyThreshold == 0 {
		c.UnhealthyThreshold = 3
	}
	return c
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,14}$`)

func Validate(raw Config) error {
	c := Normalize(raw)
	if c.Resource == "" || len(c.Resource) > 64 || !interfaceName.MatchString(c.Interface) {
		return errors.New("health requires resource and scoped interface")
	}
	target, err := netip.ParseAddr(c.Target)
	if err != nil || target.Zone() != "" || target.IsUnspecified() || target.IsMulticast() || target.Is4In6() {
		return errors.New("health target must be a unicast IPv4/IPv6 literal; dynamic target resolution is not supported yet")
	}
	if c.SourceAddress != "" {
		source, err := netip.ParseAddr(c.SourceAddress)
		if err != nil || source.Zone() != "" || source.IsUnspecified() || source.IsMulticast() || source.Is4() != target.Is4() || source.Is4In6() {
			return errors.New("health source must be a unicast address of the target family")
		}
	}
	switch c.Protocol {
	case "icmp", "tcp", "dns", "http", "https":
	default:
		return errors.New("unsupported health protocol")
	}
	if c.HTTP != nil {
		if c.Protocol != "http" && c.Protocol != "https" {
			return errors.New("HTTP request settings require http or https")
		}
		for _, status := range c.HTTP.ExpectedStatus {
			if status < 100 || status > 599 {
				return errors.New("invalid expected HTTP status")
			}
		}
	}
	if c.Port < 0 || c.Port > 65535 || c.Protocol == "icmp" && c.Port != 0 {
		return errors.New("invalid health port")
	}
	if c.IntervalMillis < 100 || c.IntervalMillis > 60000 || c.TimeoutMillis < 10 || c.TimeoutMillis > 60000 {
		return errors.New("health interval must be 100..60000ms and timeout 10..60000ms")
	}
	if c.HealthyThreshold < 1 || c.HealthyThreshold > 1000 || c.UnhealthyThreshold < 1 || c.UnhealthyThreshold > 1000 {
		return errors.New("health thresholds must be 1..1000")
	}
	return nil
}
func (c Config) spec() api.HealthCheckSpec {
	c = Normalize(c)
	family := "ipv6"
	if netip.MustParseAddr(c.Target).Is4() {
		family = "ipv4"
	}
	return api.HealthCheckSpec{Target: c.Target, Protocol: c.Protocol, Port: c.Port, SourceInterface: c.Interface, SourceAddress: c.SourceAddress, FwMark: int(c.Mark), AddressFamily: family, HealthyThreshold: c.HealthyThreshold, UnhealthyThreshold: c.UnhealthyThreshold}
}
func (c Config) Freshness() time.Duration {
	c = Normalize(c)
	return max(2*time.Minute, time.Duration(3*c.IntervalMillis+c.TimeoutMillis)*time.Millisecond)
}

type Snapshot struct {
	// ProbeEvent is offered to history storage, never serialized in the finite
	// status RPC. Shutdown carries no invented probe result.
	ProbeEvent     *daemonapi.DaemonEvent `json:"-"`
	HistoryDropped uint64                 `json:"history_dropped"`
	HistoryError   string                 `json:"history_error,omitempty"`
	Resource       string                 `json:"resource"`
	Interface      string                 `json:"interface"`
	BootID         string                 `json:"boot_id"`
	ObservedNanos  uint64                 `json:"observed_nanos"`
	ExpiresNanos   uint64                 `json:"expires_nanos"`
	Active         bool                   `json:"active"`
	Stopped        bool                   `json:"stopped,omitempty"`
	State          old.State              `json:"state"`
}

func ValidateSnapshot(raw Config, s Snapshot, clock bfd.Clock) error {
	c := Normalize(raw)
	if err := Validate(c); err != nil {
		return err
	}
	now := uint64(clock.BoottimeNanoseconds)
	if s.Resource != c.Resource || s.Interface != c.Interface || s.BootID != clock.BootID || s.ObservedNanos > now || s.ExpiresNanos <= now || s.ExpiresNanos <= s.ObservedNanos || s.ExpiresNanos-s.ObservedNanos > uint64(c.Freshness()) {
		return errors.New("health snapshot identity or finite interval mismatch")
	}
	if len(s.State.History) > 20 || len(s.State.LastMessage) > 4096 || len(s.HistoryError) > 768 {
		return errors.New("health snapshot exceeds history/message bounds")
	}
	if s.Stopped {
		if s.Active {
			return errors.New("stopped health cannot remain active")
		}
		return nil
	}
	switch s.State.Phase {
	case old.PhasePassing, old.PhaseHealthy, old.PhaseFailing, old.PhaseUnhealthy:
	default:
		return errors.New("unknown health phase")
	}
	switch s.State.Phase {
	case old.PhaseHealthy:
		if s.State.ConsecutivePassed < c.HealthyThreshold || s.State.ConsecutiveFailed != 0 {
			return errors.New("invalid healthy threshold state")
		}
	case old.PhasePassing:
		if s.State.ConsecutivePassed < 1 || s.State.ConsecutivePassed >= c.HealthyThreshold || s.State.ConsecutiveFailed != 0 {
			return errors.New("invalid passing threshold state")
		}
	case old.PhaseFailing:
		if s.State.ConsecutiveFailed < 1 || s.State.ConsecutiveFailed >= c.UnhealthyThreshold || s.State.ConsecutivePassed != 0 {
			return errors.New("invalid failing threshold state")
		}
		if s.Active && s.State.LastSuccessTime.IsZero() {
			return errors.New("failing health cannot be active before a successful probe")
		}
	case old.PhaseUnhealthy:
		if s.State.ConsecutiveFailed < c.UnhealthyThreshold || s.State.ConsecutivePassed != 0 {
			return errors.New("invalid unhealthy threshold state")
		}
	}
	if s.State.Phase == old.PhaseHealthy && !s.Active || (s.State.Phase == old.PhasePassing || s.State.Phase == old.PhaseUnhealthy) && s.Active {
		return errors.New("health eligibility does not match phase")
	}
	return nil
}

type ProbeFunc func(context.Context, Config) old.ProbeResult
type Effects func(context.Context, Snapshot) error

func Run(ctx context.Context, c Config, effects Effects, ready func()) error {
	return RunWithProbe(ctx, c, effects, ready, Probe)
}
func RunWithProbe(ctx context.Context, raw Config, effects Effects, ready func(), probe ProbeFunc) error {
	c := Normalize(raw)
	if err := Validate(c); err != nil {
		return err
	}
	if effects == nil || probe == nil {
		return errors.New("health effects and probe are required")
	}
	var state old.State
	active := false
	publish := func(ctx context.Context, stopped bool, event *daemonapi.DaemonEvent) error {
		clock, err := bfd.ReadClock()
		if err != nil {
			return err
		}
		ticks := uint64(clock.BoottimeNanoseconds)
		return effects(ctx, Snapshot{ProbeEvent: event, Resource: c.Resource, Interface: c.Interface, BootID: clock.BootID, ObservedNanos: ticks, ExpiresNanos: ticks + uint64(c.Freshness()), Active: !stopped && active, Stopped: stopped, State: state})
	}
	if _, err := bfd.ReadClock(); err != nil {
		return err
	}
	if ready != nil {
		ready()
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = publish(stop, true, nil)
	}()
	for {
		if ctx.Err() != nil {
			return nil
		}
		attempt, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMillis)*time.Millisecond)
		result := probe(attempt, c)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if len(result.Message) > 4096 {
			result.Message = result.Message[:4096]
		}
		resource := api.Resource{}
		resource.Metadata.Name = c.Resource
		evaluation := old.ApplyResult(resource, c.spec(), state, result, time.Now())
		evaluation.Event.Time = evaluation.State.LastCheckedAt
		state = evaluation.State
		switch state.Phase {
		case old.PhaseHealthy:
			active = true
		case old.PhasePassing, old.PhaseUnhealthy:
			active = false
		}
		if len(state.History) > 20 {
			state.History = append([]old.ProbeRecord(nil), state.History[len(state.History)-20:]...)
		}
		if err := publish(ctx, false, &evaluation.Event); err != nil {
			return fmt.Errorf("health publication: %w", err)
		}
		timer := time.NewTimer(time.Duration(c.IntervalMillis) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
