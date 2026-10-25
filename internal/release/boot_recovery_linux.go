// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"errors"
)

// OpenForBoot opens an existing journal and completes its boot selection before
// returning operational store access. Unlike Open, it can reject a damaged
// pending payload and durably select a fully verified fallback. It never
// initializes a missing selector or returns handles to a rejected executable.
// The caller owns both returned Store and Selection.Image on success.
func OpenForBoot(ctx context.Context, options Options, bootID string) (*Store, Selection, error) {
	if !validBootID(bootID) {
		return nil, Selection{}, errors.New("invalid boot ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, Selection{}, err
	}
	s, err := openStoreHandles(options)
	if err != nil {
		return nil, Selection{}, err
	}
	selected, err := s.BeginBoot(ctx, bootID)
	if err != nil {
		s.Close()
		return nil, selected, err
	}
	return s, selected, nil
}

// The caller holds both store locks and has authenticated the complete journal.
// Every noncandidate reference is fully verified before candidate failure can
// change selection. An untried candidate has never been authorized to execute;
// its failure preserves the current baseline/startup. A trial may have written
// state, so its signed ABI must still authenticate and equal the fallback ABI.
// Lost/untrusted trial metadata is a refusal, not permission to guess formats.
func (s *Store) recoverBootCandidate(ctx context.Context, state State, bootID string) (State, bool, error) {
	if state.Pending == nil {
		return state, false, s.verifyReferencedImages(ctx, state, "")
	}
	if err := s.verifyReferencedImages(ctx, state, state.Pending.Release.Digest); err != nil {
		return state, false, err
	}
	image, err := s.openImage(ctx, state.Pending.Release)
	if err == nil {
		image.Close()
		return state, false, nil
	}
	if err := s.checkContext(ctx); err != nil {
		return state, false, err
	}
	if state.Attempt != nil {
		next, err := s.failBootState(ctx, state, "trial runtime payload failed verification before execution")
		return next, err == nil, err
	}
	state.LastFailure = &Failure{Release: state.Pending.Release, Startup: cloneStartup(state.Pending.Startup), BootID: bootID, Reason: "prepared runtime failed verification before execution", At: s.now().UTC()}
	// PrepareRollback can point Pending at Previous. The rejected payload must
	// not remain an operationally selected reference after this transaction.
	if state.Previous != nil && state.Previous.Digest == state.Pending.Release.Digest {
		state.Previous = nil
		state.PreviousStartup = nil
	}
	state.Pending = nil
	operation := "boot-failed"
	if state.Active != nil {
		operation = "boot-rollback"
	}
	state, err = s.next(state, operation)
	return state, err == nil, err
}
