// SPDX-License-Identifier: BSD-3-Clause

package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/imksoo/routerd2/internal/buildinfo"
)

const signatureDomain = "routerd2 signed release manifest v1\x00"

func KeyID(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}

func validDigest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

func ValidateManifest(m Manifest, maxArtifact int64) error {
	if maxArtifact <= 0 || maxArtifact > MaxArtifactBytes {
		return errors.New("invalid artifact size policy")
	}
	if m.Version != ManifestVersion || m.Sequence == 0 {
		return errors.New("manifest requires version 1 and a nonzero release sequence")
	}
	if _, err := buildinfo.ParseIdentity(buildinfo.IdentityPrefix + m.ReleaseID + buildinfo.IdentitySuffix); err != nil {
		return fmt.Errorf("release ID: %w", err)
	}
	if m.GOOS != "linux" || m.GOARCH != "amd64" && m.GOARCH != "arm64" {
		return errors.New("release target must be linux/amd64 or linux/arm64")
	}
	if !validDigest(m.KeyID) {
		return errors.New("manifest requires a full SHA256 signing key ID")
	}
	if len(m.Artifacts) != 2 {
		return errors.New("manifest must contain exactly routerd and routerd-modules")
	}
	for i, name := range []string{"routerd", "routerd-modules"} {
		a := m.Artifacts[i]
		if a.Name != name || a.Size < 64 || a.Size > maxArtifact || !validDigest(a.SHA256) {
			return fmt.Errorf("invalid %s artifact; names/order, finite size, and SHA256 are required", name)
		}
	}
	return nil
}

// SignManifest returns canonical JSON and a raw 64-byte Ed25519 signature.
// It is intended for the offline publisher. Private keys never enter Store.
func SignManifest(m Manifest, private ed25519.PrivateKey) ([]byte, []byte, error) {
	if len(private) != ed25519.PrivateKeySize {
		return nil, nil, errors.New("invalid Ed25519 private key size")
	}
	public := private.Public().(ed25519.PublicKey)
	if KeyID(public) != m.KeyID {
		return nil, nil, errors.New("manifest key ID does not match signing key")
	}
	if err := ValidateManifest(m, MaxArtifactBytes); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxManifestBytes {
		return nil, nil, errors.New("manifest exceeds size limit")
	}
	signature := ed25519.Sign(private, append([]byte(signatureDomain), raw...))
	return raw, signature, nil
}

// VerifyManifest verifies exact canonical bytes, a locally trusted key, and the
// target before any executable payload is written to a version directory.
func VerifyManifest(raw, signature []byte, keys map[string]ed25519.PublicKey, targetOS, targetArch string, maxArtifact int64) (Manifest, Ref, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifestBytes || len(signature) != ed25519.SignatureSize {
		return m, Ref{}, errors.New("invalid manifest or signature size")
	}
	if err := decodeCanonical(raw, &m); err != nil {
		return m, Ref{}, fmt.Errorf("manifest: %w", err)
	}
	if err := ValidateManifest(m, maxArtifact); err != nil {
		return m, Ref{}, err
	}
	key, ok := keys[m.KeyID]
	if !ok || len(key) != ed25519.PublicKeySize || KeyID(key) != m.KeyID {
		return m, Ref{}, errors.New("manifest signing key is not trusted")
	}
	if !ed25519.Verify(key, append([]byte(signatureDomain), raw...), signature) {
		return m, Ref{}, errors.New("invalid release manifest signature")
	}
	if m.GOOS != targetOS || m.GOARCH != targetArch {
		return m, Ref{}, fmt.Errorf("release target %s/%s differs from %s/%s", m.GOOS, m.GOARCH, targetOS, targetArch)
	}
	sum := sha256.Sum256(raw)
	return m, Ref{Digest: hex.EncodeToString(sum[:]), ReleaseID: m.ReleaseID, Sequence: m.Sequence, GOOS: m.GOOS, GOARCH: m.GOARCH}, nil
}

func decodeCanonical(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	canonical, err := json.Marshal(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, canonical) {
		return errors.New("JSON must be canonical (no duplicate fields, extra whitespace, or alternative encodings)")
	}
	return nil
}

func validRef(r Ref) bool {
	_, err := buildinfo.ParseIdentity(buildinfo.IdentityPrefix + r.ReleaseID + buildinfo.IdentitySuffix)
	return err == nil && validDigest(r.Digest) && r.Sequence != 0 && r.GOOS == "linux" && (r.GOARCH == "amd64" || r.GOARCH == "arm64")
}
