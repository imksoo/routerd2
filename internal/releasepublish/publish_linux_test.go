// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package releasepublish

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/imksoo/routerd2/internal/release"
)

func testKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(der)
	path := filepath.Join(t.TempDir(), "publisher.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	return path, public
}

func TestTrustMatchesPublisherAndContainsOnlyPublicKey(t *testing.T) {
	path, public := testKey(t)
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"trust", "--private-key-file", path}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var trust map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &trust); err != nil {
		t.Fatal(err)
	}
	if len(trust) != 1 || trust[release.KeyID(public)] != base64.StdEncoding.EncodeToString(public) || stderr.Len() != 0 {
		t.Fatal("trust output differs from the signing public key")
	}
	if _, err := release.ParseTrust(stdout.Bytes()); err != nil {
		t.Fatalf("publisher output is not a runtime trust map: %v", err)
	}
}

func TestPublisherRejectsExposedLinkedAndMalformedKeys(t *testing.T) {
	path, _ := testKey(t)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("publicly readable private key accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "key")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("multiply-linked private key accepted")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(link); err == nil {
		t.Fatal("private key symlink accepted")
	}
	parentLink := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(filepath.Dir(path), parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(filepath.Join(parentLink, "publisher.pem")); err == nil {
		t.Fatal("private key parent symlink accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(raw)
	if err := os.WriteFile(path, append(raw, raw...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("multiple private keys accepted")
	}
	broken := []byte("-----BEGIN PRIVATE KEY-----\n!invalid-base64!\n-----END PRIVATE KEY-----\n")
	if err := os.WriteFile(path, append(broken, raw...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err == nil {
		t.Fatal("malformed first key was skipped in favor of a later key")
	}
}

func TestAtomicPublicationRejectsOverwriteAndLeavesNoPartialBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release.tar")
	boom := errors.New("signing or input validation failed")
	err := publish(context.Background(), path, func(f *os.File) error { _, _ = f.WriteString("partial"); return boom })
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed publisher leaked a partial output", entries, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = publish(ctx, path, func(f *os.File) error { cancel(); _, err := f.WriteString("canceled"); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, contents := range []string{"publisher A", "publisher B"} {
		wg.Go(func() {
			results <- publish(context.Background(), path, func(f *os.File) error { _, err := f.WriteString(contents); return err })
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("concurrent publishers replaced each other", successes)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "publisher A" && string(raw) != "publisher B" {
		t.Fatal("output is incomplete", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary publication artifacts leaked", entries, err)
	}
}

func TestPackRejectsUnverifiedELFWithoutPublishing(t *testing.T) {
	path, _ := testKey(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "fake")
	if err := os.WriteFile(input, []byte(strings.Repeat("not an ELF", 20)), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "release.tar")
	err := Run(context.Background(), []string{"pack", "--private-key-file", path, "--layout", testLayout(t, input, input, input), "--release-id", "test", "--sequence", "1", "--target-arch", "amd64", "--output", output}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("invalid incoming executables were signed")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid bundle was published", err)
	}
}

func testLayout(t *testing.T, supervisor, modules, logger string) string {
	t.Helper()
	l := Layout{BootABI: 1, WorkerABI: 4, Entrypoint: "routerd", Roles: []release.RoleBinding{{Role: "logging", Artifact: "routerd-logging"}, {Role: "control", Artifact: "routerd-modules"}, {Role: "network", Artifact: "routerd-modules"}}, Artifacts: []ArtifactInput{{Name: "routerd", Kind: "native", Command: "routerd", File: supervisor}, {Name: "routerd-modules", Kind: "native", Command: "routerd-modules", File: modules}, {Name: "routerd-logging", Kind: "native", Command: "routerd-logging", File: logger}}}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "layout.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
