//go:build linux

package supervisor

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRoleCatalogRequiresEveryInitialAndDynamicRoleAndCopiesCallerMap(t *testing.T) {
	base := Options{Platform: "host", ReleaseID: "test", Executables: map[string]string{"network": "/test/network", "health": "/test/protocols"}, Modules: []Spec{{Name: "network", Role: "network"}}, DynamicRoles: []string{"health"}}
	got, err := normalize(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Executables["network"] = "/changed"
	if got.Executables["network"] != "/test/network" {
		t.Fatal("caller can change normalized role binding")
	}
	for _, role := range []string{"network", "health"} {
		bad := got
		bad.Executables = map[string]string{}
		for k, v := range got.Executables {
			if k != role {
				bad.Executables[k] = v
			}
		}
		if _, err := normalize(bad); err == nil {
			t.Fatal("undeclared executable role accepted", role)
		}
	}
}

func TestRoleCatalogPinsDistinctInodesForIndependentWorkerRestarts(t *testing.T) {
	s := startTestSupervisorCatalog(t, []Spec{{Name: "network", Role: "ready"}, {Name: "control", Role: "ready2"}}, nil, true)
	first := s.wait(t, func(st Status) bool { return st.Ready })
	pinned := map[string]os.FileInfo{}
	for _, m := range first.Modules {
		info, err := os.Stat(s.options.Executables[m.Role])
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.Stat(filepath.Join("/proc", strconv.Itoa(m.PID), "exe"))
		if err != nil || !os.SameFile(info, actual) {
			t.Fatal("wrong initial role inode", m, err)
		}
		pinned[m.Role] = info
		if err := os.Remove(s.options.Executables[m.Role]); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/missing", s.options.Executables[m.Role]); err != nil {
			t.Fatal(err)
		}
	}
	if os.SameFile(pinned["ready"], pinned["ready2"]) {
		t.Fatal("fixture did not provide separate component inodes")
	}
	for _, name := range []string{"network", "control"} {
		if got := s.request(t, http.MethodPost, "/v1/modules/"+name+"/restart"); got != http.StatusAccepted {
			t.Fatal(got)
		}
	}
	final := s.wait(t, func(st Status) bool {
		if !st.Ready {
			return false
		}
		for _, m := range st.Modules {
			if m.Generation != 2 {
				return false
			}
		}
		return true
	})
	for _, m := range final.Modules {
		actual, err := os.Stat(filepath.Join("/proc", strconv.Itoa(m.PID), "exe"))
		if err != nil || !os.SameFile(pinned[m.Role], actual) {
			t.Fatal("restart resolved a changed or wrong role component", m, err)
		}
	}
}
