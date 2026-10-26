package runtimeplan

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

// BFDHealthStatus is calculated when status is read. The embedded protocol
// snapshot remains the last observation, so an old UP observation cannot be
// mistaken for a current grant after its receive deadline or worker exit.
type BFDHealthStatus struct {
	HealthyPeers   []string `json:"healthy_peers"`
	UnhealthyPeers []string `json:"unhealthy_peers"`
	ClockError     bool     `json:"clock_error,omitempty"`
}

type bfdSource struct {
	config   bfd.Config
	snapshot bfd.Snapshot
	update   serviceconfig.Update
	digest   string
	valid    bool
	peers    map[string]int
}

// collectBFD validates each source once even when many BGP neighbors consume
// the same BFD instance. It does not turn an observation into a new lease.
func collectBFD(services []serviceconfig.Service, records map[string]serviceconfig.Update) map[string]bfdSource {
	out := map[string]bfdSource{}
	for _, service := range services {
		if service.BFD == nil {
			continue
		}
		source := bfdSource{config: service.BFDConfig(), update: records[service.ModuleName()]}
		source.digest = source.update.ConfigDigest
		if len(source.update.Snapshot) > 0 && json.Unmarshal(source.update.Snapshot, &source.snapshot) == nil && bfd.ValidateSnapshot(source.config, source.snapshot) == nil {
			source.valid = true
		}
		out[service.Name] = source
	}
	return out
}

func acknowledgedBFD(grant bgpconfig.BFDGrant, proof bgpconfig.BFDAcknowledgement) bool {
	return grant.Up && proof.Enabled && grant.ConfigDigest != "" && grant.Generation != 0 && grant.ProducerEpoch != "" && proof.ConfigDigest == grant.ConfigDigest && proof.Generation == grant.Generation && proof.ProducerEpoch == grant.ProducerEpoch && proof.DownTransitions == grant.DownTransitions
}

func validBFDHex(value string, length int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(value) == length && len(decoded)*2 == length && hex.EncodeToString(decoded) == value
}

func validateBFDAcknowledgements(cfg bgpconfig.Config, snapshot bgpconfig.Snapshot) error {
	if len(snapshot.BFD) > len(cfg.PeerBFD) || len(snapshot.BFD) > bgpconfig.MaxBFDPeers {
		return errors.New("BGP BFD acknowledgement scope exceeds committed peers")
	}
	for peer, proof := range snapshot.BFD {
		if _, exists := cfg.PeerBFD[peer]; !exists {
			return errors.New("BGP acknowledged an unconfigured BFD dependency")
		}
		if proof.ConfigDigest != "" && !validBFDHex(proof.ConfigDigest, 64) || proof.ProducerEpoch != "" && !validBFDHex(proof.ProducerEpoch, 32) {
			return errors.New("BGP BFD acknowledgement has malformed source identity")
		}
		if proof.Enabled && (!snapshot.Active || !validBFDHex(proof.ConfigDigest, 64) || proof.Generation == 0 || !validBFDHex(proof.ProducerEpoch, 32)) {
			return errors.New("BGP enabled a BFD dependency without complete source evidence")
		}
	}
	return nil
}

func (source bfdSource) grant(ref bgpconfig.BFDReference, clock bfd.Clock) bgpconfig.BFDGrant {
	grant := bgpconfig.BFDGrant{Service: ref.Service, Address: ref.Address, ConfigDigest: source.digest}
	if !source.valid {
		return grant
	}
	grant.Generation, grant.ProducerEpoch, grant.Sequence = source.update.Generation, source.update.ProducerEpoch, source.update.Sequence
	grant.BootID = source.snapshot.BootID
	index, found := source.peers[ref.Address]
	if source.peers == nil {
		for i, peer := range source.snapshot.Peers {
			if peer.Address == ref.Address {
				index, found = i, true
				break
			}
		}
	}
	if found && index < len(source.snapshot.Peers) {
		peer := source.snapshot.Peers[index]
		grant.LastReceivedBootNanoseconds = peer.LastReceivedBootNanoseconds
		// The source's own observation deadline also bounds a long negotiated
		// packet interval. Polling must not manufacture a shorter receipt lease.
		grant.DeadlineBootNanoseconds = min(peer.DeadlineBootNanoseconds, int64(source.snapshot.ExpiresNanos))
		grant.DownTransitions = peer.DownTransitions
		grant.Up = source.snapshot.Active && peer.State == bfd.StateUp && source.snapshot.BootID == clock.BootID && clock.BoottimeNanoseconds > 0 && peer.LastReceivedBootNanoseconds <= clock.BoottimeNanoseconds && source.snapshot.ObservedNanos <= uint64(clock.BoottimeNanoseconds) && clock.BoottimeNanoseconds < grant.DeadlineBootNanoseconds
	}
	return grant
}

