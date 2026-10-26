//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/management"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/serviceruntime"
	"github.com/imksoo/routerd2/internal/worker"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(os.Args[1:]); err != nil {
		slog.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "version" {
		return cli.WriteJSON(os.Stdout, buildinfo.Current())
	}
	if len(args) == 0 || args[0] != "worker" {
		return errors.New("workers must be started by routerd")
	}
	f := flag.NewFlagSet("worker", flag.ContinueOnError)
	role := f.String("role", "", "module role")
	name := f.String("name", "", "instance name")
	managementPath := f.String("management-config", "", "private SSH configuration")
	instancePath := f.String("instance-config", "", "immutable private protocol configuration")
	configDigest := f.String("config-digest", "", "immutable configuration SHA-256")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *role == "" || *name == "" {
		return errors.New("worker role and instance name are required")
	}
	dir, state, path := os.Getenv("ROUTERD_RUNTIME_DIR"), os.Getenv("ROUTERD_STATE_DIR"), os.Getenv("ROUTERD_CONFIG_PATH")
	if dir == "" || state == "" || path == "" {
		return errors.New("missing supervisor environment")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	generation, generationErr := strconv.ParseUint(os.Getenv("ROUTERD_GENERATION"), 10, 64)
	if generationErr != nil || generation == 0 {
		return errors.New("invalid supervisor generation")
	}
	if !serviceconfig.ValidSupervisorInstance(os.Getenv(worker.SupervisorInstanceEnv)) {
		return errors.New("invalid supervisor instance")
	}
	if *role != "logging" {
		handler, err := logging.NewHandler(logging.HandlerOptions{RuntimeDir: dir, Module: *name, Generation: generation, Fallback: os.Stderr})
		if err != nil {
			return err
		}
		slog.SetDefault(slog.New(handler))
		defer func() {
			flushCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			_ = handler.Close(flushCtx)
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
		}()
	}
	return worker.Run(ctx, *role, buildinfo.ReleaseID, func(ctx context.Context, ready func()) error {
		switch *role {
		case "bfd", "health", "dhcp4", "dhcp6", "dhcp_server", "dhcp_relay", "ra", "ra_client", "aftr", "dns", "vrrp", "bgp":
			return serviceruntime.Run(ctx, dir, *name, *role, *instancePath, *configDigest, generation, ready)
		case "logging":
			return logging.Run(ctx, logging.Options{RuntimeDir: dir, StateDir: state, ReleaseID: buildinfo.ReleaseID, CaptureKernel: os.Getenv("ROUTERD_PLATFORM") == "appliance"}, ready)
		case "network":
			return control.ServeNetwork(ctx, dir, state, ready)
		case "control":
			startup, err := configref.DecodeSelection([]byte(os.Getenv(configref.SelectionEnv)))
			if err != nil {
				return err
			}
			authority, err := release.DecodeAuthority([]byte(os.Getenv(release.AuthorityEnv)))
			if err != nil {
				return err
			}
			return control.Serve(ctx, dir, state, path, startup, authority, ready)
		case "management":
			b, err := os.ReadFile(*managementPath)
			if err != nil {
				return err
			}
			var opts runtimeconfig.SSH
			if err = json.Unmarshal(b, &opts); err != nil {
				return err
			}
			return management.Serve(ctx, opts, dir, state, ready)
		default:
			return fmt.Errorf("unknown module role %q", *role)
		}
	})
}
