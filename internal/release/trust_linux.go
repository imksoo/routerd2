// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const MaxTrustBytes = 64 << 10

// ParseTrust accepts a single JSON object mapping full SHA256 key IDs to
// canonical padded base64 Ed25519 public keys. Whitespace is allowed; duplicate
// keys, trailing JSON, invalid encodings, and more than 64 keys are rejected.
func ParseTrust(raw []byte) (map[string]ed25519.PublicKey, error) {
	if len(raw) == 0 || len(raw) > MaxTrustBytes {
		return nil, errors.New("trust policy size is invalid")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("trust policy must be a JSON object")
	}
	keys := map[string]ed25519.PublicKey{}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid trust key entry")
		}
		id, ok := token.(string)
		if !ok || !validDigest(id) {
			return nil, errors.New("trust key ID must be a full lower-case SHA256 digest")
		}
		if _, exists := keys[id]; exists {
			return nil, errors.New("duplicate trust key ID")
		}
		if len(keys) >= 64 {
			return nil, errors.New("trust policy exceeds 64 keys")
		}
		var value string
		if err = d.Decode(&value); err != nil {
			return nil, errors.New("trust key value must be a base64 string")
		}
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != value {
			return nil, errors.New("trust key must be a canonical base64 Ed25519 public key")
		}
		if KeyID(ed25519.PublicKey(key)) != id {
			return nil, errors.New("trust key ID does not match its public key")
		}
		keys[id] = ed25519.PublicKey(key)
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') || len(keys) == 0 {
		return nil, errors.New("trust policy requires 1..64 public keys")
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trust policy has trailing JSON")
	}
	return keys, nil
}

// LoadTrustFile reads local trust policy through anchored, no-follow directory
// descriptors. Public key files may be world-readable, but neither the file
// nor non-sticky parent directories may be group/other writable. No private
// keys, environment lookups, external commands, or network sources are used.
func LoadTrustFile(path string) (map[string]ed25519.PublicKey, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, errors.New("trust file must use a clean absolute path")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), "/")
	defer func() { _ = current.Close() }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, part := range parts {
		directory := i < len(parts)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if directory {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(int(current.Fd()), part, flags, 0)
		if err != nil {
			return nil, fmt.Errorf("open trust policy component: %w", err)
		}
		f := os.NewFile(uintptr(next), part)
		var stat unix.Stat_t
		if err = unix.Fstat(next, &stat); err != nil {
			f.Close()
			return nil, err
		}
		ownerOK := stat.Uid == 0 || stat.Uid == uint32(os.Geteuid())
		writable := stat.Mode&0022 != 0
		stickyParent := directory && stat.Uid == 0 && stat.Mode&unix.S_ISVTX != 0
		if !ownerOK || writable && !stickyParent {
			f.Close()
			return nil, errors.New("trust policy and parents must be owned by root/current UID and not group/other writable")
		}
		if directory {
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				f.Close()
				return nil, errors.New("trust policy ancestor is not a directory")
			}
			current.Close()
			current = f
			continue
		}
		defer f.Close()
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Size <= 0 || stat.Size > MaxTrustBytes {
			return nil, errors.New("trust policy must be a bounded regular file with one link")
		}
		raw, err := io.ReadAll(io.LimitReader(f, MaxTrustBytes+1))
		if err != nil {
			return nil, err
		}
		return ParseTrust(raw)
	}
	return nil, errors.New("missing trust file")
}
