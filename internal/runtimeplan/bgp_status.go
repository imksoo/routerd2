package runtimeplan

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type bgpProjectionStatus struct {
	identity   string
	unresolved int
}

type bgpProvider struct {
	snapshot   bgpconfig.Snapshot
	projection string
}

// Raw route and BFD data are hashed without allocating every route again for
// status reads. Clock/peer-only heartbeat changes preserve applied FIB proof.
func bgpProviderFor(u serviceconfig.Update) (bgpProvider, error) {
	var wire struct {
		bgpconfig.Snapshot
		Routes json.RawMessage `json:"routes"`
		BFD    json.RawMessage `json:"bfd"`
	}
	if err := json.Unmarshal(u.Snapshot, &wire); err != nil {
		return bgpProvider{}, err
	}
	return bgpProvider{snapshot: wire.Snapshot, projection: identity(u.ConfigDigest, u.Generation, u.ProducerEpoch, wire.Epoch, wire.Routes, wire.BFD)}, nil
}

func validateBGPTransition(previous, next serviceconfig.Update) error {
	var s bgpconfig.Snapshot
	if err := json.Unmarshal(next.Snapshot, &s); err != nil {
		return err
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		return err
	}
	if s.BootID != clock.BootID || s.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return errors.New("BGP observation belongs to a different boot or future clock")
	}
	if previous.Generation != next.Generation || previous.ProducerEpoch != next.ProducerEpoch || len(previous.Snapshot) == 0 {
		return nil
	}
	var old bgpconfig.Snapshot
	if err := json.Unmarshal(previous.Snapshot, &old); err != nil {
		return err
	}
	if old.Epoch != s.Epoch || old.BootID != s.BootID || s.Sequence < old.Sequence || s.ObservedNanos < old.ObservedNanos || !old.Active && s.Active {
		return errors.New("BGP producer rewound its observation or final fence")
	}
	if old.Sequence == s.Sequence && !bytes.Equal(previous.Snapshot, next.Snapshot) {
		return errors.New("BGP inner sequence reused with changed observation")
	}
	return nil
}
