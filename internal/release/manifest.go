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
	"regexp"
	"strings"

	"github.com/imksoo/routerd2/internal/buildinfo"
)

var componentName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

const signatureDomain = "routerd2 signed extracted runtime manifest v2\x00"

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
		return errors.New("manifest requires version 2 and a nonzero release sequence")
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
	if m.BootABI != 1 || m.WorkerABI != 1 {
		return errors.New("unsupported boot or worker ABI")
	}
	if len(m.Artifacts) == 0 || len(m.Artifacts) > MaxArtifacts || len(m.Roles) == 0 || len(m.Roles) > MaxRoles {
		return errors.New("runtime artifact or role count exceeds supported bounds")
	}
	artifacts := map[string]Artifact{}
	var total int64
	for _, a := range m.Artifacts {
		if !componentName.MatchString(a.Name) || a.Name == "manifest.json" || a.Name == "manifest.sig" || a.Size < 1 || a.Size > maxArtifact || !validDigest(a.SHA256) {
			return fmt.Errorf("invalid runtime artifact %q", a.Name)
		}
		if _, exists := artifacts[a.Name]; exists {
			return errors.New("duplicate runtime artifact")
		}
		switch a.Kind {
		case "native":
			if a.Size < 64 || !componentName.MatchString(a.Command) {
				return errors.New("native artifact requires a bounded static ELF and command identity")
			}
		case "data":
			if a.Command != "" {
				return errors.New("data artifact cannot declare an executable command")
			}
		default:
			return errors.New("unsupported artifact kind; only static native and regular data are supported")
		}
		if a.Size > MaxRuntimeBytes-total {
			return errors.New("runtime aggregate size exceeds supported bound")
		}
		total += a.Size
		artifacts[a.Name] = a
	}
	entry, ok := artifacts[m.Entrypoint]
	if !ok || entry.Kind != "native" || entry.Command != "routerd" {
		return errors.New("runtime entrypoint must name the native routerd supervisor")
	}
	roles := map[string]bool{}
	for _, binding := range m.Roles {
		a, exists := artifacts[binding.Artifact]
		if !componentName.MatchString(binding.Role) || roles[binding.Role] || !exists || a.Kind != "native" {
			return errors.New("invalid, duplicate or non-executable runtime role binding")
		}
		roles[binding.Role] = true
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
