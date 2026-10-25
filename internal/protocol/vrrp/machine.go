package vrrp

import (
	"net/netip"
	"time"
)

type actions struct {
	update    bool
	advertise bool
	notify    bool
}

// machine contains only deterministic election state. The runtime executes an
// ownership update before the advertisement/notification actions and terminates
// with withdrawal if an acknowledged effect or send fails.
type machine struct {
	bootID                string
	observedNanos         uint64
	expiresNanos          uint64
	observationTransition uint64
	observationActive     bool
	config                Config
	source                netip.Addr
	epoch                 string
	state                 string
	transition            uint64
	activeInterval        time.Duration
	deadline              time.Time
	validUntil            time.Time
	peer                  netip.Addr
	peerPriority          uint8
	advertisements        uint64
	received              uint64
	rejected              uint64
	receiveDropped        uint64
	intervalMismatch      uint64
	lastError             string
}

func newMachine(c Config, source netip.Addr, epoch string) *machine {
	c = Normalize(c)
	return &machine{config: c, source: source, epoch: epoch, state: StateInitialize, activeInterval: c.interval()}
}

func (m *machine) skew() time.Duration {
	return time.Duration(256-m.config.Priority) * m.activeInterval / 256
}

func (m *machine) downInterval() time.Duration { return 3*m.activeInterval + m.skew() }

func (m *machine) enter(state string, now time.Time) {
	m.state = state
	m.transition++
	switch state {
	case StateActive:
		m.deadline = now.Add(m.config.interval())
		m.peer, m.peerPriority = netip.Addr{}, 0
	case StateBackup:
		m.deadline = now.Add(m.downInterval())
	default:
		m.deadline = time.Time{}
	}
	if state != StateActive {
		m.validUntil = time.Time{}
	}
}

func (m *machine) start(now time.Time) actions {
	if m.config.AddressOwner {
		m.enter(StateActive, now)
		return actions{update: true, advertise: true, notify: true}
	}
	m.enter(StateBackup, now)
	return actions{update: true}
}

func (m *machine) tick(now time.Time) actions {
	if m.deadline.IsZero() || now.Before(m.deadline) {
		return actions{}
	}
	if m.state == StateBackup {
		m.enter(StateActive, now)
		return actions{update: true, advertise: true, notify: true}
	}
	if m.state == StateActive {
		// Schedule from now, not the old deadline: do not burst delayed packets
		// after a pause in the process or its ownership reconciler.
		m.deadline = now.Add(m.config.interval())
		return actions{advertise: true}
	}
	return actions{}
}

func (m *machine) receive(now time.Time, p advertisement) actions {
	if p.source == m.source {
		return actions{}
	}
	if m.config.AddressOwner {
		m.rejected++
		m.lastError = "address owner discarded a peer advertisement"
		return actions{}
	}
	m.received++
	if p.interval != m.config.interval() {
		m.intervalMismatch++
	}
	if m.state == StateBackup {
		if p.priority == 0 {
			m.peer, m.peerPriority = p.source, p.priority
			m.deadline = now.Add(m.skew())
		} else if !m.config.preempt() || int(p.priority) >= m.config.Priority {
			m.peer, m.peerPriority = p.source, p.priority
			m.activeInterval = p.interval
			m.deadline = now.Add(m.downInterval())
		}
		return actions{}
	}
	if m.state == StateActive {
		if p.priority == 0 {
			m.deadline = now.Add(m.config.interval())
			return actions{advertise: true}
		}
		if int(p.priority) > m.config.Priority || int(p.priority) == m.config.Priority && p.source.Compare(m.source) > 0 {
			m.activeInterval = p.interval
			m.peer, m.peerPriority = p.source, p.priority
			m.enter(StateBackup, now)
			return actions{update: true}
		}
		// RFC 9568's immediate assertion also refreshes learning bridges.
		// Keep the existing periodic deadline for nonzero inferior peers.
		return actions{advertise: true}
	}
	return actions{}
}

func (m *machine) snapshot() Snapshot {
	peer := ""
	if m.peer.IsValid() {
		peer = m.peer.String()
	}
	return Snapshot{
		BootID: m.bootID, ObservedNanos: m.observedNanos, ExpiresNanos: m.expiresNanos,
		Resource: m.config.Resource, Interface: m.config.Interface, VIPInterface: m.config.VIPInterface, VRF: m.config.VRF,
		Family: m.config.Family, SourceAddress: m.source.String(), VirtualRouterID: m.config.VirtualRouterID,
		VirtualMAC: VirtualMAC(m.config.Family, m.config.VirtualRouterID).String(), Addresses: append([]string(nil), m.config.Addresses...),
		Active: m.state == StateActive, State: m.state, AcceptMode: m.config.AcceptMode || m.config.AddressOwner, AddressOwner: m.config.AddressOwner,
		Epoch: m.epoch, Transition: m.transition, Priority: m.config.Priority, Peer: peer, PeerPriority: int(m.peerPriority),
		ActiveIntervalCS: int(m.activeInterval / (10 * time.Millisecond)), Deadline: m.deadline, ValidUntil: m.validUntil,
		Advertisements: m.advertisements, Received: m.received, Rejected: m.rejected, ReceiveDropped: m.receiveDropped,
		IntervalMismatch: m.intervalMismatch, LastError: m.lastError,
	}
}
