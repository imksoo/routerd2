//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"golang.org/x/sys/unix"
)

var applianceRuntime bool

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	args := os.Args[1:]
	if len(args) == 0 && os.Getpid() == 1 {
		args = []string{"boot", "--file=/etc/routerd/boot.json"}
	}
	if err := run(args); err != nil {
		slog.Error("routerd stopped", "error", err)
		if os.Getpid() == 1 {
			// Returning from global PID 1 would panic the kernel. No network or
			// shell recovery is attempted here; the bootloader must bound retries.
			// Container mode exits normally in run(), without entering this path.
			if applianceRuntime || (len(args) > 0 && args[0] == "boot") {
				halt(true)
			}
		}
		os.Exit(1)
	}
}

func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	return f
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "version":
		return cli.WriteJSON(os.Stdout, buildinfo.Current())
	case "boot":
		f := flags("boot")
		path := f.String("file", "/etc/routerd/boot.json", "boot configuration")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected boot arguments")
		}
		b, err := runtimeconfig.Read(*path)
		if err != nil {
			return err
		}
		return serve(b)
	case "run":
		b := runtimeconfig.Default()
		f := flags("run")
		f.StringVar(&b.Platform, "platform", "", "appliance, container, or host (required)")
		f.StringVar(&b.Config, "config", b.Config, "initial network configuration")
		f.StringVar(&b.ModulePath, "module-path", b.ModulePath, "worker executable")
		f.StringVar(&b.RuntimeDir, "runtime-dir", b.RuntimeDir, "private runtime directory")
		f.StringVar(&b.StateDir, "state-dir", b.StateDir, "persistent state directory")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected run arguments")
		}
		return serve(b)
	case "status":
		return remote(args[1:], "show status")
	case "network":
		if len(args) > 1 && args[1] == "show" {
			return remote(args[2:], "show network")
		}
	case "modules":
		if len(args) > 2 && args[1] == "restart" {
			return remote(args[3:], "restart "+args[2])
		}
	case "config":
		return configure(args[1:])
	case "help", "--help", "-h":
		return usage()
	}
	return errors.New("unknown command; run routerd help")
}

func usage() error {
	_, err := fmt.Fprintln(os.Stdout, `routerd2
  routerd boot --file /etc/routerd/boot.json
  routerd run --platform host|container|appliance [--config ... --module-path ...]
  routerd status [--runtime-dir /run/routerd]
  routerd network show [--runtime-dir /run/routerd]
  routerd modules restart NAME [--runtime-dir /run/routerd]
  routerd config show [--runtime-dir /run/routerd]
  routerd config validate FILE
  routerd config apply --file FILE --expected-revision N [--request-id ID] [--runtime-dir ...]
  routerd version

Platform selection is explicit. Use host mode only for isolated development.
The boot configuration enables SSH; no shell or system service is launched.`)
	return err
}

func serve(b runtimeconfig.Boot) error {
	applianceRuntime = b.Platform == "appliance"
	if err := b.Validate(); err != nil {
		return err
	}
	if err := boot.Bootstrap(boot.Options{Platform: b.Platform, RuntimeDir: b.RuntimeDir, StateDir: b.StateDir}); err != nil {
		return err
	}
	if err := kmod.Load(b.KernelModules); err != nil {
		return err
	}
	managementConfig := filepath.Join(b.RuntimeDir, "management.json")
	if err := config.AtomicJSON(managementConfig, b.SSH); err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	err := supervisor.Run(ctx, supervisor.Options{
		ModulePath: b.ModulePath, ReleaseID: buildinfo.ReleaseID, RuntimeDir: b.RuntimeDir, StateDir: b.StateDir, ConfigPath: b.Config, Platform: b.Platform,
		Modules: []supervisor.Spec{
			{Name: "network", Role: "network", Required: true},
			{Name: "control", Role: "control", Required: true},
			{Name: "management", Role: "management", Required: true, Args: []string{"--management-config=" + managementConfig}},
		},
	})
	if b.Platform == "appliance" {
		if err != nil {
			slog.Error("appliance runtime failed", "error", err)
		}
		halt(err != nil)
	}
	return err
}

func halt(restart bool) {
	unix.Sync()
	cmd := unix.LINUX_REBOOT_CMD_POWER_OFF
	if restart {
		cmd = unix.LINUX_REBOOT_CMD_RESTART
	}
	if err := unix.Reboot(cmd); err != nil {
		slog.Error("reboot syscall failed; preserving PID 1", "error", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func remote(args []string, command string) error {
	f := flags(command)
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	return (cli.Session{RuntimeDir: *dir}).Execute(context.Background(), command, nil, os.Stdout)
}

func configure(args []string) error {
	if len(args) == 0 {
		return errors.New("config requires show, validate, or apply")
	}
	if args[0] == "show" {
		return remote(args[1:], "show config")
	}
	if args[0] == "export" {
		return remote(args[1:], "show running-config")
	}
	if args[0] == "confirm" {
		return remote(args[1:], "confirm")
	}
	if args[0] == "validate" {
		if len(args) != 2 {
			return errors.New("usage: routerd config validate FILE")
		}
		if _, err := config.Read(args[1]); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout, "configuration is valid")
		return err
	}
	if args[0] != "apply" {
		return errors.New("unknown config command")
	}
	f := flags("config apply")
	path := f.String("file", "", "configuration file")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	revision := f.Uint64("expected-revision", 0, "current committed revision (required)")
	id := f.String("request-id", "", "idempotency key (generated if omitted)")
	confirmed := f.Int("confirmed", 0, "automatically roll back unless confirmed within this many seconds (5..3600)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *path == "" || *revision == 0 {
		return errors.New("--file and --expected-revision are required")
	}
	d, err := config.Read(*path)
	if err != nil {
		return err
	}
	if *id == "" {
		var b [16]byte
		if _, err = io.ReadFull(rand.Reader, b[:]); err != nil {
			return err
		}
		*id = hex.EncodeToString(b[:])
	}
	r := control.CommitRequest{ExpectedRevision: *revision, RequestID: *id, Config: d, ConfirmTimeoutSeconds: *confirmed}
	var s control.State
	c := localapi.NewClient(filepath.Join(*dir, control.SocketName))
	if err = c.Do(context.Background(), "POST", "/v1/config/commit", r, &s); err != nil {
		return fmt.Errorf("request_id=%s: %w", *id, err)
	}
	return cli.WriteJSON(os.Stdout, s)
}
