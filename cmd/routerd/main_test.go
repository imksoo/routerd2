//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigMigrateCommandDoesNotReadLegacySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.yaml")
	if err := os.WriteFile(path, []byte("legacy router settings\n"), 0600); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"config", "migrate", path})
	if err == nil || err.Error() != "unknown config command" {
		t.Fatalf("legacy migration command was accepted or input was read: %v", err)
	}
}
