// SPDX-License-Identifier: BSD-3-Clause

// Package release verifies and stages signed pairs of routerd executables and
// durably selects their next boot. It never executes a payload, downloads a
// release, changes the running process tree, or reboots the machine.
package release

import (
	"crypto/ed25519"
	"errors"
	"os"
	"time"
)

const (
	ManifestVersion               = 1
	MaxManifestBytes              = 16 << 10
	MaxStateBytes                 = 32 << 10
	DefaultMaxArtifactBytes int64 = 512 << 20
	MaxArtifactBytes        int64 = 1 << 30
	MaxTrailerBytes               = 64 << 10
)

var (
	ErrBusy       = errors.New("release store is busy")
	ErrClosed     = errors.New("release store is closed")
	ErrStale      = errors.New("release generation or boot attempt is stale")
	ErrDowngrade  = errors.New("release sequence does not exceed the durable high watermark")
	ErrNoActive   = errors.New("no active release")
	ErrNoFallback = errors.New("failed boot has no previously confirmed release")
	ErrCorrupt    = errors.New("release store is corrupt")
	ErrUncertain  = errors.New("release selector changed but directory durability was not confirmed; read status before retrying")
	ErrCapacity   = errors.New("release store capacity reached; remove unused releases or old generations explicitly")
)

type Artifact struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Sequence is a publisher-assigned, strictly increasing release counter. A
// release ID is an opaque human-readable identity, never a version comparison.
type Manifest struct {
	Version   int        `json:"version"`
	ReleaseID string     `json:"release_id"`
	Sequence  uint64     `json:"sequence"`
	GOOS      string     `json:"goos"`
	GOARCH    string     `json:"goarch"`
	KeyID     string     `json:"key_id"`
	Artifacts []Artifact `json:"artifacts"`
}

type Ref struct {
	Digest    string `json:"digest"`
	ReleaseID string `json:"release_id"`
	Sequence  uint64 `json:"sequence"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

type Pending struct {
	Release     Ref `json:"release"`
	MaxAttempts int `json:"max_attempts"`
}

type Attempt struct {
	BootID    string    `json:"boot_id"`
	Token     string    `json:"token"`
	Number    int       `json:"number"`
	StartedAt time.Time `json:"started_at"`
}

type Failure struct {
	Release Ref       `json:"release"`
	BootID  string    `json:"boot_id,omitempty"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// State and its active/pending/previous symlinks are published together by one
// atomic current-symlink replacement. HighWatermark survives cancel/rollback.
type State struct {
	Version       int       `json:"version"`
	Generation    uint64    `json:"generation"`
	HighWatermark uint64    `json:"high_watermark"`
	Active        *Ref      `json:"active,omitempty"`
	Pending       *Pending  `json:"pending,omitempty"`
	Previous      *Ref      `json:"previous,omitempty"`
	Attempt       *Attempt  `json:"attempt,omitempty"`
	LastFailure   *Failure  `json:"last_failure,omitempty"`
	ChangedAt     time.Time `json:"changed_at"`
	Operation     string    `json:"operation"`
}

type Options struct {
	// Root must be an absolute, private directory, with no symlink components.
	Root string
	// Key IDs are full lower-case SHA256 hashes of the Ed25519 public keys.
	// This is local trust policy; a bundle cannot install or rotate trust keys.
	TrustedKeys map[string]ed25519.PublicKey
	// Empty targets default to the running Linux/architecture pair.
	TargetOS         string
	TargetArch       string
	MaxArtifactBytes int64
	// The store never silently evicts a release. Capacity failures are explicit.
	MaxStoredReleases int
	MaxGenerations    int
}

type PrepareOptions struct {
	// A retry requires a different kernel boot ID. Defaults to one attempt.
	MaxAttempts int
}

// Image pins both verified file descriptions. A launcher should use these
// descriptors, not independently resolve two paths through a moving selector.
// Close the descriptors after transferring ownership to the launcher.
type Image struct {
	Release Ref
	Routerd *os.File
	Modules *os.File
}

func (i *Image) Close() error {
	if i == nil {
		return nil
	}
	var a, b error
	if i.Routerd != nil {
		a = i.Routerd.Close()
		i.Routerd = nil
	}
	if i.Modules != nil {
		b = i.Modules.Close()
		i.Modules = nil
	}
	return errors.Join(a, b)
}

type Selection struct {
	State      State
	Image      *Image
	Trial      bool
	RolledBack bool
}

type CleanupResult struct {
	RemovedReleases    []string `json:"removed_releases"`
	RemovedGenerations []string `json:"removed_generations"`
}
