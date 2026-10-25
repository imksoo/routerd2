package runtimeplan

import (
	"errors"

	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

var ErrVRRPActivationPending = errors.New("VRRP awaits admitted VIP publication")

// An applied election keeps ownership when its admission condition later drops.
// A different producer, configuration, boot or election must be admitted again.
type vrrpOwnership struct {
	generation, transition                             uint64
	producerEpoch, electionEpoch, configDigest, bootID string
}

func ownershipFor(u serviceconfig.Update, s vrrp.Snapshot) vrrpOwnership {
	return vrrpOwnership{generation: u.Generation, transition: s.Transition, producerEpoch: u.ProducerEpoch, electionEpoch: s.Epoch, configDigest: u.ConfigDigest, bootID: s.BootID}
}

func (r *Runtime) ownsVRRPLocked(u serviceconfig.Update, s vrrp.Snapshot) bool {
	owner, ok := r.vrrpOwners[u.Name]
	return ok && s.State == vrrp.StateActive && owner == ownershipFor(u, s)
}
