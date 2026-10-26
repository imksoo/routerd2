// SPDX-License-Identifier: BSD-3-Clause

// Package runtimeabi describes format contracts without importing a compiler,
// engine or state reader. Declarations are not evidence of runtime health or a
// substitute for inspecting the actual state and qualifying a release.
package runtimeabi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const Prefix = "ROUTERD2-RUNTIME-ABI-V1:"
const Suffix = ":END"
const MaxRecordBytes = 4096
const MaxStateSchemas = 32

// CurrentRecord is a complete, retained rodata record, not linker-supplied
// publisher metadata. A breaking reader/writer or input-semantics change must
// change this declaration and its qualification. Configuration's implicit
// journal contract includes identity/adoption fences; it is not a file header.
const CurrentRecord = Prefix + `{"version":1,"profile":"routerd-native-v1","boot_abi":1,"worker_abi":3,"configuration_contract":"resource-program-native-11","configuration":{"read_min":2,"read_max":2,"write":2},"state":[{"name":"configuration","format":{"read_min":1,"read_max":1,"write":1}},{"name":"events","format":{"read_min":6,"read_max":6,"write":6}},{"name":"network","format":{"read_min":5,"read_max":5,"write":5}},{"name":"release","format":{"read_min":1,"read_max":1,"write":1}},{"name":"snapshot","format":{"read_min":1,"read_max":1,"write":1}},{"name":"ssh_identity","format":{"read_min":1,"read_max":1,"write":1}}]}` + Suffix

type Format struct {
	ReadMin uint32 `json:"read_min"`
	ReadMax uint32 `json:"read_max"`
	Write   uint32 `json:"write"`
}

func (f Format) reads(version uint32) bool { return version >= f.ReadMin && version <= f.ReadMax }
func (f Format) valid() bool {
	return f.ReadMin > 0 && f.ReadMax <= 65535 && f.ReadMin <= f.ReadMax && f.reads(f.Write)
}

type StateSchema struct {
	Name   string `json:"name"`
	Format Format `json:"format"`
}

type Descriptor struct {
	Version               int           `json:"version"`
	Profile               string        `json:"profile"`
	BootABI               uint32        `json:"boot_abi"`
	WorkerABI             uint32        `json:"worker_abi"`
	ConfigurationContract string        `json:"configuration_contract"`
	Configuration         Format        `json:"configuration"`
	State                 []StateSchema `json:"state"`
}

var name = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
var ErrIncompatible = errors.New("runtime format contracts are incompatible")

func (d Descriptor) Validate() error {
	if d.Version != 1 || !name.MatchString(d.Profile) || !name.MatchString(d.ConfigurationContract) || d.BootABI == 0 || d.BootABI > 65535 || d.WorkerABI == 0 || d.WorkerABI > 65535 || !d.Configuration.valid() || len(d.State) == 0 || len(d.State) > MaxStateSchemas {
		return errors.New("invalid or unsupported runtime ABI declaration")
	}
	previous := ""
	for _, s := range d.State {
		if !name.MatchString(s.Name) || s.Name <= previous || !s.Format.valid() {
			return errors.New("runtime state schemas must be bounded, unique and sorted with valid reader/writer ranges")
		}
		previous = s.Name
	}
	return nil
}

func (d Descriptor) Equal(other Descriptor) bool {
	if d.Version != other.Version || d.Profile != other.Profile || d.BootABI != other.BootABI || d.WorkerABI != other.WorkerABI || d.ConfigurationContract != other.ConfigurationContract || d.Configuration != other.Configuration || len(d.State) != len(other.State) {
		return false
	}
	for i, s := range d.State {
		if s != other.State[i] {
			return false
		}
	}
	return true
}

func Current() Descriptor {
	d, err := ParseRecord(CurrentRecord)
	if err != nil {
		panic(err)
	}
	return d // decoder owns fresh slices; no caller can mutate shared metadata
}

func EncodeRecord(d Descriptor) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	if len(Prefix)+len(raw)+len(Suffix) > MaxRecordBytes {
		return "", errors.New("runtime ABI declaration exceeds its byte bound")
	}
	return Prefix + string(raw) + Suffix, nil
}