func (r *Runtime) BFDProjection(name, digest string, generation uint64) (bgpconfig.BFDProjection, error) {
	return r.bfdProjection(context.Background(), name, digest, generation)
}

func (r *Runtime) bfdProjection(ctx context.Context, name, digest string, generation uint64) (bgpconfig.BFDProjection, error) {
	return r.bfdProjectionWithClock(ctx, name, digest, generation, bfd.ReadClock)
}

func (r *Runtime) bfdProjectionWithClock(ctx context.Context, name, digest string, generation uint64, readClock func() (bfd.Clock, error)) (bgpconfig.BFDProjection, error) {
	if err := ctx.Err(); err != nil {
		return bgpconfig.BFDProjection{}, err
	}
	r.bfdMu.Lock()
	if err := ctx.Err(); err != nil {
		r.bfdMu.Unlock()
		return bgpconfig.BFDProjection{}, err
	}
	recipient, exists := r.bfdRecipients[name]
	if !exists || recipient.digest != digest || recipient.generation != generation || generation == 0 {
		r.bfdMu.Unlock()
		return bgpconfig.BFDProjection{}, ErrStale
	}
	cfg := recipient.config
	if len(cfg.PeerBFD) == 0 {
		r.bfdMu.Unlock()
		return bgpconfig.BFDProjection{}, errors.New("BGP worker has no BFD dependencies")
	}
	sources := make(map[string]bfdSource, len(cfg.PeerBFD))
	for _, ref := range cfg.PeerBFD {
		sources[ref.Service] = r.bfdSources["service."+ref.Service].source
	}
	r.bfdMu.Unlock()
	// Capture immutable evidence before its evaluation clock. Sampling the
	// clock first could see a newer receive after waiting for the cache lock,
	// falsely report that sequence Down, then report it Up on the next poll.
	// Clock I/O and JSON validation also stay outside the short cache lock.
	clock, err := readClock()
	if err != nil {
		return bgpconfig.BFDProjection{}, err
	}
	if err := ctx.Err(); err != nil {
		return bgpconfig.BFDProjection{}, err
	}
	projection := bgpconfig.BFDProjection{ConfigDigest: digest, Generation: generation, Peers: map[string]bgpconfig.BFDGrant{}}
	for peer, ref := range cfg.PeerBFD {
		if ref.Address == "" {
			ref.Address = peer
		}
		source := sources[ref.Service]
		projection.Peers[peer] = source.grant(ref, clock)
	}
	if err := bgpconfig.ValidateBFDProjection(cfg, projection, digest, generation); err != nil {
		return bgpconfig.BFDProjection{}, err
	}
	return projection, nil
}

func (r *Runtime) bfdRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/services/{name}/bfd", func(w http.ResponseWriter, req *http.Request) {
		query := req.URL.Query()
		generation, err := strconv.ParseUint(query.Get("generation"), 10, 64)
		if err != nil || generation == 0 || len(query) != 2 || len(query["generation"]) != 1 || len(query["config_digest"]) != 1 || len(query.Get("config_digest")) != 64 {
			localapi.Fail(w, http.StatusBadRequest, errors.New("one config_digest and one positive generation are required"))
			return
		}
		projection, err := r.bfdProjection(req.Context(), req.PathValue("name"), query.Get("config_digest"), generation)
		if err != nil {
			status := http.StatusUnprocessableEntity
			if errors.Is(err, ErrStale) {
				status = http.StatusConflict
			}
			localapi.Fail(w, status, err)
			return
		}
		localapi.JSON(w, http.StatusOK, projection)
	})
}

func validateBFDTransition(previous, update serviceconfig.Update, clock bfd.Clock) error {
	var next bfd.Snapshot
	if err := strict(update.Snapshot, &next); err != nil {
		return err
	}
	var old bfd.Snapshot
	if previous.Generation == update.Generation && previous.ProducerEpoch == update.ProducerEpoch && len(previous.Snapshot) > 0 {
		if err := strict(previous.Snapshot, &old); err != nil {
			return err
		}
	}
	return validateBFDTransitionSnapshots(previous, update, old, next, clock)
}

