package runtimeplan

import (
	"github.com/imksoo/routerd2/internal/derived"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
)

// statusValuesLocked reads the accepted typed providers for both tracking and
// derived assertions. Caller holds mu; values keep original producer deadlines.
func (r *Runtime) statusValuesLocked(facts resourceprogram.FactSnapshot, refs []derived.Reference) (bfd.Clock, map[string]derived.Value, error) {
	// BFD publishes independently of mu. Capture its immutable evidence before
	// the clock, so a newer receive cannot appear to come from the future after
	// waiting for the cache lock. Never hold bfdMu during clock I/O/evaluation.
	r.bfdMu.Lock()
	bfdSources := make(map[string]bfdBinding, len(r.bfdSources))
	for name, owner := range r.bfdSources {
		bfdSources[name] = owner
	}
	r.bfdMu.Unlock()
	clock, err := bfd.ReadClock()
	if err != nil {
		return bfd.Clock{}, nil, err
	}
	now := uint64(clock.BoottimeNanoseconds)
	lifecycles := r.config.plan.Lifecycle(facts)
	type observed struct {
		known       bool
		snapshot    health.Snapshot
		bfdSnapshot bfd.Snapshot
		bfdConfig   bfd.Config
		isBFD       bool
		isBGP       bool
		bgpSnapshot bgpconfig.Snapshot
		bgpConfig   bgpconfig.Config
		unresolved  int
		origin      string
		until       uint64
	}
	providers := map[string]observed{}
	values := map[string]derived.Value{}
	for _, ref := range refs {
		key := ref.Key()
		provider, loaded := providers[ref.Resource]
		if !loaded {
			target := r.config.resources[ref.Resource]
			if target.Service != nil && target.Service.Health != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guard.modules[name]
				record := r.records[name]
				grant, granted := r.guardGrants[name]
				if exists && managed && granted && module.Ready && module.Generation == record.Generation && module.Generation == grant.generation && module.ConfigDigest == binding.digest && record.ConfigDigest == binding.digest && grant.revision == r.config.revision && grant.epoch == record.ProducerEpoch && grant.sequence == record.Sequence && clock.BootID == r.guard.bootID && clock.BootID == grant.bootID && now < r.guard.untilNanos {
					if grant.health != nil && !grant.health.Stopped && grant.health.BootID == clock.BootID {
						provider.snapshot = *grant.health
						provider.until = min(provider.snapshot.ExpiresNanos, grant.untilNanos, r.guard.untilNanos)
						provider.known = provider.snapshot.ObservedNanos <= now && provider.until > now && provider.until > provider.snapshot.ObservedNanos
						provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch)
					}
				}
			}
			if target.Service != nil && target.Service.BFD != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guard.modules[name]
				owner, cached := bfdSources[name]
				record := owner.source.update
				if exists && cached && owner.source.valid && managed && module.Ready && module.Generation == record.Generation && module.ConfigDigest == binding.digest && record.ConfigDigest == binding.digest && owner.binding.digest == binding.digest && clock.BootID == r.guard.bootID && now < r.guard.untilNanos {
					provider.isBFD = true
					provider.bfdConfig = owner.source.config
					provider.bfdSnapshot = owner.source.snapshot
					provider.until = r.guard.untilNanos
					provider.known = true
					provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch)
				}
			}
			if target.Service != nil && target.Service.BGP != nil && lifecycles[ref.Resource].Active {
				name := target.Service.ModuleName()
				binding, exists := r.bindings[name]
				module, managed := r.guard.modules[name]
				record := r.records[name]
				grant, granted := r.guardGrants[name]
				applied, matched := r.bgpApplied[name]
				// A matching BGP receipt survives unrelated kernel IO. A changed RIB
				// matches this identity only after its own successful application.
				if exists && managed && granted && grant.bgp != nil && matched && module.Ready && module.Generation == record.Generation && grant.generation == record.Generation && record.ConfigDigest == binding.digest && module.ConfigDigest == binding.digest && grant.revision == r.config.revision && grant.epoch == record.ProducerEpoch && grant.sequence == record.Sequence && clock.BootID == r.guard.bootID && clock.BootID == grant.bootID && now < r.guard.untilNanos && applied.identity == grant.bgp.projection {
					provider.isBGP, provider.known = true, true
					provider.bgpSnapshot = grant.bgp.snapshot
					provider.bgpConfig = r.config.protocols.bgp[name]
					provider.unresolved = applied.unresolved
					provider.until = min(grant.untilNanos, r.guard.untilNanos)
					provider.origin = identity(clock.BootID, binding.digest, module.Generation, record.ProducerEpoch, provider.bgpSnapshot.Epoch)
				}
			}
			providers[ref.Resource] = provider
		}
		value := derived.Value{}
		if provider.known && provider.isBGP {
			field, known := bgpconfig.CurrentStatus(provider.bgpConfig, provider.bgpSnapshot, ref.Field, clock, provider.unresolved)
			value = derived.Value{Text: field.Text, Known: known, Origin: provider.origin, ObservedNanos: field.ObservedNanos, ExpiresNanos: min(field.ExpiresNanos, provider.until)}
		} else if provider.known && provider.isBFD {
			field, known := bfd.CurrentStatus(provider.bfdConfig, provider.bfdSnapshot, ref.Field, clock)
			value = derived.Value{Text: field.Text, Known: known, Origin: provider.origin, Continuity: field.Continuity, EqualityContinuity: field.EqualityContinuity, ObservedNanos: field.ObservedNanos, ExpiresNanos: min(field.ExpiresNanos, provider.until)}
		} else if provider.known {
			text, known := health.ScalarStatus(provider.snapshot, ref.Field)
			value = derived.Value{Text: text, Known: known, Origin: provider.origin, ObservedNanos: provider.snapshot.ObservedNanos, ExpiresNanos: provider.until}
		}
		values[key] = value
	}
	return clock, values, nil
}