func ParseRecord(record string) (Descriptor, error) {
	var d Descriptor
	if len(record) < len(Prefix)+len(Suffix) || len(record) > MaxRecordBytes || !strings.HasPrefix(record, Prefix) || !strings.HasSuffix(record, Suffix) {
		return d, errors.New("invalid runtime ABI record")
	}
	raw := []byte(record[len(Prefix) : len(record)-len(Suffix)])
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&d); err != nil {
		return Descriptor{}, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Descriptor{}, errors.New("runtime ABI has trailing data")
	}
	canonical, err := EncodeRecord(d)
	if err != nil || canonical != record {
		return Descriptor{}, errors.New("runtime ABI must be canonical and unambiguous")
	}
	return d, nil
}

type StateVersion struct {
	Name    string
	Version uint32 // zero is an explicitly observed absent file, not an omitted fact
}

type Facts struct {
	Configuration         uint32
	ConfigurationContract string
	State                 []StateVersion
}

// CheckTransition verifies facts supplied by a trusted state inspector. It does
// not invent observations, read files, migrate state, restore identity/leases or
// authorize revocation rollback. Both the running and candidate writer formats
// must remain readable, even if the observed file still uses an older format.
func CheckTransition(active, target Descriptor, facts Facts) error {
	if err := active.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if active.Profile != target.Profile || active.BootABI != target.BootABI || active.WorkerABI != target.WorkerABI || active.ConfigurationContract != target.ConfigurationContract || facts.ConfigurationContract != active.ConfigurationContract {
		return fmt.Errorf("%w: profile, protocol or configuration semantics differ", ErrIncompatible)
	}
	if !compatible(active.Configuration, target.Configuration, facts.Configuration) || facts.Configuration == 0 {
		return fmt.Errorf("%w: configuration format", ErrIncompatible)
	}
	if len(active.State) != len(target.State) || len(facts.State) != len(active.State) {
		return fmt.Errorf("%w: missing state observations or component migration policy", ErrIncompatible)
	}
	for i, actual := range facts.State {
		a, t := active.State[i], target.State[i]
		if actual.Name != a.Name || actual.Name != t.Name || !compatible(a.Format, t.Format, actual.Version) {
			return fmt.Errorf("%w: state format %s", ErrIncompatible, a.Name)
		}
	}
	return nil
}

func compatible(active, target Format, actual uint32) bool {
	return (actual == 0 || active.reads(actual) && target.reads(actual)) && target.reads(active.Write) && active.reads(target.Write)
}

// ReadRecord streams a bounded-overlap search of an already verified readonly
// ELF section. It requires exactly one complete valid record and never executes
// incoming code or allocates the whole section.
func ReadRecord(r io.Reader) (Descriptor, error) {
	var found Descriptor
	count := 0
	buffer := make([]byte, (32<<10)+MaxRecordBytes)
	tail := 0
	var total int64
	last := int64(-1)
	lastCandidate := int64(-1)
	candidates, emptyReads := 0, 0
	for {
		n, err := r.Read(buffer[tail:])
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return Descriptor{}, io.ErrNoProgress
			}
			continue
		}
		emptyReads = 0
		data := buffer[:tail+n]
		base := total - int64(tail)
		for offset := 0; offset < len(data); {
			index := bytes.Index(data[offset:], []byte(Prefix))
			if index < 0 {
				break
			}
			index += offset
			if base+int64(index) > lastCandidate {
				lastCandidate = base + int64(index)
				candidates++
				if candidates > 16 {
					return Descriptor{}, errors.New("too many runtime ABI marker candidates")
				}
			}
			end := bytes.Index(data[index+len(Prefix):min(len(data), index+MaxRecordBytes)], []byte(Suffix))
			if end >= 0 {
				end += index + len(Prefix) + len(Suffix)
				if d, parseErr := ParseRecord(string(data[index:end])); parseErr == nil && base+int64(index) > last {
					found = d
					last = base + int64(index)
					count++
					if count > 1 {
						return Descriptor{}, errors.New("multiple complete runtime ABI records")
					}
				}
			}
			offset = index + len(Prefix)
		}
		total += int64(n)
		if err != nil {
			if err != io.EOF {
				return Descriptor{}, err
			}
			if count != 1 {
				return Descriptor{}, errors.New("missing complete runtime ABI record")
			}
			return found, nil
		}
		tail = min(MaxRecordBytes-1, len(data))
		copy(buffer, data[len(data)-tail:])
	}
}
