// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"errors"
	"math"
	"time"
)

// The machine has no I/O and only accepts explicit CLOCK_BOOTTIME samples.
// Randomness is injected so scheduling and negotiation boundaries are testable.
type session struct {
	config                                            Peer
	local, remote                                     uint32
	state, remoteState, diagnostic, remoteDiagnostic  uint8
	remoteTx, remoteRx, remoteEcho                    uint32
	remoteMult                                        uint8
	sourcePort                                        uint16
	observedSource                                    string
	polling, pollSent                                 bool
	lastRx                                            Clock
	lastTx, nextTx, deadline, remoteDeadline          int64
	transitions, upTransitions, downTransitions       uint64
	received, transmitted, transmitFailures, rejected uint64
	jitter                                            func(uint32) uint32
}

func newSession(c Peer, discriminator uint32, sourcePort uint16, now int64, jitter func(uint32) uint32) *session {
	return &session{config: c, local: discriminator, sourcePort: sourcePort, state: wireDown, remoteState: wireDown, remoteRx: 1, nextTx: now, jitter: jitter}
}

func increment(n *uint64) {
	if *n != math.MaxUint64 {
		*n++
	}
}
func (s *session) desiredTx() uint32 {
	n := uint32(s.config.MinTxMilliseconds) * 1000
	if s.state != wireUp {
		n = max(n, 1_000_000)
	}
	return n
}
func (s *session) requiredRx() uint32 { return uint32(s.config.MinRxMilliseconds) * 1000 }
func (s *session) txInterval() uint32 { return max(s.desiredTx(), s.remoteRx) }
func (s *session) detectionTime() uint64 {
	return uint64(max(s.requiredRx(), s.remoteTx)) * uint64(s.remoteMult)
}

func addInterval(at int64, microseconds uint64) (int64, error) {
	if at <= 0 || microseconds == 0 || microseconds > uint64(math.MaxInt64-at)/1000 {
		return 0, errors.New("BFD monotonic deadline overflow")
	}
	return at + int64(microseconds*1000), nil
}

func (s *session) jitteredInterval() int64 {
	span := uint32(250001)
	if s.config.DetectMultiplier == 1 {
		span = 150001
	}
	// 75..100%, or 75..90% when Detect Mult is 1, independently per packet.
	fraction := uint64(750000 + s.jitter(span))
	return int64(uint64(s.txInterval()) * 1000 * fraction / 1_000_000)
}

func (s *session) schedule(now int64) {
	if s.remoteRx == 0 {
		s.nextTx = 0
		return
	}
	if s.lastTx == 0 {
		s.nextTx = now
		return
	}
	interval := s.jitteredInterval()
	if s.lastTx > math.MaxInt64-interval {
		s.nextTx = math.MaxInt64
		return
	}
	s.nextTx = s.lastTx + interval
	if s.nextTx < now {
		s.nextTx = now
	}
}

func (s *session) enter(state, diagnostic uint8, now int64) bool {
	if s.state == state {
		return false
	}
	oldTx := s.desiredTx()
	if s.state == wireUp && state != wireUp {
		increment(&s.downTransitions)
	}
	if state == wireUp {
		increment(&s.upTransitions)
	}
	s.state, s.diagnostic = state, diagnostic
	increment(&s.transitions)
	if state != wireInit && state != wireUp {
		s.deadline = 0
	}
	if s.desiredTx() != oldTx {
		s.polling, s.pollSent = true, false
	}
	s.schedule(now)
	// A state change is an explicitly permitted transmission between periodic
	// packets. Remote RequiredMinRx=0 still suppresses periodic transmission.
	if s.remoteRx != 0 {
		s.nextTx = now
	}
	return true
}

func (s *session) expire(now int64) bool {
	if s.remoteDeadline == 0 || now < s.remoteDeadline {
		return false
	}
	// Remote discriminator expires even when the local state is already Down.
	// Keep the other negotiated timing facts across flaps (RFC 5880 6.8.1).
	s.remote, s.remoteDeadline = 0, 0
	if s.state == wireInit || s.state == wireUp {
		s.enter(wireDown, 1, now)
	}
	return true
}

