//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
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

func listSnapshots(args []string, out io.Writer) error {
	f := flags("config snapshots")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("config snapshots takes no positional arguments")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return (cli.Session{RuntimeDir: *dir}).Execute(ctx, "show snapshots", nil, out)
}

func deleteSnapshotConfig(args []string, out io.Writer) error {
	f := flags("config delete-snapshot")
	file := f.String("snapshot", "", "saved SnapshotRef JSON/YAML file (required)")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	revision := f.Uint64("expected-revision", 0, "current confirmed revision (required)")
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
	if err := ref.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var removed config.SnapshotRef
	request := control.SnapshotDeleteRequest{ExpectedRevision: *revision, Snapshot: ref}
	if err := localapi.NewClient(filepath.Join(*dir, control.SocketName)).Do(ctx, "POST", "/v1/config/snapshots/delete", request, &removed); err != nil {
		return err
	}
	if removed != ref {
		return errors.New("snapshot deletion acknowledgment does not match the selected reference")
	}
	return cli.WriteJSON(out, removed)
}
