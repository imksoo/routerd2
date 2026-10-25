package vrrp

import (
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
)

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

func parseActivationTimeout(c Config) (time.Duration, error) {
	return time.ParseDuration(Normalize(c).GracefulActivation.Timeout)
}
