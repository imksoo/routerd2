// SPDX-License-Identifier: BSD-3-Clause

// Package release verifies and stages signed extracted runtime artifact sets and
// durably selects their next boot. It never executes a payload, downloads a
// release, changes the running process tree, or reboots the machine.
package release

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/runtimeabi"
	"os"
	"time"
)

const (
	ManifestVersion               = 3
	StateVersion                  = 1
	MaxManifestBytes              = 64 << 10
	MaxArtifacts                  = 64
	MaxRoles                      = 64
	MaxRuntimeBytes         int64 = 4 << 30
	MaxStateBytes                 = 32 << 10
	DefaultMaxArtifactBytes int64 = 512 << 20
	MaxArtifactBytes        int64 = 1 << 30
	MaxTrailerBytes               = 64 << 10
)

var (
	ErrBusy                  = errors.New("release store is busy")
	ErrClosed                = errors.New("release store is closed")
	ErrStale                 = errors.New("release generation or boot attempt is stale")
	ErrDowngrade             = errors.New("release sequence does not exceed the durable high watermark")
	ErrNoActive              = errors.New("no active release")
	ErrNoFallback            = errors.New("failed boot has no previously confirmed release")
	ErrCorrupt               = errors.New("release store is corrupt")
	ErrUncertain             = errors.New("release selector changed but directory durability was not confirmed; read status before retrying")
	ErrCapacity              = errors.New("release store capacity reached; remove unused releases or old generations explicitly")
	ErrConfigurationRequired = errors.New("paired runtime requires explicit configuration preparation and confirmation")
)

type Artifact struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Command string `json:"command,omitempty"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

type RoleBinding struct {
	Role     string `json:"role"`
	Artifact string `json:"artifact"`
}

// Sequence is a publisher-assigned, strictly increasing release counter. A
// release ID is an opaque human-readable identity, never a version comparison.
type Manifest struct {
	Version    int                   `json:"version"`
	ReleaseID  string                `json:"release_id"`
	Sequence   uint64                `json:"sequence"`
	GOOS       string                `json:"goos"`
	GOARCH     string                `json:"goarch"`
	KeyID      string                `json:"key_id"`
	BootABI    int                   `json:"boot_abi"`
	WorkerABI  int                   `json:"worker_abi"`
	RuntimeABI runtimeabi.Descriptor `json:"runtime_abi"`
	Entrypoint string                `json:"entrypoint"`
	Roles      []RoleBinding         `json:"roles"`
	Artifacts  []Artifact            `json:"artifacts"`
}

type Ref struct {
	Digest    string `json:"digest"`
	ReleaseID string `json:"release_id"`
	Sequence  uint64 `json:"sequence"`
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
}

type Pending struct {
	Startup     *configref.Selection `json:"startup,omitempty"`
	Fallback    *configref.Selection `json:"fallback,omitempty"`
	Release     Ref                  `json:"release"`
	MaxAttempts int                  `json:"max_attempts"`
}

type Attempt struct {
	BootID    string    `json:"boot_id"`
	Token     string    `json:"token"`
	Number    int       `json:"number"`
	StartedAt time.Time `json:"started_at"`
}

type Failure struct {
	Startup *configref.Selection `json:"startup,omitempty"`
	Release Ref                  `json:"release"`
	BootID  string               `json:"boot_id,omitempty"`
	Reason  string               `json:"reason"`
	At      time.Time            `json:"at"`
}

// State and its active/pending/previous symlinks are published together by one
// atomic current-symlink replacement. HighWatermark survives cancel/rollback.
type State struct {
	ConfigStoreID       string               `json:"config_store_id,omitempty"`
	ConfigHighWatermark uint64               `json:"config_high_watermark,omitempty"`
	Startup             *configref.Selection `json:"startup,omitempty"`
	PreviousStartup     *configref.Selection `json:"previous_startup,omitempty"`
	Version             int                  `json:"version"`
	Generation          uint64               `json:"generation"`
	HighWatermark       uint64               `json:"high_watermark"`
	Active              *Ref                 `json:"active,omitempty"`
	Pending             *Pending             `json:"pending,omitempty"`
	Previous            *Ref                 `json:"previous,omitempty"`
	Attempt             *Attempt             `json:"attempt,omitempty"`
	LastFailure         *Failure             `json:"last_failure,omitempty"`
	ChangedAt           time.Time            `json:"changed_at"`
	Operation           string               `json:"operation"`
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
	// References must have been verified by the configuration coordinator.
	// The release store deliberately does not import or execute its compiler.
	Startup   *configref.Selection
	KnownGood *configref.Ref
	// A retry requires a different kernel boot ID. Defaults to one attempt.
	MaxAttempts int
}

// Preflight runs under the release store lock, with an independent current-state
// view and a verified target whose artifact descriptors are borrowed for this
// call only. It must not recursively call this Store. The coordinator validates
// live configuration/snapshots and target compatibility here before publication.
type Preflight func(context.Context, State, *Image) error

type ConfigurationConfirmation struct {
	Adoption configref.Adoption
	Revision uint64
}

// Image pins every verified artifact and its signed role catalog. Callers must
// not re-resolve mutable selector paths when launching or restarting workers.
type Image struct {
	Release   Ref
	Manifest  Manifest
	Artifacts map[string]*os.File
}

func (i *Image) Entrypoint() *os.File { return i.Artifacts[i.Manifest.Entrypoint] }
func (i *Image) Executable(role string) (*os.File, error) {
	for _, binding := range i.Manifest.Roles {
		if binding.Role == role {
			if f := i.Artifacts[binding.Artifact]; f != nil {
				return f, nil
			}
		}
	}
	return nil, errors.New("role is absent from the verified runtime catalog")
}
func (i *Image) Close() error {
	if i == nil {
		return nil
	}
	var errs []error
	for name, f := range i.Artifacts {
		if f != nil {
			errs = append(errs, f.Close())
		}
		delete(i.Artifacts, name)
	}
	return errors.Join(errs...)
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
