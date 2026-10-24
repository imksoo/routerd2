package runtimeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func persistentBoot() Boot {
	b := Default()
	b.Platform = "appliance"
	b.StateVolume = &StateVolume{Device: "/dev/vda"}
	return b
}

func TestStateVolumeValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Boot){
		"default wait":    func(*Boot) {},
		"minimum wait":    func(b *Boot) { b.StateVolume.WaitTimeoutSeconds = 1 },
		"maximum wait":    func(b *Boot) { b.StateVolume.WaitTimeoutSeconds = 300 },
		"NVMe partition":  func(b *Boot) { b.StateVolume.Device = "/dev/nvme0n1p2" },
		"sibling prefix":  func(b *Boot) { b.StateDir = b.RuntimeDir + "-state" },
		"reversed prefix": func(b *Boot) { b.RuntimeDir = b.StateDir + "-runtime" },
		"persistent release": func(b *Boot) {
			b.Release = &Release{Root: b.StateDir + "/releases", TrustFile: b.StateDir + "/keys.json"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := persistentBoot()
			mutate(&b)
			if err := b.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, mutate := range map[string]func(*Boot){
		"implicit platform":      func(b *Boot) { b.Platform = "" },
		"container":              func(b *Boot) { b.Platform = "container" },
		"host":                   func(b *Boot) { b.Platform = "host" },
		"missing device":         func(b *Boot) { b.StateVolume.Device = "" },
		"relative device":        func(b *Boot) { b.StateVolume.Device = "vda" },
		"device directory":       func(b *Boot) { b.StateVolume.Device = "/dev" },
		"device outside dev":     func(b *Boot) { b.StateVolume.Device = "/data/disk" },
		"unclean device":         func(b *Boot) { b.StateVolume.Device = "/dev/../dev/vda" },
		"negative wait":          func(b *Boot) { b.StateVolume.WaitTimeoutSeconds = -1 },
		"excess wait":            func(b *Boot) { b.StateVolume.WaitTimeoutSeconds = 301 },
		"overflow wait":          func(b *Boot) { b.StateVolume.WaitTimeoutSeconds = int(^uint(0) >> 1) },
		"relative state":         func(b *Boot) { b.StateDir = "state" },
		"root state":             func(b *Boot) { b.StateDir = "/" },
		"unclean state":          func(b *Boot) { b.StateDir = "/var/lib/../lib/routerd" },
		"state trailing slash":   func(b *Boot) { b.StateDir += "/" },
		"proc state":             func(b *Boot) { b.StateDir = "/proc" },
		"proc descendant":        func(b *Boot) { b.StateDir = "/proc/state" },
		"sys state":              func(b *Boot) { b.StateDir = "/sys" },
		"sys descendant":         func(b *Boot) { b.StateDir = "/sys/state" },
		"dev state":              func(b *Boot) { b.StateDir = "/dev" },
		"dev descendant":         func(b *Boot) { b.StateDir = "/dev/state" },
		"state control char":     func(b *Boot) { b.StateDir = "/var/lib/routerd\n" },
		"runtime root":           func(b *Boot) { b.RuntimeDir = "/" },
		"runtime relative":       func(b *Boot) { b.RuntimeDir = "run" },
		"runtime unclean":        func(b *Boot) { b.RuntimeDir = "/run/./routerd" },
		"runtime trailing slash": func(b *Boot) { b.RuntimeDir += "/" },
		"equal directories":      func(b *Boot) { b.RuntimeDir = b.StateDir },
		"runtime inside state":   func(b *Boot) { b.RuntimeDir = b.StateDir + "/runtime" },
		"state inside runtime":   func(b *Boot) { b.StateDir = b.RuntimeDir + "/state" },
	} {
		t.Run(name, func(t *testing.T) {
			b := persistentBoot()
			mutate(&b)
			if err := b.Validate(); err == nil {
				t.Fatal("accepted invalid state volume configuration")
			}
		})
	}
}

func TestStateVolumeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot.json")
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	want := persistentBoot()
	want.StateVolume.WaitTimeoutSeconds = 12
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	write(data)
	got, err := Read(path)
	if err != nil || got.StateVolume == nil || *got.StateVolume != *want.StateVolume || got.StateDir != want.StateDir {
		t.Fatalf("persistent boot round trip: %#v, %v", got, err)
	}
	for _, invalid := range []string{
		`{"platform":"appliance","state_volume":{"device":"/dev/vda","format":true}}`,
		`{"platform":"appliance","state_volume":{"device":"/dev/vda","target":"/elsewhere"}}`,
		`{"platform":"appliance","state_volume":{"device":"/dev/vda","wait_timeout_seconds":0.5}}`,
		`{"platform":"appliance","state_volume":{}}`,
		`{"platform":"container","state_volume":{"device":"/dev/vda"}}`,
	} {
		write([]byte(invalid))
		if _, err := Read(path); err == nil {
			t.Fatalf("accepted unsupported state volume JSON: %s", invalid)
		}
	}
}

func TestStateVolumeRemainsOptIn(t *testing.T) {
	for _, platform := range []string{"appliance", "host", "container"} {
		b := Default()
		b.Platform = platform
		if b.StateVolume != nil {
			t.Fatal("default boot configuration mounts a data device")
		}
		if err := b.Validate(); err != nil {
			t.Fatalf("existing %s configuration without volume: %v", platform, err)
		}
	}
}
