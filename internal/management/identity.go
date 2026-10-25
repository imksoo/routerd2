package management

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"

	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/crypto/ssh"
)

const IdentityVersion = 1 // existing unencrypted private-key codec, no new header
const HostKeyName = "ssh_host_ed25519_key"
const MaxHostKeyBytes = 64 << 10

var ErrIdentityUncertain = errors.New("host identity publication durability unconfirmed; inspect and retry without deleting the existing key")

// HostKeyInspection returns public identity metadata only. Version0 with
// Present=false is observed absence after opening a checked existing parent.
type HostKeyInspection struct {
	Present     bool   `json:"present"`
	Version     int    `json:"version"`
	Type        string `json:"type"`
	Fingerprint string `json:"public_key_fingerprint"`
}

func InspectHostKey(ctx context.Context, path string) (HostKeyInspection, error) {
	if err := ctx.Err(); err != nil {
		return HostKeyInspection{}, err
	}
	dir, name, err := statefs.OpenParent(path, false)
	if err != nil {
		return HostKeyInspection{}, err
	}
	defer dir.Close()
	signer, err := readHostIdentity(ctx, dir, name)
	if errors.Is(err, os.ErrNotExist) {
		return HostKeyInspection{}, nil
	}
	if err != nil {
		return HostKeyInspection{}, err
	}
	return HostKeyInspection{Present: true, Version: IdentityVersion, Type: signer.PublicKey().Type(), Fingerprint: ssh.FingerprintSHA256(signer.PublicKey())}, nil
}

func readHostIdentity(ctx context.Context, dir *os.File, name string) (ssh.Signer, error) {
	raw, err := statefs.ReadPrivate(ctx, dir, name, MaxHostKeyBytes)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	trimmed := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return nil, errors.New("host identity must contain one supported unencrypted private key")
	}
	block, rest := pem.Decode(trimmed)
	if block == nil {
		return nil, errors.New("host identity must contain one supported unencrypted private key")
	}
	defer clear(block.Bytes)
	if len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("host identity must contain exactly one unencrypted private key without trailing data")
	}
	signer, err := ssh.ParsePrivateKey(trimmed)
	if err != nil {
		return nil, errors.New("host identity private key could not be parsed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return signer, nil
}

// HostKey explicitly initializes a missing identity. Inspection never calls it.
// Existing malformed keys are errors; creation never replaces an existing key.
func HostKey(path string) (ssh.Signer, error) { return hostKey(context.Background(), path, nil) }

func hostKey(ctx context.Context, path string, after func(string) error) (ssh.Signer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, name, err := statefs.OpenParent(path, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	signer, err := readHostIdentity(ctx, dir, name)
	if err == nil {
		if err := dir.Sync(); err != nil {
			return nil, errors.Join(ErrIdentityUncertain, err)
		}
		return signer, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(der)
	raw := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	defer clear(raw)
	if err := publishHostIdentity(ctx, dir, name, raw, after); err != nil && (errors.Is(err, ErrIdentityUncertain) || !errors.Is(err, os.ErrExist)) {
		return nil, err
	}
	// Reread the selected inode, including a concurrent winner's key. Returning
	// the discarded generated signer would advertise the wrong public identity.
	signer, err = readHostIdentity(ctx, dir, name)
	if err != nil {
		return nil, err
	}
	if err := dir.Sync(); err != nil {
		return nil, errors.Join(ErrIdentityUncertain, err)
	}
	return signer, nil
}
