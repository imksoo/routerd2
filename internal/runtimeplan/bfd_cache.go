package runtimeplan

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

type bfdBinding struct {
	binding binding
	source  bfdSource
}

type bfdRecipient struct {
	digest     string
	generation uint64
	// Only the bounded dependency references are needed in a health query.
	// The full BGP config is decoded once during configuration cutover.
	config bgpconfig.Config
}

// Compile outside bfdMu. Even a large BGP configuration or filesystem-backed
// instance preparation must not delay an existing BFD publisher or consumer.
func compileBFDCache(bindings map[string]binding) (map[string]bfdBinding, map[string]bfdRecipient, error) {
	sources := map[string]bfdBinding{}
	recipients := map[string]bfdRecipient{}
	for name, owner := range bindings {
		switch owner.instance.Role {
		case "bfd":
			var cfg bfd.Config
			if err := strict(owner.instance.Config, &cfg); err != nil {
				return nil, nil, err
			}
			if err := bfd.Validate(cfg); err != nil {
				return nil, nil, err
			}
			source := bfdSource{config: bfd.Normalize(cfg), digest: owner.digest, peers: map[string]int{}}
			for i, peer := range source.config.Peers {
				source.peers[peer.Address] = i
			}
			sources[name] = bfdBinding{binding: owner, source: source}
		case "bgp":
			var cfg bgpconfig.Config
			if err := strict(owner.instance.Config, &cfg); err != nil {
				return nil, nil, err
			}
			references := make(map[string]bgpconfig.BFDReference, len(cfg.PeerBFD))
			for peer, ref := range cfg.PeerBFD {
				if ref.Address == "" {
					ref.Address = peer
				}
				references[peer] = ref
			}
			recipients[name] = bfdRecipient{digest: owner.digest, config: bgpconfig.Config{PeerBFD: references}}
		}
	}
	return sources, recipients, nil
}

// Caller holds mu. Accepted BFD state lives only in this short-lock cache;
// retained projection copies may have become older while configuration was
// compiled. Preserve the current matching source, never that retained copy.
func (r *Runtime) installBFDCacheLocked(sources map[string]bfdBinding, recipients map[string]bfdRecipient) {
	r.bfdMu.Lock()
	defer r.bfdMu.Unlock()
	for name, next := range sources {
		if old, exists := r.bfdSources[name]; exists && old.binding.digest == next.binding.digest {
			next.source = old.source
			sources[name] = next
		}
	}
	for name, recipient := range recipients {
		if accepted, ok := r.records[name]; ok && accepted.Role == "bgp" && accepted.ConfigDigest == recipient.digest {
			recipient.generation = accepted.Generation
			recipients[name] = recipient
		}
	}
	r.bfdSources, r.bfdRecipients = sources, recipients
	for name, record := range r.records {
		if record.Role == "bfd" {
			delete(r.records, name)
		}
	}
}

// Caller holds mu. The returned record bodies are immutable accepted copies.
// A pure projection uses this point-in-time set; later BFD changes reach the
// existing single network writer through its next reconciliation.
func (r *Runtime) mergeBFDRecordsLocked(records map[string]serviceconfig.Update) {
	r.bfdMu.Lock()
	defer r.bfdMu.Unlock()
	for name, owner := range r.bfdSources {
		if owner.source.valid {
			records[name] = owner.source.update
		}
	}
}

func (r *Runtime) bfdRecord(name, digest string) serviceconfig.Update {
	r.bfdMu.Lock()
	defer r.bfdMu.Unlock()
	if owner, exists := r.bfdSources[name]; exists && owner.binding.digest == digest && owner.source.valid {
		return owner.source.update
	}
	return serviceconfig.Update{}
}

// Caller holds mu and the BGP publication has succeeded. A failed or pending
// network apply must not authorize a new recipient generation in this cache.
func (r *Runtime) acceptBFDRecipientLocked(update serviceconfig.Update) {
	r.bfdMu.Lock()
	defer r.bfdMu.Unlock()
	if recipient, exists := r.bfdRecipients[update.Name]; exists && recipient.digest == update.ConfigDigest {
		recipient.generation = update.Generation
		r.bfdRecipients[update.Name] = recipient
	}
}

func (r *Runtime) acceptBFD(ctx context.Context, update serviceconfig.Update) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var next bfd.Snapshot
	if err := strict(update.Snapshot, &next); err != nil {
		return err
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		return fmt.Errorf("read BFD kernel clock: %w", err)
	}
	r.bfdMu.Lock()
	defer r.bfdMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, exists := r.bfdSources[update.Name]
	if !exists || owner.binding.digest != update.ConfigDigest {
		return ErrStale
	}
	previous := owner.source.update
	if owner.source.valid {
		if update.Generation < previous.Generation || update.Generation == previous.Generation && update.ProducerEpoch != previous.ProducerEpoch {
			return ErrStale
		}
		if update.Generation == previous.Generation {
			if update.Sequence < previous.Sequence {
				return ErrStale
			}
			if update.Sequence == previous.Sequence {
				if bytes.Equal(update.Snapshot, previous.Snapshot) {
					return nil
				}
				return errors.New("protocol sequence reused with different snapshot")
			}
		}
	}
	if err := bfd.ValidateSnapshot(owner.source.config, next); err != nil {
		return err
	}
	if err := validateBFDTransitionSnapshots(previous, update, owner.source.snapshot, next, clock); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep raw status and typed grant evidence immutable. The HTTP request or an
	// in-process caller cannot mutate a previously acknowledged source record.
	update.Snapshot = append([]byte(nil), update.Snapshot...)
	owner.source.update, owner.source.snapshot, owner.source.valid = update, next, true
	r.bfdSources[update.Name] = owner
	return nil
}

// An individual BFD status query uses the same short cache as protocol health
// queries and stays available while another module waits for a kernel apply.
func (r *Runtime) bfdInstanceStatus(name string, detail bool) (InstanceStatus, bool) {
	r.bfdMu.Lock()
	owner, exists := r.bfdSources[name]
	r.bfdMu.Unlock()
	if !exists {
		return InstanceStatus{}, false
	}
	return instanceStatus(name, owner.binding, owner.source.update, detail), true
}
