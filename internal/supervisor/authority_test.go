//go:build linux

package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/release"
)

func TestReleaseAuthorityIsCopiedValidatedAndReservedForControl(t *testing.T) {
	a := release.Authority{Root: "/state/releases", TrustFile: "/boot/trust.json", Digest: strings.Repeat("a", 64)}
	base := Options{Platform: "host", ReleaseID: "test", Executables: map[string]string{"control": "/test/control"}, Modules: []Spec{{Name: "control", Role: "control"}}, ReleaseAuthority: &a}
	got, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	a.Digest = "invalid"
	if got.ReleaseAuthority.Digest == a.Digest || got.authorityJSON != string(raw) {
		t.Fatal("authority was not frozen")
	}
	if _, err := normalize(base); err == nil {
		t.Fatal("invalid authority accepted")
	}
	t.Setenv(release.AuthorityEnv, "inherited-forgery")
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "signed"}[selected], func(t *testing.T) {
			s := startTestSupervisorCustomized(t, []Spec{{Name: "control", Role: "control"}, {Name: "network", Role: "network"}}, nil, false, func(o *Options) {
				o.Env = append(o.Env, release.AuthorityEnv+"=override-forgery")
				if selected {
					copy := *got.ReleaseAuthority
					o.ReleaseAuthority = &copy
				}
			})
			s.wait(t, func(st Status) bool { return st.Ready })
			for _, name := range []string{"control", "network"} {
				value, err := os.ReadFile(filepath.Join(s.options.StateDir, "authority."+name))
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if selected && name == "control" {
					want = string(raw)
				}
				if string(value) != want {
					t.Fatalf("%s authority %q, want %q", name, value, want)
				}
			}
		})
	}
}