func (s *session) receive(p control, source string, received Clock, now int64) (changed, final bool, err error) {
	if p.your != 0 && p.your != s.local {
		return false, false, errors.New("BFD discriminator does not identify the selected session")
	}
	if received.BoottimeNanoseconds < s.lastRx.BoottimeNanoseconds || received.BoottimeNanoseconds > now {
		return false, false, errors.New("BFD datagram has an invalid receive timestamp")
	}
	detection := uint64(max(s.requiredRx(), p.minTx)) * uint64(p.multiplier)
	deadline, err := addInterval(received.BoottimeNanoseconds, detection)
	if err != nil || now >= deadline {
		return false, false, errors.New("BFD queued datagram is already expired")
	}
	oldInterval := s.txInterval()
	oldRx := s.remoteRx
	oldRemote, oldState, oldTx, oldMult, oldEcho, oldPolling := s.remote, s.remoteState, s.remoteTx, s.remoteMult, s.remoteEcho, s.polling
	s.remote, s.remoteState, s.remoteDiagnostic = p.my, p.state, p.diagnostic
	s.remoteTx, s.remoteRx, s.remoteEcho, s.remoteMult = p.minTx, p.minRx, p.minEchoRx, p.multiplier
	s.observedSource, s.lastRx = source, received
	s.remoteDeadline = deadline
	increment(&s.received)
	if p.flags&flagFinal != 0 && s.polling && s.pollSent {
		s.polling, s.pollSent = false, false
	}
	switch {
	case s.state == wireAdminDown:
		return false, false, nil
	case p.state == wireAdminDown:
		changed = s.enter(wireDown, 3, now)
	case s.state == wireDown && p.state == wireDown:
		changed = s.enter(wireInit, 0, now)
	case s.state == wireDown && p.state == wireInit:
		changed = s.enter(wireUp, 0, now)
	case s.state == wireInit && (p.state == wireInit || p.state == wireUp):
		changed = s.enter(wireUp, 0, now)
	case s.state == wireUp && p.state == wireDown:
		changed = s.enter(wireDown, 3, now)
	}
	if s.state == wireInit || s.state == wireUp {
		s.deadline = deadline
	}
	if !changed && (oldInterval != s.txInterval() || oldRx != s.remoteRx) {
		s.schedule(now)
	}
	// A faster negotiated receive deadline or completed Poll must be published
	// immediately. Otherwise an old 200ms refresh cadence could outlive a newly
	// negotiated 50ms detection interval without ever publishing that Up grant.
	statusChanged := changed || oldRemote != s.remote || oldState != s.remoteState || oldTx != s.remoteTx || oldRx != s.remoteRx || oldMult != s.remoteMult || oldEcho != s.remoteEcho || oldPolling != s.polling
	return statusChanged, p.flags&flagPoll != 0, nil
}

func (s *session) packet(final bool) control {
	flags := uint8(0)
	if final {
		flags = flagFinal
	} else if s.polling {
		flags = flagPoll
	}
	return control{state: s.state, diagnostic: s.diagnostic, flags: flags, multiplier: uint8(s.config.DetectMultiplier), my: s.local, your: s.remote, minTx: s.desiredTx(), minRx: s.requiredRx()}
}

func (s *session) sent(now int64, final, success bool) {
	if success {
		increment(&s.transmitted)
		if !final && s.polling {
			s.pollSent = true
		}
	} else {
		increment(&s.transmitFailures)
	}
	if !final {
		s.lastTx = now
		s.schedule(now)
	}
}

func (s *session) snapshot(active bool) PeerState {
	p := PeerState{Address: s.config.Address, ObservedSource: s.observedSource, State: stateName(s.state), RemoteState: stateName(s.remoteState), Diagnostic: s.diagnostic, RemoteDiagnostic: s.remoteDiagnostic, LocalDiscriminator: s.local, RemoteDiscriminator: s.remote, SourcePort: s.sourcePort, DesiredMinTxMicroseconds: s.desiredTx(), RequiredMinRxMicroseconds: s.requiredRx(), RemoteMinTxMicroseconds: s.remoteTx, RemoteMinRxMicroseconds: s.remoteRx, RemoteMinEchoRxMicroseconds: s.remoteEcho, RemoteDetectMultiplier: s.remoteMult, TransmitIntervalMicroseconds: s.txInterval(), DetectionTimeMicroseconds: s.detectionTime(), Polling: s.polling, LastReceivedAt: s.lastRx.WallTime, LastReceivedBootNanoseconds: s.lastRx.BoottimeNanoseconds, Transitions: s.transitions, UpTransitions: s.upTransitions, DownTransitions: s.downTransitions, Received: s.received, Transmitted: s.transmitted, TransmitFailures: s.transmitFailures, Rejected: s.rejected}
	if active && (s.state == wireInit || s.state == wireUp) {
		p.DeadlineBootNanoseconds = s.deadline
		p.ValidUntil = s.lastRx.WallTime.Add(time.Duration(s.detectionTime()) * time.Microsecond)
	}
	return p
}
