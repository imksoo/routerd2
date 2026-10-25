//go:build linux

// routerd-logging is the independently built logging worker. It does not link
// the configuration coordinator, kernel network writer or protocol engines.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("logging worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "version" {
		return json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
	}
	if len(args) == 0 || args[0] != "worker" {
		return errors.New("logging must be started by routerd")
	}
	f := flag.NewFlagSet("logging", flag.ContinueOnError)
	role := f.String("role", "", "logging role")
	name := f.String("name", "", "instance name")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *role != "logging" || *name == "" {
		return errors.New("logging command accepts only its logging role and instance name")
	}
	dir, state, path := os.Getenv("ROUTERD_RUNTIME_DIR"), os.Getenv("ROUTERD_STATE_DIR"), os.Getenv("ROUTERD_CONFIG_PATH")
	if dir == "" || state == "" || path == "" {
		return errors.New("missing supervisor environment")
	}
	generation, err := strconv.ParseUint(os.Getenv("ROUTERD_GENERATION"), 10, 64)
	if err != nil || generation == 0 {
		return errors.New("invalid supervisor generation")
	}
	instance := os.Getenv(worker.SupervisorInstanceEnv)
	id, err := hex.DecodeString(instance)
	if len(instance) != 32 || err != nil || len(id) != 16 {
		return errors.New("invalid supervisor instance")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return worker.Run(ctx, *role, buildinfo.ReleaseID, func(ctx context.Context, ready func()) error {
		return logging.Run(ctx, logging.Options{RuntimeDir: dir, StateDir: state, ReleaseID: buildinfo.ReleaseID, CaptureKernel: os.Getenv("ROUTERD_PLATFORM") == "appliance"}, ready)
	})
}
