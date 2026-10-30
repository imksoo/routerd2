// SPDX-License-Identifier: BSD-3-Clause

package release

import (
	"bytes"
	gobuildinfo "debug/buildinfo"
	"debug/elf"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/runtimeabi"
)

// InspectELF reads an executable without running it. The accepted build profile
// deliberately matches the project's static Go -buildmode=exe output; a future
// static PIE or a different CPU baseline requires an explicit format extension.
func InspectELF(r io.ReaderAt, size int64, artifact, releaseID, targetOS, targetArch string) error {
	_, err := InspectELFABI(r, size, artifact, releaseID, targetOS, targetArch)
	return err
}

// InspectELFABI additionally reads the executable's complete ABI declaration
// from the verified readonly section, without running incoming code.
func InspectELFABI(r io.ReaderAt, size int64, artifact, releaseID, targetOS, targetArch string) (runtimeabi.Descriptor, error) {
	if err := inspectELF(r, size, artifact, releaseID, targetOS, targetArch); err != nil {
		return runtimeabi.Descriptor{}, err
	}
	f, err := elf.NewFile(r)
	if err != nil {
		return runtimeabi.Descriptor{}, err
	}
	defer f.Close()
	section := f.Section(".rodata")
	if section == nil {
		return runtimeabi.Descriptor{}, errors.New("ELF has no readonly ABI declaration section")
	}
	return runtimeabi.ReadRecord(io.NewSectionReader(r, int64(section.Offset), int64(section.Size)))
}

func inspectELF(r io.ReaderAt, size int64, artifact, releaseID, targetOS, targetArch string) error {
	if !componentName.MatchString(artifact) || size < 64 || size > MaxArtifactBytes {
		return errors.New("invalid ELF artifact or size")
	}
	var header [64]byte
	if _, err := r.ReadAt(header[:], 0); err != nil {
		return err
	}
	if string(header[:4]) != "\x7fELF" || header[4] != byte(elf.ELFCLASS64) || header[5] != byte(elf.ELFDATA2LSB) || header[6] != 1 {
		return errors.New("artifact must be a little-endian ELF64 executable")
	}
	// Reject extended or excessive header tables before debug/elf allocates.
	phoff, shoff := binary.LittleEndian.Uint64(header[32:40]), binary.LittleEndian.Uint64(header[40:48])
	phsize, phnum := binary.LittleEndian.Uint16(header[54:56]), binary.LittleEndian.Uint16(header[56:58])
	shsize, shnum := binary.LittleEndian.Uint16(header[58:60]), binary.LittleEndian.Uint16(header[60:62])
	shstr := binary.LittleEndian.Uint16(header[62:64])
	if binary.LittleEndian.Uint16(header[52:54]) != 64 || phsize != 56 || phnum == 0 || phnum > 64 || shsize != 64 || shnum == 0 || shnum > 4096 || shstr >= shnum {
		return errors.New("unsupported or excessive ELF header tables")
	}
	if phoff > uint64(size) || uint64(phnum)*56 > uint64(size)-phoff || shoff > uint64(size) || uint64(shnum)*64 > uint64(size)-shoff {
		return errors.New("ELF header table exceeds artifact size")
	}
	f, err := elf.NewFile(r)
	if err != nil {
		return fmt.Errorf("read ELF: %w", err)
	}
	defer f.Close()
	wantMachine := elf.EM_X86_64
	if targetArch == "arm64" {
		wantMachine = elf.EM_AARCH64
	}
	if targetOS != "linux" || targetArch != "amd64" && targetArch != "arm64" || f.Machine != wantMachine || f.Type != elf.ET_EXEC || f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX || f.ABIVersion != 0 {
		return errors.New("ELF architecture/type/ABI does not match the static Linux release")
	}
	entryOK := false
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP || p.Type == elf.PT_DYNAMIC {
			return errors.New("dynamic loader or dynamic ELF segment is forbidden")
		}
		if p.Off > uint64(size) || p.Filesz > uint64(size)-p.Off || p.Filesz > p.Memsz {
			return errors.New("ELF segment exceeds artifact bounds")
		}
		if p.Type == elf.PT_LOAD {
			if p.Flags&(elf.PF_W|elf.PF_X) == elf.PF_W|elf.PF_X {
				return errors.New("writable executable ELF segment is forbidden")
			}
			if p.Flags&elf.PF_X != 0 && f.Entry >= p.Vaddr && f.Entry-p.Vaddr < p.Filesz {
				entryOK = true
			}
		}
	}
	if !entryOK {
		return errors.New("ELF entry is not in an executable load segment")
	}
	var rodata *elf.Section
	for _, s := range f.Sections {
		if s.Type != elf.SHT_NOBITS && (s.Offset > uint64(size) || s.Size > uint64(size)-s.Offset) {
			return errors.New("ELF section exceeds artifact bounds")
		}
		if s.Type == elf.SHT_DYNAMIC || s.Type == elf.SHT_DYNSYM {
			return errors.New("dynamic ELF sections are forbidden")
		}
		if s.Name == ".rodata" {
			if rodata != nil || s.Type != elf.SHT_PROGBITS || s.Flags&elf.SHF_ALLOC == 0 || s.Flags&(elf.SHF_WRITE|elf.SHF_COMPRESSED) != 0 {
				return errors.New("invalid or ambiguous ELF release identity section")
			}
			rodata = s
		}
	}
	if rodata == nil {
		return errors.New("ELF has no .rodata identity section")
	}
	loaded := false
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Flags&elf.PF_R != 0 && p.Flags&elf.PF_W == 0 && rodata.Offset >= p.Off {
			delta := rodata.Offset - p.Off
			if delta <= p.Filesz && rodata.Size <= p.Filesz-delta && rodata.Addr >= p.Vaddr && rodata.Addr-p.Vaddr == delta {
				loaded = true
			}
		}
	}
	if !loaded {
		return errors.New("ELF identity section is not covered by a read-only load segment")
	}
	identity, count, err := readIdentity(io.NewSectionReader(r, int64(rodata.Offset), int64(rodata.Size)))
	if err != nil {
		return err
	}
	if count != 1 || identity != releaseID {
		return fmt.Errorf("ELF release identity mismatch: found %d complete identity records, expected release %q", count, releaseID)
	}
	info, err := gobuildinfo.Read(r)
	if err != nil {
		return fmt.Errorf("read Go build identity: %w", err)
	}
	if info.Path != "github.com/imksoo/routerd2/cmd/"+artifact || info.Main.Path != "github.com/imksoo/routerd2" {
		return errors.New("ELF Go command/module identity differs from the named artifact")
	}
	settings := map[string]string{}
	for _, setting := range info.Settings {
		if _, exists := settings[setting.Key]; exists {
			return errors.New("duplicate Go build setting")
		}
		settings[setting.Key] = setting.Value
	}
	cgoOK := settings["CGO_ENABLED"] == "0" || (artifact == "routerd-modules" && settings["CGO_ENABLED"] == "1" && slices.Contains(strings.Split(settings["-tags"], ","), "libndpi"))
	if settings["GOOS"] != targetOS || settings["GOARCH"] != targetArch || !cgoOK || settings["-buildmode"] != "exe" {
		return errors.New("ELF Go build settings are not the static target profile")
	}
	if targetArch == "amd64" && settings["GOAMD64"] != "v1" {
		return errors.New("ELF requires a non-baseline amd64 CPU")
	}
	if targetArch == "arm64" && settings["GOARM64"] != "v8.0" {
		return errors.New("ELF requires a non-baseline arm64 CPU")
	}
	return nil
}

