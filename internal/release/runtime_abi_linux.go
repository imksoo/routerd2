// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"fmt"
	"os"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

type nativeCheck struct{ Manifest, Artifact string }

// Caller holds s.mu and has just checked the complete current payload digest,
// regular-file mode/size and signed manifest. That manifest digest binds all
// inspection expectations, not just an artifact's bytes. No result crosses a
// Store lifetime; every runtime image acquisition still hashes every artifact.
func (s *Store) verifyNative(file *os.File, artifact Artifact, manifest Manifest, digest string) error {
	key := nativeCheck{Manifest: digest, Artifact: artifact.Name}
	if _, ok := s.nativeChecks[key]; ok {
		return nil
	}
	if err := s.checkpoint("native-inspection"); err != nil {
		return err
	}
	declared, err := InspectELFABI(file, artifact.Size, artifact.Command, manifest.ReleaseID, manifest.GOOS, manifest.GOARCH)
	if err != nil {
		return err
	}
	if !declared.Equal(manifest.RuntimeABI) {
		return fmt.Errorf("native ABI declaration differs from signed manifest")
	}
	if s.nativeChecks == nil || len(s.nativeChecks) >= MaxArtifacts*4 {
		s.nativeChecks = make(map[nativeCheck]struct{})
	}
	s.nativeChecks[key] = struct{}{}
	return nil
}

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
