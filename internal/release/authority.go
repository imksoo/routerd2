// SPDX-License-Identifier: BSD-3-Clause

package release

import (
	"errors"
	"path/filepath"
)

const AuthorityEnv = "ROUTERD_RELEASE_AUTHORITY"
const MaxAuthorityBytes = 4096

// Authority is private supervisor-to-control context from the verified boot
// selection. It is not operator configuration or an authorization credential.
// Filesystem ownership, signed manifests and the store lock remain authoritative.
type Authority struct {
	Root      string `json:"root"`
	TrustFile string `json:"trust_file"`
	Digest    string `json:"digest"`
}

func (a Authority) Validate() error {
	for _, path := range []string{a.Root, a.TrustFile} {
		if len(path) > 1024 || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
			return errors.New("release authority requires bounded clean absolute paths")
		}
	}
	if !validDigest(a.Digest) {
		return errors.New("release authority requires a full lower-case manifest digest")
	}
	return nil
}

// DecodeAuthority accepts only the bounded canonical encoding emitted by the
// supervisor. Empty input is standalone mode; ambiguous aliases/duplicates and
// inherited nonempty garbage never silently select standalone mode.
func DecodeAuthority(raw []byte) (*Authority, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxAuthorityBytes {
		return nil, errors.New("release authority exceeds its size bound")
	}
	var a Authority
	if err := decodeCanonical(raw, &a); err != nil {
		return nil, err
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return &a, nil
}
