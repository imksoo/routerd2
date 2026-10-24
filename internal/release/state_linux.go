// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var journalEntries = map[string]bool{"state.json": true, "active": true, "pending": true, "previous": true}

func stateLinks(state State) map[string]*Ref {
	links := map[string]*Ref{"active": state.Active, "previous": state.Previous, "pending": nil}
	if state.Pending != nil {
		links["pending"] = &state.Pending.Release
	}
	return links
}

func validBootID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return id != "." && id != ".."
}

func validToken(token string) bool {
	if len(token) != 32 {
		return false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validReason(reason string) bool {
	if len(reason) == 0 || len(reason) > 256 || strings.TrimSpace(reason) == "" || !utf8.ValidString(reason) {
		return false
	}
	for _, c := range reason {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

func validTime(t time.Time) bool {
	_, offset := t.Zone()
	return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 && offset == 0
}

func validateState(state State) error {
	if state.Version != 1 || !validTime(state.ChangedAt) {
		return errors.New("invalid release journal version or timestamp")
	}
	switch state.Operation {
	case "empty", "initialize", "prepare", "begin-boot", "confirm", "cancel", "rollback", "boot-failed", "boot-rollback":
	default:
		return errors.New("invalid release journal operation")
	}
	if state.Generation == 0 {
		if state.HighWatermark != 0 || state.Active != nil || state.Pending != nil || state.Previous != nil || state.Attempt != nil || state.LastFailure != nil || state.Operation != "empty" {
			return errors.New("nonempty initial release journal")
		}
		return nil
	}
	if state.HighWatermark == 0 || state.Operation == "empty" {
		return errors.New("missing release high watermark")
	}
	for _, ref := range stateLinks(state) {
		if ref != nil && (!validRef(*ref) || ref.Sequence > state.HighWatermark) {
			return errors.New("invalid release journal reference or high watermark")
		}
	}
	if state.Previous != nil && (state.Active == nil || state.Previous.Digest == state.Active.Digest) {
		return errors.New("invalid previous release")
	}
	if state.Pending != nil {
		if state.Pending.MaxAttempts < 1 || state.Pending.MaxAttempts > 5 {
			return errors.New("invalid boot attempt policy")
		}
		if state.Attempt == nil && state.Active != nil && state.Pending.Release.Digest == state.Active.Digest {
			return errors.New("active pending release has no boot attempt")
		}
	}
	if state.Attempt != nil {
		a := state.Attempt
		if state.Pending == nil || state.Active == nil || *state.Active != state.Pending.Release || a.Number < 1 || a.Number > state.Pending.MaxAttempts || !validBootID(a.BootID) || !validToken(a.Token) || !validTime(a.StartedAt) {
			return errors.New("invalid trial boot attempt")
		}
	}
	if state.LastFailure != nil {
		f := state.LastFailure
		if !validRef(f.Release) || f.Release.Sequence > state.HighWatermark || !validTime(f.At) || !validReason(f.Reason) || f.BootID != "" && !validBootID(f.BootID) {
			return errors.New("invalid failed release record")
		}
	}
	if state.Active == nil && state.Pending == nil && state.LastFailure == nil {
		return errors.New("unexplained release journal with no active or pending release")
	}
	return nil
}

func (s *Store) readState(ctx context.Context) (State, error) {
	target, err := readLink(s.root, "current")
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return State{}, os.ErrNotExist
		}
		return State{}, fmt.Errorf("%w: read selector: %v", ErrCorrupt, err)
	}
	if !strings.HasPrefix(target, "generations/") {
		return State{}, fmt.Errorf("%w: selector escapes generations", ErrCorrupt)
	}
	name := strings.TrimPrefix(target, "generations/")
	if !generationName.MatchString(name) {
		return State{}, fmt.Errorf("%w: invalid generation selector", ErrCorrupt)
	}
	dir, err := openDirectory(s.generations, name)
	if err != nil {
		return State{}, fmt.Errorf("%w: open generation: %v", ErrCorrupt, err)
	}
	defer dir.Close()
	raw, err := readFile(dir, "state.json", MaxStateBytes)
	if err != nil {
		return State{}, fmt.Errorf("%w: journal: %v", ErrCorrupt, err)
	}
	var state State
	if err = decodeCanonical(raw, &state); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if err = validateState(state); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if name[1:21] != fmt.Sprintf("%020d", state.Generation) {
		return State{}, fmt.Errorf("%w: journal generation differs from directory", ErrCorrupt)
	}
	digest := sha256.Sum256(raw)
	if name[22:86] != hex.EncodeToString(digest[:]) {
		return State{}, fmt.Errorf("%w: journal checksum differs from selector", ErrCorrupt)
	}
	names, err := directoryNames(dir, 4)
	if err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	for _, n := range names {
		if !journalEntries[n] {
			return State{}, fmt.Errorf("%w: unexpected journal entry", ErrCorrupt)
		}
	}
	verified := map[string]bool{}
	for link, ref := range stateLinks(state) {
		got, linkErr := readLink(dir, link)
		if ref == nil {
			if !errors.Is(linkErr, unix.ENOENT) {
				return State{}, fmt.Errorf("%w: unexpected %s link", ErrCorrupt, link)
			}
			continue
		}
		if linkErr != nil || got != "../../releases/"+ref.Digest {
			return State{}, fmt.Errorf("%w: %s link differs from journal", ErrCorrupt, link)
		}
		if !verified[ref.Digest] {
			image, err := s.openImage(ctx, *ref)
			if err != nil {
				return State{}, err
			}
			image.Close()
			verified[ref.Digest] = true
		}
	}
	return state, nil
}

func (s *Store) next(state State, operation string) (State, error) {
	if state.Generation == math.MaxUint64 {
		return state, errors.New("release generation exhausted")
	}
	state.Generation++
	state.ChangedAt = s.now().UTC()
	state.Operation = operation
	return state, nil
}

func (s *Store) commit(state State) error {
	if err := validateState(state); err != nil {
		return err
	}
	names, err := directoryNames(s.generations, s.options.MaxGenerations)
	if err != nil {
		return err
	}
	if len(names) >= s.options.MaxGenerations {
		return ErrCapacity
	}
	id, err := nonce()
	if err != nil {
		return err
	}
	temporary := ".gen-" + id
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > MaxStateBytes {
		return errors.New("release journal exceeds size bound")
	}
	digest := sha256.Sum256(raw)
	final := fmt.Sprintf("g%020d-%x-%s", state.Generation, digest, id)
	if err = unix.Mkdirat(int(s.generations.Fd()), temporary, 0700); err != nil {
		return err
	}
	dir, err := openDirectory(s.generations, temporary)
	if err != nil {
		return err
	}
	defer dir.Close()
	selected := false
	directoryName := temporary
	selectorName := ".current-" + id
	defer func() {
		_ = unix.Unlinkat(int(s.root.Fd()), selectorName, 0)
		if !selected {
			_ = removeFlatDirectory(s.generations, directoryName, journalEntries)
		}
	}()
	if err = writeFile(dir, "state.json", raw, 0400); err != nil {
		return err
	}
	if err = s.checkpoint("journal-state-synced"); err != nil {
		return err
	}
	for name, ref := range stateLinks(state) {
		if ref != nil {
			if err = unix.Symlinkat("../../releases/"+ref.Digest, int(dir.Fd()), name); err != nil {
				return err
			}
		}
	}
	if err = dir.Chmod(0500); err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		return err
	}
	if err = s.checkpoint("journal-generation-synced"); err != nil {
		return err
	}
	if err = unix.Renameat2(int(s.generations.Fd()), temporary, int(s.generations.Fd()), final, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	directoryName = final
	if err = s.checkpoint("journal-generation-published"); err != nil {
		return err
	}
	if err = s.generations.Sync(); err != nil {
		return err
	}
	if err = s.checkpoint("journal-parent-synced"); err != nil {
		return err
	}
	if err = unix.Symlinkat("generations/"+final, int(s.root.Fd()), selectorName); err != nil {
		return err
	}
	if err = s.checkpoint("journal-selector-created"); err != nil {
		return err
	}
	if err = unix.Renameat(int(s.root.Fd()), selectorName, int(s.root.Fd()), "current"); err != nil {
		return err
	}
	selected = true
	if err = s.checkpoint("journal-selector-replaced"); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if err = s.root.Sync(); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if err = s.checkpoint("journal-root-synced"); err != nil {
		return errors.Join(ErrUncertain, err)
	}
	return nil
}

// Initialize establishes the image installer's known-good baseline. It is
// allowed only in a truly empty journal and only from an already staged,
// currently verified signed bundle. The caller is declaring that baseline
// healthy; this method does not itself boot or test the release.
func (s *Store) Initialize(ctx context.Context, ref Ref) (State, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Generation != 0 {
		return State{}, errors.New("release store is already initialized or has accepted an update")
	}
	image, err := s.openImage(ctx, ref)
	if err != nil {
		return State{}, err
	}
	defer image.Close()
	state, err = s.next(state, "initialize")
	if err != nil {
		return State{}, err
	}
	state.Active = &ref
	state.HighWatermark = ref.Sequence
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// Prepare accepts a future boot candidate, but leaves Active unchanged.
// expectedGeneration is a required compare-and-swap fence, including zero.
func (s *Store) Prepare(ctx context.Context, ref Ref, expectedGeneration uint64, options PrepareOptions) (State, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Generation != expectedGeneration {
		return State{}, ErrStale
	}
	if state.Pending != nil || state.Attempt != nil {
		return State{}, ErrBusy
	}
	if ref.Sequence <= state.HighWatermark {
		return State{}, ErrDowngrade
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 1
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 5 {
		return State{}, errors.New("max attempts must be 1..5")
	}
	// A queued update must have room to complete its ordinary boot lifecycle.
	// In particular, do not consume the last journal slot with Prepare and
	// leave PID 1 unable to record either confirmation or bounded fallback.
	// Prune can reclaim old generations before the operator retries Prepare.
	generations, err := directoryNames(s.generations, s.options.MaxGenerations)
	if err != nil {
		return State{}, err
	}
	if s.options.MaxGenerations-len(generations) < options.MaxAttempts+2 {
		return State{}, fmt.Errorf("prepare needs room for %d boot attempts and confirmation/fallback: %w", options.MaxAttempts, ErrCapacity)
	}
	image, err := s.openImage(ctx, ref)
	if err != nil {
		return State{}, err
	}
	defer image.Close()
	state, err = s.next(state, "prepare")
	if err != nil {
		return State{}, err
	}
	state.Pending = &Pending{Release: ref, MaxAttempts: options.MaxAttempts}
	state.HighWatermark = ref.Sequence
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// BeginBoot must receive the current kernel boot ID (or an equivalent launcher
// boot identity), not a new random value for every invocation. Duplicate calls
// in the same boot return the same trial and token without spending an attempt.
// A returned Image pins the verified pair; the caller must close it.
func (s *Store) BeginBoot(ctx context.Context, bootID string) (Selection, error) {
	if !validBootID(bootID) {
		return Selection{}, errors.New("invalid boot ID")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return Selection{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return Selection{}, err
	}
	changed, rolledBack, failed := false, false, false
	if state.Pending != nil {
		if state.Attempt == nil || state.Attempt.BootID != bootID {
			if state.Attempt != nil && state.Attempt.Number >= state.Pending.MaxAttempts {
				state, err = s.failBootState(state, "unconfirmed boot attempt limit reached")
				if err != nil {
					return Selection{}, err
				}
				changed, rolledBack, failed = true, state.Active != nil, true
			} else {
				token, err := nonce()
				if err != nil {
					return Selection{}, err
				}
				number := 1
				if state.Attempt != nil {
					number = state.Attempt.Number + 1
				} else {
					state.Previous = state.Active
				}
				active := state.Pending.Release
				state.Active = &active
				state.Attempt = &Attempt{BootID: bootID, Token: token, Number: number, StartedAt: s.now().UTC()}
				state, err = s.next(state, "begin-boot")
				if err != nil {
					return Selection{}, err
				}
				changed = true
			}
		}
	}
	var image *Image
	if state.Active != nil {
		image, err = s.openImage(ctx, *state.Active)
		if err != nil {
			return Selection{}, err
		}
	}
	if changed {
		if err = s.commit(state); err != nil {
			image.Close()
			return Selection{}, err
		}
	}
	if image == nil {
		if failed {
			return Selection{State: state}, ErrNoFallback
		}
		return Selection{State: state}, ErrNoActive
	}
	return Selection{State: state, Image: image, Trial: state.Attempt != nil, RolledBack: rolledBack}, nil
}

// Confirm records caller-observed health of exactly this active trial. There
// is no "confirm latest" shortcut: stale boot IDs, tokens and digests fail.
func (s *Store) Confirm(ctx context.Context, bootID, token, digest string) (State, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Attempt == nil || state.Pending == nil || state.Active == nil || state.Attempt.BootID != bootID || state.Attempt.Token != token || state.Active.Digest != digest {
		return State{}, ErrStale
	}
	state, err = s.next(state, "confirm")
	if err != nil {
		return State{}, err
	}
	state.Attempt = nil
	state.Pending = nil
	// Verification reads both selected executable pairs and can take longer
	// than a health deadline on slow storage. Do not start a new durable
	// confirmation after that caller's decision window has expired. Once the
	// commit begins, its fsync/rename transaction cannot be rolled back by
	// context cancellation.
	if err = s.checkContext(ctx); err != nil {
		return State{}, err
	}
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (s *Store) CancelPending(ctx context.Context, expectedGeneration uint64) (State, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Generation != expectedGeneration {
		return State{}, ErrStale
	}
	if state.Attempt != nil {
		return State{}, ErrBusy
	}
	if state.Pending == nil {
		return State{}, errors.New("no pending release")
	}
	canceled := state.Pending.Release
	state, err = s.next(state, "cancel")
	if err != nil {
		return State{}, err
	}
	state.Pending = nil
	// An empty-store candidate has no baseline. Retain an explicit failure
	// record instead of manufacturing a healthy active release.
	if state.Active == nil {
		state.LastFailure = &Failure{Release: canceled, Reason: "pending initial release canceled", At: s.now().UTC()}
	}
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// Rollback explicitly selects the previously confirmed release. A queued but
// untried candidate must be canceled separately. Trial failures do not become
// the new Previous, so another rollback cannot accidentally retry a bad image.
func (s *Store) Rollback(ctx context.Context, expectedGeneration uint64, reason string) (State, error) {
	if !validReason(reason) {
		return State{}, errors.New("rollback requires a 1..256 byte printable reason")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Generation != expectedGeneration {
		return State{}, ErrStale
	}
	if state.Pending != nil && state.Attempt == nil {
		return State{}, ErrBusy
	}
	if state.Active == nil || state.Previous == nil {
		return State{}, ErrNoFallback
	}
	if state.Attempt != nil {
		state, err = s.failBootState(state, reason)
	} else {
		old := state.Active
		state.Active = state.Previous
		state.Previous = old
		state, err = s.next(state, "rollback")
	}
	if err != nil {
		return State{}, err
	}
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	return state, nil
}

// FailBoot is an explicit health/watchdog failure for the exact trial token.
// No timer silently invents health. A launcher may call it on its own readiness
// deadline; the library changes only the durable selection.
func (s *Store) FailBoot(ctx context.Context, bootID, token, reason string) (State, error) {
	if !validReason(reason) {
		return State{}, errors.New("boot failure requires a 1..256 byte printable reason")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return State{}, err
	}
	if state.Attempt == nil || state.Attempt.BootID != bootID || state.Attempt.Token != token {
		return State{}, ErrStale
	}
	state, err = s.failBootState(state, reason)
	if err != nil {
		return State{}, err
	}
	if err = s.commit(state); err != nil {
		return State{}, err
	}
	if state.Active == nil {
		return state, ErrNoFallback
	}
	return state, nil
}

func (s *Store) failBootState(state State, reason string) (State, error) {
	if state.Active == nil || state.Attempt == nil || state.Pending == nil {
		return state, ErrStale
	}
	state.LastFailure = &Failure{Release: *state.Active, BootID: state.Attempt.BootID, Reason: reason, At: s.now().UTC()}
	state.Active = state.Previous
	state.Previous = nil
	state.Pending = nil
	state.Attempt = nil
	operation := "boot-failed"
	if state.Active != nil {
		operation = "boot-rollback"
	}
	return s.next(state, operation)
}

// Prune removes only old journal generations and explicitly named, unselected
// releases. The current Active/Pending/Previous set is protected. It also
// removes incomplete private transaction directories left by a crashed writer.
// expectedGeneration fences an operator's reviewed cleanup list.
func (s *Store) Prune(ctx context.Context, expectedGeneration uint64, releaseDigests []string) (CleanupResult, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return CleanupResult{}, err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return CleanupResult{}, err
	}
	if state.Generation != expectedGeneration {
		return CleanupResult{}, ErrStale
	}
	if len(releaseDigests) > s.options.MaxStoredReleases {
		return CleanupResult{}, errors.New("cleanup digest list exceeds store capacity")
	}
	protected := map[string]bool{}
	for _, ref := range stateLinks(state) {
		if ref != nil {
			protected[ref.Digest] = true
		}
	}
	seen := map[string]bool{}
	for _, digest := range releaseDigests {
		if !validDigest(digest) || seen[digest] {
			return CleanupResult{}, errors.New("invalid or duplicate cleanup digest")
		}
		if protected[digest] {
			return CleanupResult{}, errors.New("cannot remove an active, pending, or previous release")
		}
		seen[digest] = true
	}
	current, err := readLink(s.root, "current")
	if err != nil {
		return CleanupResult{}, err
	}
	generationNames, err := directoryNames(s.generations, s.options.MaxGenerations)
	if err != nil {
		return CleanupResult{}, err
	}
	releaseNames, err := directoryNames(s.releases, s.options.MaxStoredReleases)
	if err != nil {
		return CleanupResult{}, err
	}
	rootNames, err := directoryNames(s.root, s.options.MaxGenerations+16)
	if err != nil {
		return CleanupResult{}, err
	}
	var result CleanupResult
	for _, name := range generationNames {
		if "generations/"+name == current {
			continue
		}
		if !generationName.MatchString(name) && !(strings.HasPrefix(name, ".gen-") && validToken(strings.TrimPrefix(name, ".gen-"))) {
			return result, fmt.Errorf("%w: unknown generation directory", ErrCorrupt)
		}
		if generationName.MatchString(name) {
			number, parseErr := strconv.ParseUint(name[1:21], 10, 64)
			if parseErr != nil || number > state.Generation && number-state.Generation > 1 {
				return result, fmt.Errorf("%w: invalid orphan generation", ErrCorrupt)
			}
		}
		if err = s.checkContext(ctx); err != nil {
			return result, err
		}
		if err = removeFlatDirectory(s.generations, name, journalEntries); err != nil {
			return result, err
		}
		result.RemovedGenerations = append(result.RemovedGenerations, name)
	}
	for _, name := range releaseNames {
		temporary := strings.HasPrefix(name, ".stage-") && validToken(strings.TrimPrefix(name, ".stage-"))
		if !temporary && !seen[name] {
			continue
		}
		if err = s.checkContext(ctx); err != nil {
			return result, err
		}
		if err = removeFlatDirectory(s.releases, name, releaseEntries); err != nil {
			return result, err
		}
		result.RemovedReleases = append(result.RemovedReleases, name)
	}
	for _, name := range rootNames {
		if !strings.HasPrefix(name, ".current-") || !validToken(strings.TrimPrefix(name, ".current-")) {
			continue
		}
		target, err := readLink(s.root, name)
		if err != nil || !strings.HasPrefix(target, "generations/") || !generationName.MatchString(strings.TrimPrefix(target, "generations/")) {
			return result, fmt.Errorf("%w: invalid abandoned selector", ErrCorrupt)
		}
		if err = unix.Unlinkat(int(s.root.Fd()), name, 0); err != nil {
			return result, err
		}
	}
	if err = s.root.Sync(); err != nil {
		return result, err
	}
	return result, nil
}
