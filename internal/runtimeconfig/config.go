package runtimeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// Boot is intentionally separate from the transactional network configuration.
// Changing boot paths, platform, or SSH keys/listeners requires a system restart.
type Boot struct {
	Platform      string   `json:"platform"`
	Config        string   `json:"config"`
	ModulePath    string   `json:"module_path"`
	RuntimeDir    string   `json:"runtime_dir"`
	StateDir      string   `json:"state_dir"`
	KernelModules []string `json:"kernel_modules,omitempty"`
	SSH           SSH      `json:"ssh"`
}

type SSH struct {
	Listen         string   `json:"listen"`
	User           string   `json:"user"`
	AuthorizedKeys []string `json:"authorized_keys"`
}

func Default() Boot {
	return Boot{Config: "/etc/routerd/config.json", ModulePath: "/usr/libexec/routerd-modules", RuntimeDir: "/run/routerd", StateDir: "/var/lib/routerd", SSH: SSH{User: "admin"}}
}

func Read(path string) (Boot, error) {
	b := Default()
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
	switch b.Platform {
	case "appliance", "container", "host":
	default:
		return errors.New("platform must explicitly be appliance, container, or host")
	}
	for name, path := range map[string]string{"config": b.Config, "module_path": b.ModulePath, "runtime_dir": b.RuntimeDir, "state_dir": b.StateDir} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if b.RuntimeDir == b.StateDir {
		return errors.New("runtime_dir and state_dir must be separate")
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
