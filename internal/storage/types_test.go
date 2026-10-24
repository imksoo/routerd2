package storage

import (
	"strings"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{Platform: "appliance", Device: "/dev/vda", Target: "/var/lib/routerd", WaitTimeout: 250 * time.Millisecond}
}

func TestOptionsValidate(t *testing.T) {
	for _, change := range []func(*Options){func(*Options) {}, func(o *Options) { o.WaitTimeout = 0 }, func(o *Options) { o.WaitTimeout = MaxWaitTimeout }, func(o *Options) { o.Target = "/srv/router state" }} {
		o := testOptions()
		change(&o)
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(*Options){
		"container":          func(o *Options) { o.Platform = "container" },
		"host":               func(o *Options) { o.Platform = "host" },
		"implicit platform":  func(o *Options) { o.Platform = "" },
		"relative device":    func(o *Options) { o.Device = "vda" },
		"device outside dev": func(o *Options) { o.Device = "/var/disk" },
		"dev directory":      func(o *Options) { o.Device = "/dev" },
		"unclean device":     func(o *Options) { o.Device = "/dev/../dev/vda" },
		"relative target":    func(o *Options) { o.Target = "state" },
		"root target":        func(o *Options) { o.Target = "/" },
		"proc target":        func(o *Options) { o.Target = "/proc" },
		"proc descendant":    func(o *Options) { o.Target = "/proc/test" },
		"sys descendant":     func(o *Options) { o.Target = "/sys/test" },
		"dev descendant":     func(o *Options) { o.Target = "/dev/test" },
		"unclean target":     func(o *Options) { o.Target = "/state/" },
		"NUL target":         func(o *Options) { o.Target = "/state\x00" },
		"newline target":     func(o *Options) { o.Target = "/state\nother" },
		"long target":        func(o *Options) { o.Target = "/" + strings.Repeat("x", maxPathBytes) },
		"negative timeout":   func(o *Options) { o.WaitTimeout = -time.Second },
		"large timeout":      func(o *Options) { o.WaitTimeout = MaxWaitTimeout + 1 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			o := testOptions()
			change(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("accepted invalid options")
			}
		})
	}
}
