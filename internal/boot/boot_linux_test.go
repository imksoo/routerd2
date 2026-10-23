//go:build linux

package boot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformDoesNotInferApplianceFromPID(t *testing.T) {
	for _, platform := range []string{Host, Container} {
		t.Run(platform, func(t *testing.T) {
			mounted, directories := false, false
			err := bootstrap(Options{Platform: platform}, 1, func() error { mounted = true; return nil }, func(Options) error { directories = true; return nil })
			if err != nil || mounted || !directories {
				t.Fatalf("err=%v mounted=%v directories=%v", err, mounted, directories)
			}
		})
	}
}

func TestApplianceRejectsNonPID1BeforeSideEffects(t *testing.T) {
	called := false
	err := bootstrap(Options{Platform: Appliance}, 42, func() error { called = true; return nil }, func(Options) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("err=%v side effects=%v", err, called)
	}
	if err := bootstrap(Options{}, 1, func() error { called = true; return nil }, func(Options) error { called = true; return nil }); err == nil {
		t.Fatal("implicit platform accepted")
	}
}

func TestBootstrapHostCreatesDirectoriesAndRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	o := Options{Platform: Host, RuntimeDir: filepath.Join(dir, "run"), StateDir: filepath.Join(dir, "state")}
	if err := Bootstrap(o); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{o.RuntimeDir: 0750, o.StateDir: 0700} {
		stat, err := os.Stat(path)
		if err != nil || stat.Mode().Perm() != mode {
			t.Fatalf("%s mode: %+v, %v", path, stat, err)
		}
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(o.RuntimeDir, link); err != nil {
		t.Fatal(err)
	}
	o.RuntimeDir = link
	if err := Bootstrap(o); err == nil {
		t.Fatal("runtime directory symlink accepted")
	}
}
