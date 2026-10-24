package network

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A binding is public metadata. Source files are imported once and immutable
// snapshots survive configuration rollback; their bytes never enter a journal.
type wireGuardSecretBinding struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func validWireGuardSecretID(id string) bool {
	data, err := hex.DecodeString(id)
	return err == nil && len(data) == sha256.Size && hex.EncodeToString(data) == id
}

func wireGuardSecretID(key wireGuardKey) string {
	sum := sha256.Sum256(key[:])
	return hex.EncodeToString(sum[:])
}

func clampWireGuardPrivate(key *wireGuardKey) {
	key[0] &= 248
	key[31] = key[31]&127 | 64
}

// Refuse symlinks in every component, including a projected Secret's final
// component. The native reader pins the parent directory and uses a nonblocking
// no-follow open before inspecting the file, so a replacement FIFO cannot hang
// the worker. Callers install versioned 0600 regular files.
func readPrivateRegularFile(path string, maximum int64) ([]byte, error) {
	f, err := openWireGuardSecretFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 || !privateFileOwner(after) {
		return nil, errors.New("secret must be a regular file owned by the worker, with no group/other permissions")
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		clear(data)
		return nil, errors.New("secret file could not be read within the size limit")
	}
	return data, nil
}

func readWireGuardSource(path, kind string) (wireGuardKey, error) {
	data, err := readPrivateRegularFile(path, 128)
	if err != nil {
		return wireGuardKey{}, err
	}
	defer clear(data)
	key, err := parseWireGuardKey(strings.TrimSpace(string(data)))
	if err != nil || key.zero() {
		return wireGuardKey{}, errors.New("secret file must contain one nonzero base64-encoded 32-byte key")
	}
	if kind == "private" {
		clampWireGuardPrivate(&key)
	}
	return key, nil
}

func syncWireGuardDirectory(directory string) error {
	f, err := openWireGuardSecretDirectory(directory)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func generateWireGuardSource(path string) error {
	var key wireGuardKey
	if _, err := rand.Read(key[:]); err != nil {
		return errors.New("WireGuard key generation failed")
	}
	defer clear(key[:])
	clampWireGuardPrivate(&key)
	data := []byte(key.encoded() + "\n")
	defer clear(data)
	if err := publishWireGuardSecretFile(path, data, nil); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil // Resolve and validate the existing file; never overwrite it.
		}
		return fmt.Errorf("could not persist the generated WireGuard key: %w", err)
	}
	return nil
}

func (e *Engine) wireGuardSecretDirectory() (string, error) {
	directory := e.opts.StatePath + ".wg-secrets"
	if err := prepareWireGuardSecretDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func (e *Engine) loadWireGuardSecret(id string) (wireGuardKey, error) {
	if !validWireGuardSecretID(id) {
		return wireGuardKey{}, errors.New("invalid WireGuard secret reference")
	}
	directory, err := e.wireGuardSecretDirectory()
	if err != nil {
		return wireGuardKey{}, err
	}
	data, err := readPrivateRegularFile(filepath.Join(directory, id), 32)
	if err != nil {
		return wireGuardKey{}, fmt.Errorf("read immutable WireGuard secret: %w", err)
	}
	defer clear(data)
	var key wireGuardKey
	if len(data) != len(key) {
		return key, errors.New("invalid immutable WireGuard secret length")
	}
	copy(key[:], data)
	if wireGuardSecretID(key) != id || key.zero() {
		clear(key[:])
		return key, errors.New("immutable WireGuard secret failed its integrity check")
	}
	return key, nil
}

func (e *Engine) saveWireGuardSecret(key wireGuardKey) (string, error) {
	directory, err := e.wireGuardSecretDirectory()
	if err != nil {
		return "", err
	}
	id := wireGuardSecretID(key)
	path := filepath.Join(directory, id)
	data := append([]byte(nil), key[:]...)
	defer clear(data)
	err = publishWireGuardSecretFile(path, data, nil)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := e.loadWireGuardSecret(id)
		defer clear(existing[:])
		if readErr != nil || !key.equal(existing) {
			return "", errors.New("existing immutable WireGuard secret does not match")
		}
		if err := syncWireGuardDirectory(filepath.Dir(directory)); err != nil {
			return "", err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	if err := syncWireGuardDirectory(filepath.Dir(directory)); err != nil {
		return "", err
	}
	return id, nil
}

func (p *apply) resolveWireGuardSecret(path, kind string, generate bool) (string, error) {
	for _, binding := range p.e.state.WireGuardSecrets {
		if binding.Path == path && binding.Kind == kind {
			// Validate the immutable copy on every apply, but never substitute a
			// changed source file for a previously bound key during recovery.
			key, err := p.e.loadWireGuardSecret(binding.ID)
			clear(key[:])
			return binding.ID, err
		}
	}
	if generate {
		if err := generateWireGuardSource(path); err != nil {
			return "", fmt.Errorf("generate private key file: %w", err)
		}
	}
	key, err := readWireGuardSource(path, kind)
	if err != nil {
		return "", fmt.Errorf("read %s key file: %w", kind, err)
	}
	defer clear(key[:])
	id, err := p.e.saveWireGuardSecret(key)
	if err != nil {
		return "", err
	}
	p.e.state.WireGuardSecrets = append(p.e.state.WireGuardSecrets, wireGuardSecretBinding{Path: path, Kind: kind, ID: id})
	if err := p.checkpoint("WireGuard secret binding"); err != nil {
		return "", err
	}
	return id, nil
}
