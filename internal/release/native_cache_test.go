//go:build linux

package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeInspectionMemoRequiresFreshHashAndSignedManifest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	inspections := 0
	s.fault = func(point string) error {
		if point == "native-inspection" {
			inspections++
		}
		return nil
	}
	ref := initializeFixture(t, s)
	if inspections != 2 {
		t.Fatalf("first qualification inspected %d native members, want 2", inspections)
	}
	for i := 0; i < 3; i++ {
		image, err := s.Verify(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		image.Close()
	}
	if inspections != 2 {
		t.Fatal("unchanged fully hashed members were repeatedly parsed", inspections)
	}
	// The manifest digest, not just a payload digest, binds cached command,
	// release, target and ABI expectations. A re-signed lie must miss the memo.
	m, entries := entriesFor(t, "fixture-r1", 1)
	m.RuntimeABI.ConfigurationContract = "resource-program-native-x"
	raw, sig, err := SignManifest(m, testKey())
	if err != nil {
		t.Fatal(err)
	}
	entries[0].body, entries[1].body = raw, sig
	if _, err := s.Stage(ctx, bytes.NewReader(fixtureArchive(t, entries))); err == nil {
		t.Fatal("different signed expectations reused the old native check")
	}
	// A cached parse never substitutes for reading/hash-checking the current
	// file. Restore its mode so this specifically tests the content digest.
	path := filepath.Join(s.options.Root, "releases", ref.Digest, "routerd")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[128] ^= 1
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, ref); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "artifact digest changed") {
		t.Fatal("memo bypassed a fresh payload hash", err)
	}
}
