//go:build linux

package management

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func TestHostIdentityInspectionDoesNotInitializeOrExposePrivateMaterial(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), HostKeyName)
	absent, err := InspectHostKey(ctx, path)
	if err != nil || absent != (HostKeyInspection{}) {
		t.Fatal("missing identity inspection", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatal("inspection initialized identity", err)
	}
	if _, err := InspectHostKey(ctx, filepath.Join(filepath.Dir(path), "missing", HostKeyName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing parent is not observed file absence", err)
	}
	signer, err := HostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("persisted identity signer must retain its private material")
	signature, err := signer.Sign(rand.Reader, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.PublicKey().Verify(message, signature); err != nil {
		t.Fatal("scratch cleanup invalidated the returned signer", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := InspectHostKey(ctx, path)
	if err != nil || !got.Present || got.Version != IdentityVersion || got.Type != signer.PublicKey().Type() || got.Fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatal("public identity inspection", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("inspection replaced identity", err)
	}
	now, err := os.Stat(path)
	if err != nil || !os.SameFile(info, now) || info.Mode() != now.Mode() || info.ModTime() != now.ModTime() {
		t.Fatal("inspection changed identity inode or metadata", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := InspectHostKey(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled inspection proceeded", err)
	}
}

func TestHostIdentityReadsExistingOpenSSHEncodingWithoutConversion(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	block, err := ssh.MarshalPrivateKey(key, "existing imported identity")
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(block)
	defer clear(raw)
	defer clear(block.Bytes)
	path := filepath.Join(t.TempDir(), HostKeyName)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	signer, err := HostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), mustPublicKey(t, key)) {
		t.Fatal("existing OpenSSH identity changed")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("existing encoding was migrated or rewritten", err)
	}
}

func TestHostIdentityReadsAnOwnerReadOnlyKeyWithoutChangingItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), HostKeyName)
	original, err := HostKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	got, err := InspectHostKey(context.Background(), path)
	if err != nil || got.Fingerprint != ssh.FingerprintSHA256(original.PublicKey()) {
		t.Fatal("safe read-only key refused", err)
	}
	if _, err := HostKey(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal("read-only key permissions changed", err)
	}
}

func mustPublicKey(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey().Marshal()
}

func TestHostIdentityPublicationFailureKeepsItsDurabilityClassification(t *testing.T) {
	for _, stage := range []string{"file-synced", "published", "directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), HostKeyName)
			cut := errors.New("injected identity interruption")
			_, err := hostKey(context.Background(), path, func(at string) error {
				if at == stage {
					return cut
				}
				return nil
			})
			published := stage != "file-synced"
			if !errors.Is(err, cut) || errors.Is(err, ErrIdentityUncertain) != published {
				t.Fatal("publication failure classification", err)
			}
			before, err := InspectHostKey(context.Background(), path)
			if err != nil || before.Present != published {
				t.Fatal("wrong publication visibility", err)
			}
			signer, err := HostKey(path)
			if err != nil {
				t.Fatal(err)
			}
			if published && ssh.FingerprintSHA256(signer.PublicKey()) != before.Fingerprint {
				t.Fatal("uncertain publication retry replaced identity")
			}
		})
	}
}

func TestHostIdentityRejectsSpecialOversizeAndCorruptFilesWithoutRepair(t *testing.T) {
	for _, kind := range []string{"fifo", "empty", "oversize", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), HostKeyName)
			if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "oversize" {
					if err := f.Truncate(MaxHostKeyBytes + 1); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "corrupt" {
					if _, err := f.WriteString("PRIVATE_TEST_SENTINEL_invalid_key"); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for _, read := range []func() error{func() error { _, err := InspectHostKey(context.Background(), path); return err }, func() error { _, err := HostKey(path); return err }} {
				if err := read(); err == nil || strings.Contains(err.Error(), "PRIVATE_TEST_SENTINEL") {
					t.Fatal("invalid identity accepted or contents echoed", err)
				}
			}
		})
	}
}

func TestHostIdentityConcurrentCreatorsOnlyReturnThePublishedWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), HostKeyName)
	ready := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	type result struct {
		signer ssh.Signer
		err    error
	}
	first := make(chan result, 1)
	go func() {
		s, err := hostKey(context.Background(), path, func(stage string) error {
			if stage == "file-synced" {
				close(ready)
				<-resume
			}
			return nil
		})
		first <- result{s, err}
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("creator did not reach temporary file sync")
	}
	absent, err := InspectHostKey(context.Background(), path)
	if err != nil || absent.Present {
		t.Fatal("unpublished key visible under final name", err)
	}
	winner, err := HostKey(path)
	if err != nil {
		t.Fatal("concurrent reader saw partial key", err)
	}
	unblock()
	r := <-first
	if r.err != nil || !bytes.Equal(winner.PublicKey().Marshal(), r.signer.PublicKey().Marshal()) {
		t.Fatal("losing creator returned an unselected key", r.err)
	}
}

func TestHostIdentityPublicationCrash(t *testing.T) {
	const stageEnv = "ROUTERD2_IDENTITY_CRASH_STAGE"
	const pathEnv = "ROUTERD2_IDENTITY_CRASH_PATH"
	if stage := os.Getenv(stageEnv); stage != "" {
		_, err := hostKey(context.Background(), os.Getenv(pathEnv), func(at string) error {
			if at == stage {
				os.Exit(73)
			}
			return nil
		})
		t.Fatal("crash point was not reached", err)
	}
	for _, stage := range []string{"created", "written", "file-synced", "published", "directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), HostKeyName)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(exe, "-test.run=^TestHostIdentityPublicationCrash$", "-test.count=1")
			child.Env = append(os.Environ(), stageEnv+"="+stage, pathEnv+"="+path)
			output, err := child.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash child: %v\n%s", err, output)
			}
			before, err := InspectHostKey(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			published := stage == "published" || stage == "directory-synced"
			if before.Present != published {
				t.Fatal("identity visibility disagrees with publication boundary")
			}
			entries, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			orphans := map[string]bool{}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".ssh-host-key-") {
					info, err := e.Info()
					if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
						t.Fatal("unsafe crash orphan", err)
					}
					orphans[e.Name()] = true
				}
			}
			if !published && len(orphans) != 1 {
				t.Fatal("missing ignored private crash orphan")
			}
			signer, err := HostKey(path)
			if err != nil {
				t.Fatal(err)
			}
			if published && ssh.FingerprintSHA256(signer.PublicKey()) != before.Fingerprint {
				t.Fatal("retry rotated a published identity")
			}
			for name := range orphans {
				if _, err := os.Stat(filepath.Join(filepath.Dir(path), name)); err != nil {
					t.Fatal("retry deleted prior evidence", err)
				}
			}
		})
	}
}
