package control

import (
	"context"
	"fmt"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/release"
)

func openSelectedConfiguration(ctx context.Context, dir, seed string, startup *configref.Selection, authority *release.Authority) (*config.Store, config.Committed, *release.Store, error) {
	if authority == nil {
		s, c, err := config.OpenSelected(dir, seed, startup)
		return s, c, nil, err
	}
	if err := authority.Validate(); err != nil {
		return nil, config.Committed{}, nil, err
	}
	keys, err := release.LoadTrustFile(authority.TrustFile)
	if err != nil {
		return nil, config.Committed{}, nil, err
	}
	root, err := release.Open(release.Options{Root: authority.Root, TrustedKeys: keys})
	if err != nil {
		return nil, config.Committed{}, nil, err
	}
	var store *config.Store
	var current config.Committed
	err = root.WithState(ctx, func(s release.State) error {
		if s.Active == nil || s.Active.Digest != authority.Digest || (s.Startup == nil) != (startup == nil) || s.Startup != nil && !s.Startup.Equal(*startup) {
			return fmt.Errorf("%w: worker boot selection differs from release journal", ErrConflict)
		}
		if s.ConfigStoreID != "" {
			existing, err := (&config.Store{Dir: dir}).Load()
			if err != nil {
				return err
			}
			if existing.StoreID != s.ConfigStoreID {
				return fmt.Errorf("%w: foreign configuration store", ErrConflict)
			}
		}
		store, current, err = config.OpenSelected(dir, seed, startup)
		if err != nil {
			return err
		}
		return selectedConfiguration(s, authority.Digest, current)
	})
	if err != nil {
		root.Close()
		return nil, config.Committed{}, nil, err
	}
	return store, current, root, nil
}

// NewWithRelease binds the writer to the exact manifest chosen by the loader.
// Its caller owns the Store lifetime. Standalone workers use New instead.
func NewWithRelease(store *config.Store, current config.Committed, n Network, root *release.Store, digest string) (*Engine, error) {
	if root == nil {
		return nil, fmt.Errorf("%w: release store is required", ErrConflict)
	}
	e := New(store, current, n)
	e.releaseStore, e.releaseDigest = root, digest
	if err := e.withRelease(context.Background(), false, func() error { return nil }); err != nil {
		return nil, err
	}
	return e, nil
}

func selectedConfiguration(s release.State, digest string, current config.Committed) error {
	if s.Active == nil || s.Active.Digest != digest {
		return fmt.Errorf("%w: executing runtime differs from release selection", ErrConflict)
	}
	if s.ConfigStoreID != "" && s.ConfigStoreID != current.StoreID {
		return fmt.Errorf("%w: release selection belongs to another configuration store", ErrConflict)
	}
	if s.Startup != nil && (current.Startup == nil || !current.Startup.Selection.Equal(*s.Startup)) {
		return fmt.Errorf("%w: configuration has not consumed the selected startup", ErrConflict)
	}
	return nil
}

// Caller holds e.mu (or is constructing the engine before publication). Keep
// this lock order: engine first, release second. Never call back into Engine
// from a release store operation.
func (e *Engine) withRelease(ctx context.Context, mutation bool, fn func() error) error {
	if e.releaseStore == nil {
		return fn()
	}
	return e.releaseStore.WithState(ctx, func(s release.State) error {
		if err := selectedConfiguration(s, e.releaseDigest, e.state.Committed); err != nil {
			return err
		}
		if mutation && s.Pending != nil && s.Pending.Startup != nil {
			return fmt.Errorf("%w: paired release preparation or trial freezes configuration commits", ErrConflict)
		}
		return fn()
	})
}
