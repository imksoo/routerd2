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

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/releaseboot"
	"github.com/imksoo/routerd2/internal/releasecli"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
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
	case "release":
		return releasecli.Run(context.Background(), args[1:], os.Stdout, os.Stderr)
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
		return bootConfigured(*path, b)
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
	case "logs":
		return logs(args[1:])
	case "cli":
		f := flags("cli")
		dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected cli arguments")
		}
		if term.IsTerminal(int(os.Stdin.Fd())) {
			previous, err := term.MakeRaw(int(os.Stdin.Fd()))
			if err != nil {
				return err
			}
			defer term.Restore(int(os.Stdin.Fd()), previous)
		}
		return (cli.Session{RuntimeDir: *dir}).Interactive(context.Background(), stdio{})
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
  routerd cli [--runtime-dir /run/routerd]
  routerd logs [--module NAME --level INFO --limit 100 --runtime-dir ...]
  routerd network show [--runtime-dir /run/routerd]
  routerd modules restart NAME [--runtime-dir /run/routerd]
  routerd config show [--runtime-dir /run/routerd]
  routerd config export [--runtime-dir /run/routerd]
  routerd config confirm [--runtime-dir /run/routerd]
  routerd config validate FILE
  routerd config migrate [--policy FILE] FILE
  routerd config apply --file FILE --expected-revision N [--request-id ID --confirmed SECONDS --runtime-dir ...]
  routerd release help
  routerd version

Platform selection is explicit. Use host mode only for isolated development.
The boot configuration enables SSH; no shell or system service is launched.`)
	return err
}

func serve(b runtimeconfig.Boot) error { return serveWithTrial(b, nil) }

func serveWithTrial(b runtimeconfig.Boot, trial *releaseboot.Trial) error {
	applianceRuntime = b.Platform == "appliance"
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := bootstrapRuntime(ctx, b); err != nil {
		return err
	}
	managementConfig := filepath.Join(b.RuntimeDir, "management.json")
	if err := config.AtomicJSON(managementConfig, b.SSH); err != nil {
		return err
	}
	logger, err := logging.NewHandler(logging.HandlerOptions{RuntimeDir: b.RuntimeDir, Module: "supervisor", Fallback: os.Stderr})
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(logger))
	closeLogger := func() {
		flushCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = logger.Close(flushCtx)
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}
	defer closeLogger()
	var trialDone chan error
	if trial != nil {
		trialDone = make(chan error, 1)
		go func() {
			trialErr := trial.Watch(ctx)
			trialDone <- trialErr
			if trialErr != nil {
				cancel()
			}
		}()
	}
	err = supervisor.Run(ctx, supervisor.Options{
		ModulePath: b.ModulePath, ReleaseID: buildinfo.ReleaseID, RuntimeDir: b.RuntimeDir, StateDir: b.StateDir, ConfigPath: b.Config, Platform: b.Platform,
		DynamicRoles: []string{"dhcp4", "dhcp6pd", "dhcp_server", "ra", "ra_client", "aftr", "dns", "vrrp", "bgp"},
		Modules: []supervisor.Spec{
			{Name: "logging", Role: "logging", Required: false},
			{Name: "network", Role: "network", Required: true},
			{Name: "control", Role: "control", Required: true, StartupTimeout: 60 * time.Second},
			{Name: "management", Role: "management", Required: true, Args: []string{"--management-config=" + managementConfig}},
		},
	})
	if trialDone != nil {
		cancel()
		err = errors.Join(err, <-trialDone)
		if err != nil {
			failure := trial.Fail(context.Background(), "selected userland failed during startup")
			if !errors.Is(failure, release.ErrStale) {
				err = errors.Join(err, failure)
			}
		}
	}
	if b.Platform == "appliance" {
		if err != nil {
			slog.Error("appliance runtime failed", "error", err)
		}
		closeLogger()
		halt(err != nil)
	}
	return err
}

type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

func logs(args []string) error {
	f := flags("logs")
	dir := f.String("runtime-dir", "/run/routerd", "runtime directory")
	module := f.String("module", "", "filter module")
	level := f.String("level", "INFO", "minimum level")
	limit := f.Int("limit", 100, "maximum records (1..200)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected logs arguments")
	}
	command := fmt.Sprintf("show logs level %s limit %d", *level, *limit)
	if *module != "" {
		command += " module " + *module
	}
	return (cli.Session{RuntimeDir: *dir}).Execute(context.Background(), command, nil, os.Stdout)
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
		return errors.New("config requires show, export, validate, migrate, apply, or confirm")
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
	if args[0] == "migrate" {
		return migrate(args[1:], os.Stdout, os.Stderr)
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
