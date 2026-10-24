//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
)

func migrationFixture(name string) string {
	return filepath.Join("..", "..", "internal", "legacyconfig", "testdata", name)
}

func TestMigrateCLIExplicitPolicy(t *testing.T) {
	var output, notes bytes.Buffer
	input := migrationFixture("wan-dynamic.yaml")
	if err := migrate([]string{input}, &output, &notes); err == nil {
		t.Fatal("implicit WAN host defaults were accepted")
	}
	if output.Len() != 0 || notes.Len() != 0 {
		t.Fatal("failed migration emitted partial output")
	}
	if err := migrate([]string{"--policy", migrationFixture("wan-dynamic-policy.yaml"), input}, &output, &notes); err != nil {
		t.Fatal(err)
	}
	var doc config.Document
	if err := config.Decode(bytes.NewReader(output.Bytes()), &doc); err != nil {
		t.Fatalf("stdout is not a native document: %v", err)
	}
	if err := config.Validate(doc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes.String(), "migration ") || !strings.Contains(notes.String(), "IPv6RAAddress/") || !strings.Contains(notes.String(), "DSLiteTunnel/") {
		t.Fatalf("missing review notes: %s", notes.String())
	}
}

func TestMigrateCLIRejectsInvalidPolicyAndArgumentsAtomically(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--policy", migrationFixture("wan-dynamic-policy.yaml")},
		{migrationFixture("wan.yaml"), "extra"},
		{"--policy", migrationFixture("wan.yaml"), migrationFixture("wan-dynamic.yaml")},
		{"--policy", filepath.Join(t.TempDir(), "missing-policy"), migrationFixture("wan-dynamic.yaml")},
	} {
		var output bytes.Buffer
		if err := migrate(args, &output, io.Discard); err == nil || output.Len() != 0 {
			t.Fatalf("args %v: err=%v bytes=%d", args, err, output.Len())
		}
	}
}

type failedMigrationNotes struct{}

func (failedMigrationNotes) Write([]byte) (int, error) { return 0, errors.New("review sink failed") }

func TestMigrateCLIDiagnosticFailureDoesNotEmitConfiguration(t *testing.T) {
	var output bytes.Buffer
	err := migrate([]string{"--policy", migrationFixture("wan-dynamic-policy.yaml"), migrationFixture("wan-dynamic.yaml")}, &output, failedMigrationNotes{})
	if err == nil || !strings.Contains(err.Error(), "review sink failed") || output.Len() != 0 {
		t.Fatalf("err=%v bytes=%d", err, output.Len())
	}
}
