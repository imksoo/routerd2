// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"fmt"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

// Called only under the store lock after readState has verified the selected
// payloads. Re-reading their signed metadata avoids hashing the same artifact
// set again merely to compare declarations; digest/ref checks remain mandatory.
func (s *Store) selectedABI(ctx context.Context, ref Ref) (runtimeabi.Descriptor, error) {
	if err := s.checkContext(ctx); err != nil {
		return runtimeabi.Descriptor{}, err
	}
	dir, err := openDirectory(s.releases, ref.Digest)
	if err != nil {
		return runtimeabi.Descriptor{}, err
	}
	defer dir.Close()
	m, err := s.readImageManifest(dir, ref)
	if err != nil {
		return runtimeabi.Descriptor{}, err
	}
	return m.RuntimeABI, nil
}

func (s *Store) requireSameABI(ctx context.Context, active Ref, target runtimeabi.Descriptor) error {
	current, err := s.selectedABI(ctx, active)
	if err != nil {
		return err
	}
	if !current.Equal(target) {
		return fmt.Errorf("%w: changing format declarations requires qualified configuration/state migration, not callback success alone", runtimeabi.ErrIncompatible)
	}
	return nil
}

func (s *Store) requireSameSelectedABI(ctx context.Context, active, previous Ref) error {
	target, err := s.selectedABI(ctx, previous)
	if err != nil {
		return err
	}
	return s.requireSameABI(ctx, active, target)
}
