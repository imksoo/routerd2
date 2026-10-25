// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// This dedicated no-NIC guest is a destructive power-cut fixture, never a
// production initializer. Every operation requires its explicit kernel flag.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/storage"
	"golang.org/x/sys/unix"
)

const (
	marker          = "ROUTERD_HEALTH_POWER"
	guestFlag       = "routerd2_health_power_fixture=1"
	identity        = "health-power-gate"
	stateDir        = "/var/lib/routerd"
	runtimeDir      = "/run/routerd"
	devicePath      = "/dev/vda"
	seedPath        = stateDir + "/bootstrap.json"
	bootPath        = "/etc/routerd/boot.json"
	expectedPath    = "/test/expected.json"
	targetTrigger   = `CREATE TRIGGER fail_health_power_target BEFORE INSERT ON events WHEN json_extract(CAST(NEW.body AS TEXT),'$.topic') LIKE 'routerd.healthcheck.wan.%' BEGIN SELECT RAISE(ABORT,'health power target fault'); END`
	registryTrigger = `CREATE TRIGGER fail_health_power_registry BEFORE UPDATE ON producers WHEN NEW.name='control-health-relays' BEGIN SELECT RAISE(ABORT,'health power registry ACK fault'); END`
)

type observation struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Cursor uint64 `json:"cursor"`
}

type witness struct {
	Phase                string                 `json:"phase"`
	BootID               string                 `json:"boot_id"`
	OriginBootID         string                 `json:"origin_boot_id"`
	PID                  int                    `json:"pid"`
	ExecVerified         bool                   `json:"exec_verified"`
	Filesystem           string                 `json:"filesystem"`
	Revision             uint64                 `json:"revision"`
	PendingCount         int                    `json:"pending_count"`
	Registered           int                    `json:"registered"`
	Retiring             int                    `json:"retiring"`
	HealthWorker         bool                   `json:"health_worker"`
	HealthError          bool                   `json:"health_error"`
	OriginalSupervisor   string                 `json:"original_supervisor"`
	OriginalEpoch        string                 `json:"original_epoch"`
	OriginalGeneration   uint64                 `json:"original_generation"`
	OriginalDigest       string                 `json:"original_digest"`
	Observations         []observation          `json:"observations"`
	Completed            []completedObservation `json:"completed"`
	ArchivedObservations int                    `json:"archived_observations"`
}

func main() {
	probe := len(os.Args) == 2 && os.Args[1] == "probe"
	if (!probe && (len(os.Args) != 1 || os.Getpid() != 1)) || (probe && os.Getpid() <= 1) {
		fail(errors.New("requires dedicated PID 1 or its independent probe"))
	}
	if !probe {
		if _, err := os.Stat("/proc/cmdline"); errors.Is(err, os.ErrNotExist) {
			if err = unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
				fail(err)
			}
		}
	}
	phase, err := phaseFromKernel()
	if err != nil {
		fail(err)
	}
	if probe {
		err = runProbe(phase)
	} else {
		err = initializeAndExec(phase)
	}
	if err != nil {
		fail(err)
	}
	fail(errors.New("unexpected return from guest execution"))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, marker+"_FAIL:", err)
	if os.Getpid() != 1 {
		os.Exit(1)
	}
	for {
		time.Sleep(time.Second)
	}
}

func phaseFromKernel() (string, error) {
	if os.Geteuid() != 0 {
		return "", errors.New("guest root required")
	}
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", err
	}
	flag, phase := false, ""
	for _, field := range strings.Fields(string(data)) {
		if field == guestFlag {
			flag = true
		}
		if strings.HasPrefix(field, "routerd2_health_power_phase=") {
			if phase != "" {
				return "", errors.New("duplicate phase")
			}
			phase = strings.TrimPrefix(field, "routerd2_health_power_phase=")
		}
	}
	if !flag || phase != "produce" && phase != "registry" && phase != "recovered" {
		return "", errors.New("fixed power-cut guest flag and phase required")
	}
	return phase, nil
}

