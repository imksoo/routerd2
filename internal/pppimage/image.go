// SPDX-License-Identifier: BSD-3-Clause

// Package pppimage describes the fixed pppd root filesystem shipped with PPPoE.
// It contains no protocol engine or process orchestration.
package pppimage

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const Command = "usr/sbin/pppd"
const Plugin = "usr/lib/pppd/2.5.3/pppoe.so"
const LegacyProvider = "usr/lib/ossl-modules/legacy.so"
const MaxBytes = 64 << 20

type entry struct {
	name string
	mode os.FileMode
	data []byte
}

func read(file *os.File) ([]entry, error) {
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() < 1024 || stat.Size() > MaxBytes {
		return nil, errors.New("PPP image must be a bounded regular tar archive")
	}
	r := tar.NewReader(io.NewSectionReader(file, 0, stat.Size()))
	var entries []entry
	seen := map[string]bool{}
	var total int64
	for {
		h, err := r.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		name := h.Name
		if h.Typeflag != tar.TypeReg || name == "" || name == "." || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || seen[name] || len(entries) >= 128 || h.Size < 0 || h.Size > 16<<20 || h.Size > MaxBytes-total {
			return nil, errors.New("invalid PPP image file, path, duplicate or size")
		}
		seen[name] = true
		e := entry{name: name, mode: os.FileMode(h.Mode & 0777)}
		e.data, err = io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		total += h.Size
		entries = append(entries, e)
	}
}

// Validate checks the actual ELF loader/library/plugin closure before a role
// is advertised. The archive is immutable and pinned by its owning catalog.
func Validate(file *os.File, arch string) error {
	entries, err := read(file)
	if err != nil {
		return err
	}
	files := make(map[string]entry, len(entries))
	for _, e := range entries {
		files[e.name] = e
	}
	machine := elf.EM_X86_64
	if arch == "arm64" {
		machine = elf.EM_AARCH64
	} else if arch != "amd64" {
		return errors.New("unsupported PPP image architecture")
	}
	pending := []string{Command, Plugin, LegacyProvider}
	checked := map[string]bool{}
	for len(pending) != 0 {
		name := pending[0]
		pending = pending[1:]
		if checked[name] {
			continue
		}
		checked[name] = true
		e, ok := files[name]
		if !ok {
			return fmt.Errorf("PPP image is missing %s", name)
		}
		f, err := elf.NewFile(bytes.NewReader(e.data))
		if err != nil {
			return fmt.Errorf("PPP ELF %s: %w", name, err)
		}
		if f.Class != elf.ELFCLASS64 || f.Machine != machine || f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
			return fmt.Errorf("PPP ELF %s has the wrong architecture or type", name)
		}
		if name == Command {
			if e.mode&0111 == 0 {
				return errors.New("PPP command is not executable")
			}
			loader := ""
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					b, err := io.ReadAll(io.LimitReader(p.Open(), 256))
					if err != nil {
						return err
					}
					loader = strings.TrimRight(string(b), "\x00")
				}
			}
			if !path.IsAbs(loader) {
				return errors.New("PPP command requires its bundled ELF interpreter")
			}
			pending = append(pending, strings.TrimPrefix(loader, "/"))
		}
		libraries, err := f.ImportedLibraries()
		if err != nil {
			return err
		}
		for _, library := range libraries {
			found := false
			for _, dir := range []string{"lib", "usr/local/lib", "usr/lib"} {
				name := path.Join(dir, library)
				if _, ok := files[name]; ok {
					pending = append(pending, name)
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("PPP image is missing library %s", library)
			}
		}
	}
	return nil
}

// Extract runs after the engine helper has entered an empty private chroot.
// It accepts only the regular-file image produced by the build path.
func Extract(file *os.File) error {
	entries, err := read(file)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.MkdirAll(path.Dir(e.name), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(e.name, e.data, e.mode); err != nil {
			return err
		}
	}
	return nil
}
