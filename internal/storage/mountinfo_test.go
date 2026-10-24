package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMountInfoEscapesAndFutureFields(t *testing.T) {
	data := []byte("1 9 0:1 / / rw - rootfs rootfs rw\n" + `2 1 252:0 / /var/lib/router\040state\134x rw,nosuid,nodev,relatime future:ok - ext4 /dev/disk\040name rw,errors=remount-ro` + "\n")
	entries, err := parseMountInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].target != `/var/lib/router state\x` || entries[1].source != "/dev/disk name" || entries[1].optional[0] != "future:ok" {
		t.Fatalf("unexpected decode: %#v", entries)
	}
	for _, tc := range []struct{ in, want string }{{`a\011b`, "a\tb"}, {`a\012b`, "a\nb"}, {`a\134040`, `a\040`}} {
		got, err := unescapeMount(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("unescape(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestMountInfoRejectsMalformedAndBoundedInput(t *testing.T) {
	good := "2 1 252:0 / /state rw,nosuid,nodev - ext4 /dev/vda rw\n"
	cases := map[string]string{
		"empty": "", "truncated": "2 1 252:0 / /state rw - ext4\n",
		"zero ID":                strings.Replace(good, "2 1", "0 1", 1),
		"negative ID":            strings.Replace(good, "2 1", "-2 1", 1),
		"overflow device":        strings.Replace(good, "252:0", "4294967296:0", 1),
		"signed device":          strings.Replace(good, "252:0", "+252:0", 1),
		"extra device separator": strings.Replace(good, "252:0", "252:0:0", 1),
		"relative target":        strings.Replace(good, "/state", "state", 1),
		"unclean target":         strings.Replace(good, "/state", "/a/../state", 1),
		"malformed escape":       strings.Replace(good, "/state", `/sta\777te`, 1),
		"short escape":           strings.Replace(good, "/state", `/sta\04`, 1),
		"empty option":           strings.Replace(good, "rw,nosuid", "rw,,nosuid", 1),
		"duplicate option":       strings.Replace(good, "rw,nosuid", "rw,rw,nosuid", 1),
		"bad shared":             strings.Replace(good, " - ", " shared:0 - ", 1),
		"bad master":             strings.Replace(good, " - ", " master:no - ", 1),
		"bad unbindable":         strings.Replace(good, " - ", " unbindable:1 - ", 1),
		"extra field":            strings.TrimSuffix(good, "\n") + " surprise\n",
		"duplicate ID":           good + good,
		"oversize":               strings.Repeat("x", maxMountInfoBytes+1),
		"oversize line":          strings.Repeat("x", maxMountLine+1) + "\n",
	}
	var many strings.Builder
	for i := 1; i <= maxMounts+1; i++ {
		fmt.Fprintf(&many, "%d 99999 0:1 / /%d rw - tmpfs tmpfs rw\n", i, i)
	}
	cases["too many mounts"] = many.String()
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := parseMountInfo([]byte(value)); err == nil {
				t.Fatalf("accepted malformed mount table: %#v", got)
			}
		})
	}
}

func rootMount() mountInfo {
	return mountInfo{id: 1, parent: 9, major: 0, minor: 1, root: "/", target: "/", filesystem: "rootfs", source: "rootfs", options: []string{"rw"}, superOptions: []string{"rw"}}
}

func stateMount() mountInfo {
	return mountInfo{id: 2, parent: 1, major: 252, minor: 0, root: "/", target: "/var/lib/routerd", filesystem: "ext4", source: "/proc/self/fd/11", options: []string{"rw", "nosuid", "nodev", "relatime"}, superOptions: []string{"rw", "errors=remount-ro", "data=ordered"}}
}

func TestSelectMountUsesDeviceIdentityNotSourceString(t *testing.T) {
	m := stateMount()
	m.source = "/an/old/device-name"
	selected, err := selectMount([]mountInfo{rootMount(), m}, m.target, 252, 0)
	if err != nil || selected.exact == nil || selected.exact.id != m.id {
		t.Fatalf("matching st_dev rejected: %#v, %v", selected, err)
	}
	selected, err = selectMount([]mountInfo{rootMount()}, m.target, 252, 0)
	if err != nil || selected.exact != nil || selected.cover.id != 1 {
		t.Fatalf("unmounted target selection: %#v, %v", selected, err)
	}
}

func TestSelectMountRejectsForeignStackedAndUnsafeMounts(t *testing.T) {
	tests := map[string]func(*mountInfo){
		"different device":       func(m *mountInfo) { m.minor++ },
		"different fs":           func(m *mountInfo) { m.filesystem = "xfs" },
		"subdirectory bind":      func(m *mountInfo) { m.root = "/elsewhere" },
		"readonly":               func(m *mountInfo) { m.options = []string{"ro", "nosuid", "nodev"} },
		"contradictory readonly": func(m *mountInfo) { m.options = append(m.options, "ro") },
		"suid":                   func(m *mountInfo) { m.options = []string{"rw", "nodev"} },
		"devices":                func(m *mountInfo) { m.options = []string{"rw", "nosuid"} },
		"noexec":                 func(m *mountInfo) { m.options = append(m.options, "noexec") },
		"readonly super":         func(m *mountInfo) { m.superOptions = []string{"ro"} },
		"writeback":              func(m *mountInfo) { m.superOptions = append(m.superOptions, "data=writeback") },
		"no journal replay":      func(m *mountInfo) { m.superOptions = append(m.superOptions, "norecovery") },
		"no barriers":            func(m *mountInfo) { m.superOptions = append(m.superOptions, "barrier=0") },
		"continue on error":      func(m *mountInfo) { m.superOptions = append(m.superOptions, "errors=continue") },
		"shared":                 func(m *mountInfo) { m.optional = []string{"shared:4"} },
		"slave":                  func(m *mountInfo) { m.optional = []string{"master:3"} },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			m := stateMount()
			change(&m)
			if _, err := selectMount([]mountInfo{rootMount(), m}, "/var/lib/routerd", 252, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict, got %v", err)
			}
		})
	}
	for _, name := range []string{"stacked target", "stacked ancestor", "descendant", "same device elsewhere", "shared covering mount"} {
		t.Run(name, func(t *testing.T) {
			root, m := rootMount(), stateMount()
			entries := []mountInfo{root}
			switch name {
			case "stacked target":
				entries = append(entries, m)
				m.id = 3
			case "stacked ancestor":
				m = root
				m.id = 3
			case "descendant":
				m.target += "/log"
				m.major, m.minor = 0, 3
			case "same device elsewhere":
				m.target = "/backup"
			case "shared covering mount":
				entries[0].optional = []string{"shared:5"}
				m.target = "/unrelated"
				m.major = 0
			}
			entries = append(entries, m)
			if _, err := selectMount(entries, "/var/lib/routerd", 252, 0); !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict, got %v", err)
			}
		})
	}
}
