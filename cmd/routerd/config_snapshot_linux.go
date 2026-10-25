//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
)

func snapshotConfig(args []string, out io.Writer) error {
	f := flags("config snapshot")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	revision := f.Uint64("expected-revision", 0, "current confirmed revision (required)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *revision == 0 {
		return errors.New("--expected-revision is required; no positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ref config.SnapshotRef
	if err := localapi.NewClient(filepath.Join(*dir, control.SocketName)).Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: *revision}, &ref); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	return cli.WriteJSON(out, ref)
}
