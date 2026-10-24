// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictTrustPolicyAndFilesystemBoundary(t *testing.T) {
	key := testKey().Public().(ed25519.PublicKey)
	id, encoded := KeyID(key), base64.StdEncoding.EncodeToString(key)
	raw, err := json.MarshalIndent(map[string]string{id: encoded}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseTrust(raw)
	if err != nil || len(parsed) != 1 || string(parsed[id]) != string(key) {
		t.Fatalf("trust: %v", err)
	}
	for name, bad := range map[string]string{
		"empty": "{}", "null": "null", "array": "[]",
		"duplicate":      `{"` + id + `":"` + encoded + `","` + id + `":"` + encoded + `"}`,
		"wrong ID":       `{"` + strings.Repeat("a", 64) + `":"` + encoded + `"}`,
		"non-string":     `{"` + id + `":42}`,
		"short key":      `{"` + id + `":"YQ=="}`,
		"unpadded":       `{"` + id + `":"` + strings.TrimRight(encoded, "=") + `"}`,
		"base64 newline": `{"` + id + `":"` + encoded[:10] + `\n` + encoded[10:] + `"}`,
		"trailing":       string(raw) + "{}",
		"oversized":      strings.Repeat(" ", MaxTrustBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTrust([]byte(bad)); err == nil {
				t.Fatal("invalid trust policy accepted")
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustFile(path); err == nil {
		t.Fatal("group-writable trust policy accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "symlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustFile(filepath.Join(dir, "symlink")); err == nil {
		t.Fatal("trust symlink accepted")
	}
	redirect := filepath.Join(t.TempDir(), "redirect")
	if err := os.Symlink(dir, redirect); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustFile(filepath.Join(redirect, "trust.json")); err == nil {
		t.Fatal("trust ancestor symlink accepted")
	}
	if err := os.Link(path, filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTrustFile(path); err == nil {
		t.Fatal("hard-linked trust policy accepted")
	}
}
