//go:build linux

// Package kmod loads explicitly selected kernel modules and their dependencies
// without invoking modprobe. Images must contain uncompressed modules.dep entries.
package kmod

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type Index struct {
	Root         string
	Files        map[string]string
	Dependencies map[string][]string
	Builtins     map[string]bool
}

func ModuleName(path string) string {
	b := filepath.Base(path)
	for _, suffix := range []string{".zst", ".xz", ".gz"} {
		b = strings.TrimSuffix(b, suffix)
	}
	return strings.ReplaceAll(strings.TrimSuffix(b, ".ko"), "-", "_")
}

func safeRelative(p string) bool {
	return p != "" && !filepath.IsAbs(p) && filepath.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

func ReadIndex(root string) (*Index, error) {
	i := &Index{Root: root, Files: map[string]string{}, Dependencies: map[string][]string{}, Builtins: map[string]bool{}}
	f, err := os.Open(filepath.Join(root, "modules.dep"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		path, deps, ok := strings.Cut(s.Text(), ":")
		if !ok || !safeRelative(path) {
			return nil, errors.New("invalid modules.dep path")
		}
		if !strings.HasSuffix(path, ".ko") {
			return nil, fmt.Errorf("module %s is compressed: decompress modules at image build time", path)
		}
		n := ModuleName(path)
		if old, ok := i.Files[n]; ok && old != path {
			return nil, fmt.Errorf("ambiguous kernel module %s", n)
		}
		i.Files[n] = path
		for _, dep := range strings.Fields(deps) {
			if !safeRelative(dep) {
				return nil, errors.New("invalid module dependency")
			}
			i.Dependencies[n] = append(i.Dependencies[n], ModuleName(dep))
		}
	}
	if err = s.Err(); err != nil {
		return nil, err
	}
	fb, err := os.Open(filepath.Join(root, "modules.builtin"))
	if err == nil {
		defer fb.Close()
		b := bufio.NewScanner(fb)
		for b.Scan() {
			i.Builtins[ModuleName(b.Text())] = true
		}
		if err = b.Err(); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return i, nil
}

func (i *Index) Order(names []string) ([]string, error) {
	state := map[string]int{}
	var result []string
	var visit func(string) error
	visit = func(n string) error {
		n = strings.ReplaceAll(n, "-", "_")
		if i.Builtins[n] {
			return nil
		}
		if state[n] == 1 {
			return fmt.Errorf("kernel module dependency cycle at %s", n)
		}
		if state[n] == 2 {
			return nil
		}
		if _, ok := i.Files[n]; !ok {
			return fmt.Errorf("kernel module %s absent from image", n)
		}
		state[n] = 1
		for _, dep := range i.Dependencies[n] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[n] = 2
		result = append(result, i.Files[n])
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func Load(names []string) error {
	if len(names) == 0 {
		return nil
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return err
	}
	release := unix.ByteSliceToString(u.Release[:])
	i, err := ReadIndex(filepath.Join("/lib/modules", release))
	if err != nil {
		return err
	}
	order, err := i.Order(names)
	if err != nil {
		return err
	}
	for _, path := range order {
		if _, err := os.Stat(filepath.Join("/sys/module", ModuleName(path))); err == nil {
			continue
		}
		f, err := os.Open(filepath.Join(i.Root, path))
		if err != nil {
			return err
		}
		err = unix.FinitModule(int(f.Fd()), "", 0)
		_ = f.Close()
		if err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("load %s: %w", path, err)
		}
	}
	return nil
}
