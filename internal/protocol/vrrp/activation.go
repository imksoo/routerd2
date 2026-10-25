package vrrp

import (
	"context"
	"errors"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

// ActivationGrant is a short admission permission, not a renewable ownership
// lease. The coordinator binds its ID to the current producer/source/election
// and original condition deadline; only that recipient can request publication.
type ActivationGrant struct {
	ID            uint64 `json:"id,omitempty"`
	BootID        string `json:"boot_id,omitempty"`
	ElectionEpoch string `json:"election_epoch,omitempty"`
	Transition    uint64 `json:"transition,omitempty"`
	ObservedNanos uint64 `json:"observed_nanos,omitempty"`
	ExpiresNanos  uint64 `json:"expires_nanos,omitempty"`
}

type ActivationAuthority interface {
	GrantActivation(context.Context, Snapshot) (ActivationGrant, error)
}

func (g ActivationGrant) ValidFor(s Snapshot, clock boottime.Clock) bool {
	now := uint64(clock.BoottimeNanoseconds)
	return g.ID != 0 && clock.BoottimeNanoseconds > 0 && g.BootID == s.BootID && g.BootID == clock.BootID && g.ElectionEpoch == s.Epoch && g.Transition == s.Transition && g.ObservedNanos == s.ObservedNanos && g.ObservedNanos <= now && now < g.ExpiresNanos && g.ExpiresNanos <= s.ExpiresNanos && s.State == StateActive
}

func (m *machine) activationStatus(clock boottime.Clock) {
	if m.config.GracefulActivation == nil || m.state != StateActive {
		return
	}
	if m.activationStartedNanos == 0 {
		m.activationStartedNanos = uint64(clock.BoottimeNanoseconds)
	}
	m.activationState = "Preparing"
	if m.published {
		m.activationState = "Ready"
		return
	}
	timeout, _ := parseActivationTimeout(m.config)
	if uint64(clock.BoottimeNanoseconds) >= m.activationStartedNanos && uint64(clock.BoottimeNanoseconds)-m.activationStartedNanos >= uint64(timeout) {
		m.activationState = "ReadinessTimeout"
	}
}

func (m *machine) requestActivation(ctx context.Context, effects Effects) (bool, error) {
	if m.config.GracefulActivation == nil || m.state != StateActive || m.published {
		return false, nil
	}
	authority, ok := effects.(ActivationAuthority)
	if !ok {
		return false, errors.New("graceful VRRP requires an activation authority")
	}
	grant, err := authority.GrantActivation(ctx, m.snapshot())
	if err != nil {
		return false, err
	}
	if grant.ID == 0 {
		return false, nil
	}
	clock, err := boottime.Read()
	if err != nil || !grant.ValidFor(m.snapshot(), clock) {
		return false, errors.Join(errors.New("invalid or expired VRRP activation grant"), err)
	}
	m.activationGrant = grant
	m.published = true
	m.activationState = "Ready"
	return true, nil
}

func parseActivationTimeout(c Config) (time.Duration, error) {
	return time.ParseDuration(Normalize(c).GracefulActivation.Timeout)
}