func validateBFDTransitionSnapshots(previous, update serviceconfig.Update, old, next bfd.Snapshot, clock bfd.Clock) error {
	if next.BootID != clock.BootID || clock.BoottimeNanoseconds <= 0 || next.ObservedNanos > uint64(clock.BoottimeNanoseconds) {
		return errors.New("BFD observation belongs to a different kernel boot")
	}
	for _, p := range next.Peers {
		if p.LastReceivedBootNanoseconds > clock.BoottimeNanoseconds {
			return errors.New("BFD receive observation is in the future")
		}
	}
	if previous.Generation != update.Generation || previous.ProducerEpoch != update.ProducerEpoch || len(previous.Snapshot) == 0 {
		return nil
	}
	if next.ObservedNanos < old.ObservedNanos || next.TimestampChanges < old.TimestampChanges || next.TimestampChanges == old.TimestampChanges && next.TimestampChanges != math.MaxUint64 && !next.UpdatedAt.Equal(old.UpdatedAt) || old.BootID != next.BootID || len(old.Peers) != len(next.Peers) || next.Rejected < old.Rejected || next.ReceiveDropped < old.ReceiveDropped || !old.Active && next.Active {
		return errors.New("BFD producer rewound its identity, final fence or counters")
	}
	for i, p := range next.Peers {
		before := old.Peers[i]
		if p.Address != before.Address || p.LocalDiscriminator != before.LocalDiscriminator || p.SourcePort != before.SourcePort || p.LastReceivedBootNanoseconds < before.LastReceivedBootNanoseconds || p.Transitions < before.Transitions || p.UpTransitions < before.UpTransitions || p.DownTransitions < before.DownTransitions || p.Received < before.Received || p.Transmitted < before.Transmitted || p.TransmitFailures < before.TransmitFailures || p.Rejected < before.Rejected {
			return errors.New("BFD producer rewound a peer identity, receive time or counter")
		}
		if p.Received == before.Received {
			if p.LastReceivedBootNanoseconds != before.LastReceivedBootNanoseconds || !p.LastReceivedAt.Equal(before.LastReceivedAt) || p.ObservedSource != before.ObservedSource || p.RemoteState != before.RemoteState || p.RemoteDiagnostic != before.RemoteDiagnostic || p.RemoteMinTxMicroseconds != before.RemoteMinTxMicroseconds || p.RemoteMinRxMicroseconds != before.RemoteMinRxMicroseconds || p.RemoteMinEchoRxMicroseconds != before.RemoteMinEchoRxMicroseconds || p.RemoteDetectMultiplier != before.RemoteDetectMultiplier || p.DetectionTimeMicroseconds != before.DetectionTimeMicroseconds || p.UpTransitions != before.UpTransitions {
				return errors.New("BFD producer changed receive-derived evidence without a packet")
			}
			if p.DeadlineBootNanoseconds != 0 && p.DeadlineBootNanoseconds != before.DeadlineBootNanoseconds || !p.ValidUntil.IsZero() && !p.ValidUntil.Equal(before.ValidUntil) {
				return errors.New("BFD producer changed its detection deadline without a packet")
			}
			if p.RemoteDiscriminator != before.RemoteDiscriminator {
				// Local expiry may clear a learned remote discriminator even
				// when the session was already Down. It cannot invent another
				// remote identity. uint64 addition avoids signed overflow near
				// the end of the representable BOOTTIME range.
				until := uint64(before.LastReceivedBootNanoseconds) + before.DetectionTimeMicroseconds*1000
				if p.RemoteDiscriminator != 0 || uint64(clock.BoottimeNanoseconds) < until {
					return errors.New("BFD producer changed a live remote discriminator without a packet")
				}
			}
		}
		if before.State == bfd.StateUp && p.State != bfd.StateUp && p.DownTransitions == before.DownTransitions {
			return errors.New("BFD producer omitted a Down transition")
		}
	}
	return nil
}

func bfdHealth(instance serviceconfig.Instance, update serviceconfig.Update) *BFDHealthStatus {
	var cfg bfd.Config
	var snapshot bfd.Snapshot
	if json.Unmarshal(instance.Config, &cfg) != nil {
		return &BFDHealthStatus{HealthyPeers: []string{}, UnhealthyPeers: []string{}, ClockError: true}
	}
	health := &BFDHealthStatus{HealthyPeers: []string{}, UnhealthyPeers: []string{}}
	clock, err := bfd.ReadClock()
	health.ClockError = err != nil
	valid := json.Unmarshal(update.Snapshot, &snapshot) == nil && err == nil
	for _, peer := range cfg.Peers {
		if valid && bfd.Healthy(cfg, snapshot, peer.Address, clock) {
			health.HealthyPeers = append(health.HealthyPeers, peer.Address)
		} else {
			health.UnhealthyPeers = append(health.UnhealthyPeers, peer.Address)
		}
	}
	return health
}