func readIdentity(r io.Reader) (string, int, error) {
	const block = 64 << 10
	maxRecord := len(buildinfo.IdentityPrefix) + 128 + len(buildinfo.IdentitySuffix)
	buffer := make([]byte, block+maxRecord)
	tail, total, count := 0, int64(0), 0
	last := int64(-1)
	var identity string
	for {
		n, err := r.Read(buffer[tail : tail+block])
		if n == 0 && err == nil {
			return "", 0, io.ErrNoProgress
		}
		b := buffer[:tail+n]
		base := total - int64(tail)
		for offset := 0; offset < len(b); {
			p := bytes.Index(b[offset:], []byte(buildinfo.IdentityPrefix))
			if p < 0 {
				break
			}
			p += offset
			endLimit := min(len(b), p+maxRecord)
			end := bytes.Index(b[p+len(buildinfo.IdentityPrefix):endLimit], []byte(buildinfo.IdentitySuffix))
			if end >= 0 {
				end += p + len(buildinfo.IdentityPrefix) + len(buildinfo.IdentitySuffix)
				if id, parseErr := buildinfo.ParseIdentity(string(b[p:end])); parseErr == nil && base+int64(p) > last {
					last = base + int64(p)
					count++
					identity = id
					if count > 1 {
						return identity, count, nil
					}
				}
			}
			offset = p + len(buildinfo.IdentityPrefix)
		}
		total += int64(n)
		if err != nil {
			if err == io.EOF {
				return identity, count, nil
			}
			return "", 0, err
		}
		tail = min(maxRecord-1, len(b))
		copy(buffer, b[len(b)-tail:])
	}
}