func initializeAndExec(phase string) error {
	if err := boot.Bootstrap(boot.Options{Platform: boot.Appliance, RuntimeDir: runtimeDir, StateDir: stateDir}); err != nil {
		return err
	}
	if err := requireNoNIC(); err != nil {
		return err
	}
	modules := []string{"virtio_pci", "virtio_blk", "ext4", "nf_tables", "nft_ct", "nft_chain_nat", "nft_masq", "ip6_tunnel"}
	if err := kmod.Load(modules); err != nil {
		return err
	}
	if phase == "produce" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		v, err := storage.Mount(ctx, storage.Options{Platform: "appliance", Device: devicePath, Target: stateDir, WaitTimeout: 10 * time.Second})
		if err != nil {
			return err
		}
		if !v.Mounted || v.AlreadyMounted {
			return errors.New("seed disk was not freshly mounted")
		}
		if err = seed(); err != nil {
			return err
		}
		// This unmount is before any worker accepts an observation. Later boots
		// never pre-mount/recover/clean the disk on the product's behalf.
		if err = unix.Unmount(stateDir, 0); err != nil {
			return err
		}
		if _, err = os.Stat(seedPath); !errors.Is(err, os.ErrNotExist) {
			return errors.New("seed remained visible after unmount")
		}
	}
	b := runtimeconfig.Default()
	b.Platform = "appliance"
	b.Config = seedPath
	b.Workers = runtimeconfig.NativeWorkers("/test/bin/routerd-modules")
	b.RuntimeDir = runtimeDir
	b.StateDir = stateDir
	b.KernelModules = modules
	b.StateVolume = &runtimeconfig.StateVolume{Device: devicePath, WaitTimeoutSeconds: 10}
	if err := b.Validate(); err != nil {
		return err
	}
	if err := config.AtomicJSON(bootPath, b); err != nil {
		return err
	}
	fmt.Printf("%s_SETUP phase=%s seed_mounted=false product_mount_required=true\n", marker, phase)
	p, err := os.StartProcess("/init", []string{"health-power-probe", "probe"}, &os.ProcAttr{Dir: "/", Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return err
	}
	if err = p.Release(); err != nil {
		return err
	}
	return unix.Exec("/test/bin/routerd", []string{"routerd", "boot", "--file", bootPath}, os.Environ())
}

func seed() error {
	if _, err := os.Lstat(seedPath); !errors.Is(err, os.ErrNotExist) {
		return errors.New("produce disk is not new")
	}
	spec := api.DerivedEventSpec{Topic: "routerd.fixture.power.current", Inputs: []api.ReadyWhenSpec{{Field: "${HealthCheck/wan.status.phase}", Equals: "Healthy"}}, EmitInitial: true}
	rule := api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "routerd.healthcheck.wan.passed", Threshold: 2}, Emit: api.EventRuleEmitSpec{Topic: "routerd.fixture.power.samples"}}
	source := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "loopback", Interface: &network.Interface{Name: "lo"}},
		{ID: "wan", Service: &serviceconfig.Service{Name: "wan", Health: &health.Config{Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: "tcp", Port: 18089, IntervalMillis: 1000, TimeoutMillis: 300, UnhealthyThreshold: 1}}},
		{ID: "wan-current", DerivedEvent: &spec}, {ID: "history-rule", EventRule: &rule},
	}}
	if err := config.AtomicJSON(seedPath, source); err != nil {
		return err
	}
	store, err := events.Open(events.Options{Directory: filepath.Join(stateDir, "events")})
	if err != nil {
		return err
	}
	db, err := openInjector()
	if err != nil {
		store.Close()
		return err
	}
	_, err = db.Exec(targetTrigger)
	return errors.Join(err, db.Close(), store.Close())
}

func openInjector() (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "events", "journal.sqlite3"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=2000`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func requireNoNIC() error {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "lo" {
			continue
		}
		if e.Name() == "ip6tnl0" {
			if b, err := os.ReadFile("/sys/class/net/ip6tnl0/type"); err == nil && strings.TrimSpace(string(b)) == fmt.Sprint(unix.ARPHRD_TUNNEL6) {
				if _, err = os.Stat("/sys/class/net/ip6tnl0/device"); errors.Is(err, os.ErrNotExist) {
					continue
				}
			}
		}
		return fmt.Errorf("unexpected guest NIC %s", e.Name())
	}
	return nil
}

func readExpected() (witness, error) {
	b, err := os.ReadFile(expectedPath)
	if err != nil {
		return witness{}, err
	}
	var w witness
	err = json.Unmarshal(b, &w)
	return w, err
}
