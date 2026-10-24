package storage

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	maxMountInfoBytes = 2 << 20
	maxMounts         = 4096
	maxMountLine      = 32 << 10
)

type mountInfo struct {
	id, parent         uint64
	major, minor       uint32
	root, target       string
	options, optional  []string
	filesystem, source string
	superOptions       []string
}

func parseMountInfo(data []byte) ([]mountInfo, error) {
	if len(data) == 0 || len(data) > maxMountInfoBytes {
		return nil, errors.New("mount table is empty or exceeds 2 MiB")
	}
	var entries []mountInfo
	seen := make(map[uint64]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxMountLine)
	for scanner.Scan() {
		if len(entries) == maxMounts {
			return nil, errors.New("mount table exceeds 4096 entries")
		}
		entry, err := parseMountLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("mount table line %d: %w", len(entries)+1, err)
		}
		if seen[entry.id] {
			return nil, errors.New("mount table has duplicate mount IDs")
		}
		seen[entry.id] = true
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read bounded mount table: %w", err)
	}
	return entries, nil
}

func parseMountLine(line string) (mountInfo, error) {
	var m mountInfo
	f := strings.Fields(line)
	separator := -1
	for i := 6; i < len(f); i++ {
		if f[i] == "-" {
			separator = i
			break
		}
	}
	if len(f) < 10 || separator < 6 || separator+4 != len(f) {
		return m, errors.New("invalid mountinfo field count")
	}
	var err error
	if m.id, err = parsePositive(f[0]); err != nil {
		return m, fmt.Errorf("mount ID: %w", err)
	}
	if m.parent, err = parsePositive(f[1]); err != nil {
		return m, fmt.Errorf("parent mount ID: %w", err)
	}
	major, minor, ok := strings.Cut(f[2], ":")
	if !ok {
		return m, errors.New("invalid device number")
	}
	maj, err := strconv.ParseUint(major, 10, 32)
	if err != nil || !decimal(major) {
		return m, errors.New("invalid device major")
	}
	min, err := strconv.ParseUint(minor, 10, 32)
	if err != nil || !decimal(minor) {
		return m, errors.New("invalid device minor")
	}
	m.major, m.minor = uint32(maj), uint32(min)
	if m.root, err = unescapeMount(f[3]); err != nil {
		return m, err
	}
	if m.target, err = unescapeMount(f[4]); err != nil {
		return m, err
	}
	for _, path := range []string{m.root, m.target} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > maxPathBytes {
			return m, errors.New("mount root and target must be canonical absolute paths")
		}
	}
	if m.options, err = parseOptions(f[5]); err != nil {
		return m, err
	}
	m.optional = append([]string(nil), f[6:separator]...)
	for _, option := range m.optional {
		key, value, hasValue := strings.Cut(option, ":")
		switch key {
		case "shared", "master", "propagate_from":
			if _, err := parsePositive(value); !hasValue || err != nil {
				return m, errors.New("invalid mount propagation ID")
			}
		case "unbindable":
			if hasValue {
				return m, errors.New("invalid unbindable mount field")
			}
		default: // The kernel ABI explicitly permits future optional fields.
		}
	}
	m.filesystem = f[separator+1]
	if m.source, err = unescapeMount(f[separator+2]); err != nil {
		return m, err
	}
	if m.superOptions, err = parseOptions(f[separator+3]); err != nil {
		return m, err
	}
	return m, nil
}

func decimal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parsePositive(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 || !decimal(s) {
		return 0, errors.New("expected a positive decimal integer")
	}
	return n, nil
}

func unescapeMount(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			if s[i] == 0 {
				return "", errors.New("NUL in mount table")
			}
			b.WriteByte(s[i])
			continue
		}
		if i+4 > len(s) {
			return "", errors.New("truncated mount escape")
		}
		switch s[i : i+4] {
		case `\040`:
			b.WriteByte(' ')
		case `\011`:
			b.WriteByte('\t')
		case `\012`:
			b.WriteByte('\n')
		case `\134`:
			b.WriteByte('\\')
		default:
			return "", errors.New("unknown mount escape")
		}
		i += 3
	}
	return b.String(), nil
}

