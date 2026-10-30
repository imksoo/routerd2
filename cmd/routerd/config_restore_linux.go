//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
)

func restoreConfig(args []string, out io.Writer) error {
	f := flags("config restore")
	file := f.String("snapshot", "", "saved SnapshotRef JSON/YAML file (required)")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	revision := f.Uint64("expected-revision", 0, "current committed revision (required)")
	id := f.String("request-id", "", "idempotency key (generated if omitted)")
	confirmed := f.Int("confirmed", 0, "automatically roll back unless confirmed within this many seconds (5..3600)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *file == "" || *revision == 0 {
		return errors.New("--snapshot and --expected-revision are required; no positional arguments")
	}
	input, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer input.Close()
	var ref config.SnapshotRef
	if err := config.Decode(input, &ref); err != nil {
		return err
	}
	if *id == "" {
		var token [16]byte
		if _, err := io.ReadFull(rand.Reader, token[:]); err != nil {
			return err
		}
		*id = hex.EncodeToString(token[:])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state, err := (cli.Session{RuntimeDir: *dir}).RestoreConfiguration(ctx, control.RestoreRequest{Snapshot: ref, ExpectedRevision: *revision, RequestID: *id, ConfirmTimeoutSeconds: *confirmed})
	if err != nil {
		return fmt.Errorf("request_id=%s: %w", *id, err)
	}
	return cli.WriteConfigurationState(out, state)
}
