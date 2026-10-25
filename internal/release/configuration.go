package release

import (
	"errors"
	"github.com/imksoo/routerd2/internal/configref"
)

func cloneStartup(s *configref.Selection) *configref.Selection {
	if s == nil {
		return nil
	}
	v := s.Clone()
	return &v
}
func sameStartup(a, b *configref.Selection) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// prepareStartup reserves both the candidate and possible recovery generation.
// Cancel/failure never reset these fences or the logical configuration store.
// Filesystem verification, compatibility and live-writer fencing belong to the
// coordinator before this pure release-journal transaction is invoked.
func prepareStartup(state *State, pending *Pending, options PrepareOptions) error {
	if options.Startup == nil {
		if options.KnownGood != nil || state.ConfigStoreID != "" {
			return ErrConfigurationRequired
		}
		return nil
	}
	candidate := options.Startup.Clone()
	if err := candidate.Validate(); err != nil {
		return err
	}
	if candidate.Recovery != nil || candidate.Generation == ^uint64(0) || candidate.ExpectedRevision > ^uint64(0)-2 {
		return errors.New("candidate must reserve ordinary adoption and conditional recovery capacity")
	}
	if candidate.Generation <= state.ConfigHighWatermark {
		return ErrStale
	}
	if state.ConfigStoreID != "" && state.ConfigStoreID != candidate.Snapshot.StoreID {
		return errors.New("configuration store identity cannot change with a runtime update")
	}
	if state.Active == nil {
		if options.KnownGood != nil {
			return errors.New("initial runtime has no declared healthy fallback")
		}
	} else {
		if options.KnownGood == nil {
			return ErrConfigurationRequired
		}
		known := *options.KnownGood
		if err := known.Validate(); err != nil {
			return err
		}
		if known.StoreID != candidate.Snapshot.StoreID || known.Revision != candidate.ExpectedRevision {
			return errors.New("known-good snapshot must bind the existing store and exact prepared revision")
		}
		fallback := configref.Selection{Generation: candidate.Generation + 1, ExpectedRevision: candidate.ExpectedRevision, Snapshot: known, Recovery: &configref.Recovery{FailedGeneration: candidate.Generation, FailedSnapshot: candidate.Snapshot}}
		if err := fallback.Validate(); err != nil {
			return err
		}
		pending.Fallback = &fallback
	}
	pending.Startup = &candidate
	state.ConfigStoreID = candidate.Snapshot.StoreID
	state.ConfigHighWatermark = candidate.Generation + 1
	return nil
}

func validateConfigurationState(state State) error {
	var selected []*configref.Selection
	selected = append(selected, state.Startup, state.PreviousStartup)
	if state.Pending != nil {
		selected = append(selected, state.Pending.Startup, state.Pending.Fallback)
	}
	if state.LastFailure != nil {
		selected = append(selected, state.LastFailure.Startup)
	}
	if state.ConfigHighWatermark == 0 {
		if state.ConfigStoreID != "" {
			return errors.New("configuration identity without watermark")
		}
		for _, s := range selected {
			if s != nil {
				return errors.New("configuration reference without authority")
			}
		}
		return nil
	}
	if state.Generation == 0 || !configref.ValidHex(state.ConfigStoreID, 32) {
		return errors.New("invalid configuration authority")
	}
	for _, s := range selected {
		if s == nil {
			continue
		}
		if err := s.Validate(); err != nil {
			return err
		}
		if s.Snapshot.StoreID != state.ConfigStoreID || s.Generation > state.ConfigHighWatermark {
			return errors.New("configuration reference differs from durable authority")
		}
	}
	if state.Startup != nil && state.Active == nil || state.PreviousStartup != nil && state.Previous == nil {
		return errors.New("configuration without selected runtime")
	}
	if state.Pending != nil {
		p := state.Pending
		if p.Startup == nil {
			return errors.New("bound configuration authority cannot accept runtime-only pending updates")
		}
		if p.Startup.Recovery != nil || p.Startup.Generation == ^uint64(0) || p.Startup.Generation+1 != state.ConfigHighWatermark {
			return errors.New("invalid candidate configuration generation")
		}
		baseline := state.Active != nil
		if state.Attempt != nil {
			baseline = state.Previous != nil
		}
		if baseline {
			if p.Fallback == nil {
				return errors.New("paired update has no compatible configuration fallback")
			}
			want := configref.Selection{Generation: p.Startup.Generation + 1, ExpectedRevision: p.Startup.ExpectedRevision, Snapshot: p.Fallback.Snapshot, Recovery: &configref.Recovery{FailedGeneration: p.Startup.Generation, FailedSnapshot: p.Startup.Snapshot}}
			if !p.Fallback.Equal(want) {
				return errors.New("fallback is not fenced to its exact candidate")
			}
		} else if p.Fallback != nil {
			return errors.New("fallback has no previously healthy runtime")
		}
		if state.Attempt != nil && (!sameStartup(state.Startup, p.Startup) || !sameStartup(state.PreviousStartup, p.Fallback)) {
			return errors.New("trial runtime/configuration tuple differs from prepared intent")
		}
	}
	return nil
}