func parseOptions(s string) ([]string, error) {
	options := strings.Split(s, ",")
	if len(options) > 256 {
		return nil, errors.New("too many mount options")
	}
	seen := make(map[string]bool)
	for _, option := range options {
		if option == "" || seen[option] || strings.IndexByte(option, 0) >= 0 {
			return nil, errors.New("invalid or duplicate mount option")
		}
		seen[option] = true
	}
	return options, nil
}

func has(options []string, value string) bool {
	for _, option := range options {
		if option == value {
			return true
		}
	}
	return false
}

type selection struct {
	exact *mountInfo
	cover mountInfo
}

// selectMount rejects ambiguous and conflicting ownership before any mount
// operation. Device identity comes from the block node's st_rdev, not a source
// pathname printed by mountinfo.
func selectMount(entries []mountInfo, target string, major, minor uint32) (selection, error) {
	var result selection
	coverTargets := make(map[string]bool)
	for _, m := range entries {
		if m.major == major && m.minor == minor && m.target != target {
			return result, fmt.Errorf("%w: selected device is already mounted at %s", ErrConflict, m.target)
		}
		if m.target != target && within(m.target, target) {
			return result, fmt.Errorf("%w: target contains mount %s", ErrConflict, m.target)
		}
		if !within(target, m.target) {
			continue
		}
		if coverTargets[m.target] {
			return result, fmt.Errorf("%w: stacked mounts at %s", ErrConflict, m.target)
		}
		coverTargets[m.target] = true
		if len(m.target) > len(result.cover.target) {
			result.cover = m
		}
		if m.target == target {
			copy := m
			result.exact = &copy
		}
	}
	if result.cover.id == 0 {
		return result, errors.New("mount table does not contain the target's covering mount")
	}
	for _, option := range result.cover.optional {
		key, _, _ := strings.Cut(option, ":")
		if key == "shared" || key == "master" || key == "propagate_from" {
			return result, fmt.Errorf("%w: state target must have private mount propagation", ErrConflict)
		}
	}
	if result.exact != nil {
		if err := validateMounted(*result.exact, major, minor); err != nil {
			return result, err
		}
	}
	return result, nil
}

func validateMounted(m mountInfo, major, minor uint32) error {
	if m.major != major || m.minor != minor || m.filesystem != "ext4" || m.root != "/" {
		return fmt.Errorf("%w: existing mount is not the selected whole ext4 filesystem", ErrConflict)
	}
	for _, required := range []string{"rw", "nosuid", "nodev"} {
		if !has(m.options, required) {
			return fmt.Errorf("%w: existing mount lacks %s", ErrConflict, required)
		}
	}
	if !has(m.superOptions, "rw") {
		return fmt.Errorf("%w: existing superblock is not writable", ErrConflict)
	}
	for _, forbidden := range []string{"ro", "noexec", "suid", "dev"} {
		if has(m.options, forbidden) {
			return fmt.Errorf("%w: incompatible VFS mount option %s", ErrConflict, forbidden)
		}
	}
	for _, forbidden := range []string{"ro", "noload", "norecovery", "nobarrier", "barrier=0", "data=writeback", "errors=continue", "errors=panic"} {
		if has(m.superOptions, forbidden) {
			return fmt.Errorf("%w: incompatible ext4 mount option %s", ErrConflict, forbidden)
		}
	}
	for _, option := range m.superOptions {
		if strings.HasPrefix(option, "errors=") && option != "errors=remount-ro" || strings.HasPrefix(option, "data=") && option != "data=ordered" && option != "data=journal" || strings.HasPrefix(option, "barrier=") && option != "barrier=1" {
			return fmt.Errorf("%w: unrecognized ext4 safety option %s", ErrConflict, option)
		}
	}
	return nil
}
