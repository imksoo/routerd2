// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Store struct {
	options                           Options
	root, releases, generations, lock *os.File
	mu                                sync.Mutex
	closed                            bool
	nativeChecks                      map[nativeCheck]struct{} // per Store, bounded; never substitutes for hashing
	now                               func() time.Time
	fault                             func(string) error // deterministic crash/I/O boundary tests, never configuration
}

// Open anchors all operations to directory descriptors and creates the empty
// journal if this is a genuinely new store. An absent or corrupt selector in an
// existing store is never silently treated as a new installation.
func Open(options Options) (*Store, error) {
	if options.TargetOS == "" {
		options.TargetOS = runtime.GOOS
	}
	if options.TargetArch == "" {
		options.TargetArch = runtime.GOARCH
	}
	if options.TargetOS != "linux" || options.TargetArch != "amd64" && options.TargetArch != "arm64" {
		return nil, errors.New("unsupported release target")
	}
	if options.MaxArtifactBytes == 0 {
		options.MaxArtifactBytes = DefaultMaxArtifactBytes
	}
	if options.MaxArtifactBytes < 64 || options.MaxArtifactBytes > MaxArtifactBytes {
		return nil, errors.New("invalid maximum artifact size")
	}
	if options.MaxStoredReleases == 0 {
		options.MaxStoredReleases = 32
	}
	if options.MaxGenerations == 0 {
		options.MaxGenerations = 256
	}
	if options.MaxStoredReleases < 2 || options.MaxStoredReleases > 1024 || options.MaxGenerations < 4 || options.MaxGenerations > 4096 {
		return nil, errors.New("invalid release store capacity")
	}
	keys := map[string]ed25519.PublicKey{}
	for id, key := range options.TrustedKeys {
		if len(key) != ed25519.PublicKeySize || KeyID(key) != id {
			return nil, errors.New("invalid trusted Ed25519 key ID or length")
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	if len(keys) == 0 || len(keys) > 64 {
		return nil, errors.New("release store requires 1..64 locally trusted keys")
	}
	options.TrustedKeys = keys
	s := &Store{options: options, now: time.Now}
	root, err := openRoot(options.Root)
	if err != nil {
		return nil, err
	}
	s.root = root
	fail := func(err error) (*Store, error) { s.closeFiles(); return nil, err }
	if s.releases, err = makeDirectory(root, "releases"); err != nil {
		return fail(err)
	}
	if s.generations, err = makeDirectory(root, "generations"); err != nil {
		return fail(err)
	}
	if s.lock, err = openRegular(root, ".lock", unix.O_RDWR|unix.O_CREAT, 0600, 0); err != nil {
		return fail(err)
	}
	if err = root.Sync(); err != nil {
		return fail(err)
	}
	unlock, err := s.acquire(context.Background())
	if err != nil {
		return fail(err)
	}
	_, err = s.readState(context.Background())
	if errors.Is(err, os.ErrNotExist) {
		names, listErr := directoryNames(s.generations, options.MaxGenerations)
		if listErr != nil {
			err = listErr
		} else if len(names) != 0 {
			err = fmt.Errorf("%w: missing current selector in nonempty journal", ErrCorrupt)
		} else {
			state := State{Version: StateVersion, Generation: 0, ChangedAt: s.now().UTC(), Operation: "empty"}
			err = s.commit(state)
		}
	}
	unlock()
	if err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *Store) closeFiles() {
	for _, f := range []*os.File{s.lock, s.generations, s.releases, s.root} {
		if f != nil {
			_ = f.Close()
		}
	}
}

func (s *Store) Close() error {
	if !s.mu.TryLock() {
		return ErrBusy
	}
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.closeFiles()
	return nil
}

func (s *Store) acquire(ctx context.Context) (func(), error) {
	if err := s.checkContext(ctx); err != nil {
		return nil, err
	}
	if !s.mu.TryLock() {
		return nil, ErrBusy
	}
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	if err := unix.Flock(int(s.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		s.mu.Unlock()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { _ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN); s.mu.Unlock() }, nil
}

func (s *Store) Status(ctx context.Context) (State, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return State{}, err
	}
	defer unlock()
	return s.readState(ctx)
}

// WithState keeps both the process mutex and the interprocess file lock until
// fn returns. It lets the configuration writer validate the selected runtime
// and finish a configuration transaction without racing prepare/boot/confirm.
// The callback must not call another method on this Store. Competing operations
// return ErrBusy rather than waiting. The supplied state is not persisted.
func (s *Store) WithState(ctx context.Context, fn func(State) error) error {
	if fn == nil {
		return errors.New("nil release state callback")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := s.readState(ctx)
	if err != nil {
		return err
	}
	return fn(state)
}

func (s *Store) bundleLimit() int64 {
	return min(int64(MaxArtifacts)*s.options.MaxArtifactBytes, MaxRuntimeBytes) + MaxManifestBytes + MaxTrailerBytes + int64(MaxArtifacts+4)*1024
}

// StageFile accepts only a bounded regular local file and never follows a final
// symlink. Stage also supports a caller-owned Reader; its caller is responsible
// for interrupting a blocked Reader when the context is canceled.
func (s *Store) StageFile(ctx context.Context, path string) (Ref, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return Ref{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return Ref{}, err
	}
	if !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > s.bundleLimit() {
		return Ref{}, errors.New("release archive must be a bounded regular local file")
	}
	return s.Stage(ctx, f)
}

func (s *Store) Stage(ctx context.Context, input io.Reader) (Ref, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return Ref{}, err
	}
	defer unlock()
	if input == nil {
		return Ref{}, errors.New("nil release bundle reader")
	}
	if _, err := s.readState(ctx); err != nil {
		return Ref{}, err
	}
	b := bundleReader{ctx: ctx, r: io.LimitReader(input, s.bundleLimit()+1)}
	raw, err := b.metadata("manifest.json", MaxManifestBytes)
	if err != nil {
		return Ref{}, err
	}
	signature, err := b.metadata("manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return Ref{}, err
	}
	m, ref, err := VerifyManifest(raw, signature, s.options.TrustedKeys, s.options.TargetOS, s.options.TargetArch, s.options.MaxArtifactBytes)
	if err != nil {
		return Ref{}, err
	}
	if err = s.checkpoint("stage-signature-verified"); err != nil {
		return Ref{}, err
	}
	names, err := directoryNames(s.releases, s.options.MaxStoredReleases)
	if err != nil {
		return Ref{}, err
	}
	exists := false
	for _, name := range names {
		exists = exists || name == ref.Digest
	}
	if exists {
		image, err := s.openImage(ctx, ref)
		if err != nil {
			return Ref{}, err
		}
		image.Close()
		// Validate the complete duplicate input without creating an extra
		// temporary version at capacity. Its signed digests identify the
		// existing ELF bytes already inspected through pinned descriptors.
		for _, artifact := range m.Artifacts {
			size, err := b.header(artifact.Name, artifact.Size)
			if err != nil {
				return Ref{}, err
			}
			if size != artifact.Size {
				return Ref{}, errors.New("tar size differs from signed artifact")
			}
			hash := sha256.New()
			n, err := io.Copy(hash, contextReader{ctx, io.LimitReader(b.r, size)})
			if err != nil {
				return Ref{}, err
			}
			if n != size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
				return Ref{}, errors.New("duplicate artifact digest differs from signed manifest")
			}
			if err = b.padding(size); err != nil {
				return Ref{}, err
			}
		}
		if err = b.finish(); err != nil {
			return Ref{}, err
		}
		return ref, nil
	}
	if len(names) >= s.options.MaxStoredReleases {
		return Ref{}, ErrCapacity
	}
	n, err := nonce()
	if err != nil {
		return Ref{}, err
	}
	tempName := ".stage-" + n
	if err = unix.Mkdirat(int(s.releases.Fd()), tempName, 0700); err != nil {
		return Ref{}, err
	}
	dir, err := openDirectory(s.releases, tempName)
	if err != nil {
		return Ref{}, err
	}
	defer dir.Close()
	published := false
	defer func() {
		if !published {
			_ = removeFlatDirectory(s.releases, tempName, runtimeEntries(m))
		}
	}()
	if err = writeFile(dir, "manifest.json", raw, 0400); err != nil {
		return Ref{}, err
	}
	if err = writeFile(dir, "manifest.sig", signature, 0400); err != nil {
		return Ref{}, err
	}
	for _, a := range m.Artifacts {
		size, err := b.header(a.Name, a.Size)
		if err != nil {
			return Ref{}, err
		}
		if size != a.Size {
			return Ref{}, errors.New("tar size differs from the signed artifact size")
		}
		if err = s.stageArtifact(ctx, dir, a, b); err != nil {
			return Ref{}, err
		}
		if err = s.checkpoint("stage-" + a.Name + "-synced"); err != nil {
			return Ref{}, err
		}
	}
	if err = b.finish(); err != nil {
		return Ref{}, err
	}
	if err = dir.Chmod(0500); err != nil {
		return Ref{}, err
	}
	if err = dir.Sync(); err != nil {
		return Ref{}, err
	}
	if err = s.checkpoint("stage-directory-synced"); err != nil {
		return Ref{}, err
	}
	// Never replace an existing content-addressed version, including a corrupt
	// one. Idempotent re-staging verifies the existing pinned payload first.
	err = unix.Renameat2(int(s.releases.Fd()), tempName, int(s.releases.Fd()), ref.Digest, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		image, verifyErr := s.openImage(ctx, ref)
		if verifyErr != nil {
			return Ref{}, verifyErr
		}
		image.Close()
		return ref, nil
	}
	if err != nil {
		return Ref{}, err
	}
	published = true
	if err = s.checkpoint("stage-directory-published"); err != nil {
		return Ref{}, errors.Join(ErrUncertain, err)
	}
	if err = s.releases.Sync(); err != nil {
		return Ref{}, errors.Join(ErrUncertain, err)
	}
	return ref, nil
}

func runtimeEntries(m Manifest) map[string]bool {
	entries := map[string]bool{"manifest.json": true, "manifest.sig": true}
	for _, a := range m.Artifacts {
		entries[a.Name] = true
	}
	return entries
}

// An abandoned stage may contain only partially written metadata. Artifact
// names come from a validated manifest, never from directory enumeration.
func (s *Store) cleanupRuntimeEntries(name string) (map[string]bool, error) {
	dir, err := openDirectory(s.releases, name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	raw, err := readFile(dir, "manifest.json", MaxManifestBytes)
	if errors.Is(err, unix.ENOENT) {
		return runtimeEntries(Manifest{}), nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err = decodeCanonical(raw, &m); err != nil {
		return nil, err
	}
	if err = ValidateManifest(m, s.options.MaxArtifactBytes); err != nil {
		return nil, err
	}
	return runtimeEntries(m), nil
}

func (s *Store) stageArtifact(ctx context.Context, dir *os.File, a Artifact, b bundleReader) error {
	f, err := openRegular(dir, a.Name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600, a.Size)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(f, hash), contextReader{ctx, io.LimitReader(b.r, a.Size)})
	if err != nil {
		return err
	}
	if written != a.Size || hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		return errors.New("artifact size or SHA256 differs from its signed manifest")
	}
	if err = b.padding(a.Size); err != nil {
		return err
	}
	// Identity and static ELF parsing happen only after the signature and full
	// payload digest have been checked.
	raw, err := readFile(dir, "manifest.json", MaxManifestBytes)
	if err != nil {
		return err
	}
	var m Manifest
	if err = decodeCanonical(raw, &m); err != nil {
		return err
	}
	if a.Kind == "native" {
		sum := sha256.Sum256(raw)
		if err := s.verifyNative(f, a, m, hex.EncodeToString(sum[:])); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	mode := os.FileMode(0400)
	if a.Kind == "native" {
		mode = 0500
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

func (s *Store) openImage(ctx context.Context, ref Ref) (*Image, error) {
	if !validRef(ref) {
		return nil, fmt.Errorf("%w: invalid release reference", ErrCorrupt)
	}
	dir, err := openDirectory(s.releases, ref.Digest)
	if err != nil {
		return nil, fmt.Errorf("%w: open release: %v", ErrCorrupt, err)
	}
	defer dir.Close()
	m, err := s.readImageManifest(dir, ref)
	if err != nil {
		return nil, err
	}
	names, err := directoryNames(dir, MaxArtifacts+2)
	if err != nil {
		return nil, err
	}
	allowed := runtimeEntries(m)
	if len(names) != len(allowed) {
		return nil, fmt.Errorf("%w: incomplete runtime directory", ErrCorrupt)
	}
	for _, name := range names {
		if !allowed[name] {
			return nil, fmt.Errorf("%w: unexpected runtime entry", ErrCorrupt)
		}
	}
	image := &Image{Release: ref, Manifest: m, Artifacts: map[string]*os.File{}}
	fail := func(err error) (*Image, error) { image.Close(); return nil, err }
	for _, a := range m.Artifacts {
		f, err := openRegular(dir, a.Name, unix.O_RDONLY, 0, a.Size)
		if err != nil {
			return fail(fmt.Errorf("%w: %v", ErrCorrupt, err))
		}
		image.Artifacts[a.Name] = f
		stat, err := f.Stat()
		if err != nil {
			return fail(err)
		}
		mode := os.FileMode(0400)
		if a.Kind == "native" {
			mode = 0500
		}
		if stat.Size() != a.Size || stat.Mode().Perm() != mode {
			return fail(fmt.Errorf("%w: artifact size or mode changed", ErrCorrupt))
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, contextReader{ctx, io.NewSectionReader(f, 0, a.Size)}); err != nil {
			return fail(err)
		}
		if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
			return fail(fmt.Errorf("%w: artifact digest changed", ErrCorrupt))
		}
		if a.Kind == "native" {
			if err := s.verifyNative(f, a, m, ref.Digest); err != nil {
				return fail(fmt.Errorf("%w: %v", ErrCorrupt, err))
			}
		}
	}
	return image, nil
}

// readImageManifest is also used while holding the store lock after readState
// has verified all selected payloads. It does not stand in for payload checks.
func (s *Store) readImageManifest(dir *os.File, ref Ref) (Manifest, error) {
	raw, err := readFile(dir, "manifest.json", MaxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := readFile(dir, "manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return Manifest{}, err
	}
	m, actual, err := VerifyManifest(raw, sig, s.options.TrustedKeys, s.options.TargetOS, s.options.TargetArch, s.options.MaxArtifactBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if actual != ref {
		return Manifest{}, fmt.Errorf("%w: release reference differs from its signed manifest", ErrCorrupt)
	}
	return m, nil
}

// Verify returns the complete pinned, re-verified runtime artifact set for a staged reference. It
// does not select that release. The caller owns the returned descriptors.
func (s *Store) Verify(ctx context.Context, ref Ref) (*Image, error) {
	unlock, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err = s.readState(ctx); err != nil {
		return nil, err
	}
	return s.openImage(ctx, ref)
}

// Lookup resolves a content digest to its signed reference and re-verifies the
// complete installed runtime. It is the CLI/API boundary for --digest arguments;
// callers must never invent ReleaseID, Sequence, or architecture from a path.
func (s *Store) Lookup(ctx context.Context, digest string) (Ref, error) {
	if !validDigest(digest) {
		return Ref{}, errors.New("invalid release digest")
	}
	unlock, err := s.acquire(ctx)
	if err != nil {
		return Ref{}, err
	}
	defer unlock()
	if _, err = s.readState(ctx); err != nil {
		return Ref{}, err
	}
	dir, err := openDirectory(s.releases, digest)
	if err != nil {
		return Ref{}, err
	}
	defer dir.Close()
	raw, err := readFile(dir, "manifest.json", MaxManifestBytes)
	if err != nil {
		return Ref{}, err
	}
	sig, err := readFile(dir, "manifest.sig", ed25519.SignatureSize)
	if err != nil {
		return Ref{}, err
	}
	_, ref, err := VerifyManifest(raw, sig, s.options.TrustedKeys, s.options.TargetOS, s.options.TargetArch, s.options.MaxArtifactBytes)
	if err != nil {
		return Ref{}, err
	}
	if ref.Digest != digest {
		return Ref{}, fmt.Errorf("%w: manifest differs from directory digest", ErrCorrupt)
	}
	image, err := s.openImage(ctx, ref)
	if err != nil {
		return Ref{}, err
	}
	image.Close()
	return ref, nil
}
