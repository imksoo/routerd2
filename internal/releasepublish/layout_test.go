//go:build linux

package releasepublish

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublisherRejectsOldImplicitPairFlagsAndUnknownLayoutCommands(t *testing.T) {
	err := Run(context.Background(), []string{"pack", "--routerd", "/old/routerd", "--modules", "/old/modules"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatal("old implicit pair publisher accepted", err)
	}
	path := testLayout(t, "/test/routerd", "/test/worker", "/test/logging")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{`,"argv":["sh"]}`, `}{}`} {
		bad := filepath.Join(t.TempDir(), "layout.json")
		if err := os.WriteFile(bad, append(append([]byte(nil), raw[:len(raw)-1]...), []byte(suffix)...), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readLayout(bad); err == nil {
			t.Fatal("unknown/trailing executable command accepted")
		}
	}
}
