package runtimeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/imksoo/routerd2/internal/configref"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/storage"
)

// Boot is intentionally separate from the transactional network configuration.
// Changing boot paths, platform, or SSH keys/listeners requires a system restart.
type Boot struct {
	Startup       *configref.Selection `json:"startup,omitempty"`
	Platform      string               `json:"platform"`
	Config        string               `json:"config"`
	Workers       map[string]string    `json:"workers"`
	RuntimeDir    string               `json:"runtime_dir"`
	StateDir      string               `json:"state_dir"`
	KernelModules []string             `json:"kernel_modules,omitempty"`
	SSH           SSH                  `json:"ssh"`
	Release       *Release             `json:"release,omitempty"`
	StateVolume   *StateVolume         `json:"state_volume,omitempty"`
}

// StateVolume mounts an explicitly selected, existing ext4 device at StateDir.
// Only appliance PID 1 may mount it. The image must include and explicitly load
// the device and filesystem drivers through KernelModules. Zero wait uses the
// storage package's bounded default; no disk is discovered, formatted or repaired.
type StateVolume struct {
	Device             string `json:"device"`
	WaitTimeoutSeconds int    `json:"wait_timeout_seconds,omitempty"`
}

// Release selects signed userland pairs from a persistent store. StateVolume,
// when configured, is mounted before the selector opens this store or trust
// file. Kernel/rootfs selection and configuration schema migrations are not
// performed by this userland selector.
type Release struct {
	Root                string `json:"root"`
	TrustFile           string `json:"trust_file"`
	ReadyTimeoutSeconds int    `json:"ready_timeout_seconds,omitempty"`
	StableSeconds       int    `json:"stable_seconds,omitempty"`
}

type SSH struct {
	Listen         string   `json:"listen"`
	User           string   `json:"user"`
	AuthorizedKeys []string `json:"authorized_keys"`
}

func Default() Boot {
	return Boot{Config: "/etc/routerd/config.json", Workers: NativeWorkers("/usr/libexec/routerd-modules"), RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd", SSH: SSH{User: "admin"}}
}

func Read(path string) (Boot, error) {
	b := Default()
	b.Workers = nil // serialized boot input must supply its own catalog, not merge default roles
	f, err := os.Open(path)
	if err != nil {
		return b, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&b); err != nil {
		return b, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return b, errors.New("expected a single boot configuration")
	}
	return b, b.Validate()
}

func (b Boot) Validate() error {
	if b.Startup != nil {
		if err := b.Startup.Validate(); err != nil {
			return err
		}
		if b.Release != nil {
			return errors.New("signed release startup selection requires an atomic runtime/configuration boot transaction; standalone startup input is not accepted")
		}
	}
	if b.Release != nil {
		r := b.Release
		if b.Platform != "appliance" {
			return errors.New("boot release selection requires explicit appliance mode; containers pin their OCI image")
		}
		if !filepath.IsAbs(r.Root) || filepath.Clean(r.Root) == "/" || !filepath.IsAbs(r.TrustFile) {
			return errors.New("release root and trust_file must be absolute paths")
		}
		timeout, stable := r.ReadyTimeoutSeconds, r.StableSeconds
		if timeout == 0 {
			timeout = 120
		}
		if stable == 0 {
			stable = 10
		}
		if timeout < 5 || timeout > 900 || stable < 1 || stable > 300 || stable >= timeout {
			return errors.New("release readiness timeout must be 5..900 seconds and stability 1..300 seconds below the timeout")
		}
	}
	switch b.Platform {
	case "appliance", "container", "host":
	default:
		return errors.New("platform must explicitly be appliance, container, or host")
	}
	for name, path := range map[string]string{"config": b.Config, "runtime_dir": b.RuntimeDir, "state_dir": b.StateDir} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if (b.Release == nil && len(b.Workers) == 0) || len(b.Workers) > 64 {
		return errors.New("boot requires an explicit bounded worker role catalog")
	}
	for role, path := range b.Workers {
		if role == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return errors.New("worker roles require clean absolute executable paths")
		}
	}
	if b.RuntimeDir == b.StateDir {
		return errors.New("runtime_dir and state_dir must be separate")
	}
	if b.StateVolume != nil {
		v := b.StateVolume
		// Bound the integer before converting it to time.Duration, so a very
		// large JSON number cannot wrap into an apparently valid timeout.
		if v.WaitTimeoutSeconds < 0 || v.WaitTimeoutSeconds > int(storage.MaxWaitTimeout/time.Second) {
			return errors.New("state_volume.wait_timeout_seconds must be zero (default) or 1..300")
		}
		opts := storage.Options{Platform: b.Platform, Device: v.Device, Target: b.StateDir, WaitTimeout: time.Duration(v.WaitTimeoutSeconds) * time.Second}
		if err := opts.Validate(); err != nil {
			return fmt.Errorf("state_volume: %w", err)
		}
		if b.RuntimeDir == "/" || filepath.Clean(b.RuntimeDir) != b.RuntimeDir {
			return errors.New("state_volume requires a clean, absolute, non-root runtime_dir")
		}
		if pathContains(b.StateDir, b.RuntimeDir) || pathContains(b.RuntimeDir, b.StateDir) {
			return errors.New("state_volume requires runtime_dir and state_dir to have no ancestor or descendant overlap")
		}
	}
	if b.Platform != "appliance" && len(b.KernelModules) > 0 {
		return errors.New("only appliance mode may load kernel modules")
	}
	if b.SSH.Listen != "" {
		if _, _, err := net.SplitHostPort(b.SSH.Listen); err != nil {
			return fmt.Errorf("ssh.listen: %w", err)
		}
	}
	if b.SSH.User == "" {
		return errors.New("ssh.user must not be empty")
	}
	return nil
}

func pathContains(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, parent+"/")
}

// NativeWorkers explicitly binds the current native multicall command to each
// supported role for factory/development boot configuration. Serialized input
// is a role catalog; there is no old module_path reader or implicit conversion.
func NativeWorkers(path string) map[string]string {
	out := map[string]string{}
	for _, role := range []string{"logging", "network", "control", "management", "health", "pppoe", "dhcp4", "dhcp6", "dhcp_server", "dhcp_relay", "ra", "ra_client", "aftr", "dns", "vrrp", "bgp", "bfd"} {
		if role == "logging" {
			out[role] = filepath.Join(filepath.Dir(path), "routerd-logging")
		} else {
			out[role] = path
		}
	}
	return out
}
