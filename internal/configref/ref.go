// Package configref defines bounded boot references without importing the
// resource compiler, configuration store, kernel engines or worker runtime.
package configref

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const SelectionEnv = "ROUTERD_STARTUP_SELECTION"
const MaxSelectionBytes = 1024

type Ref struct {
	StoreID  string `json:"store_id"`
	Revision uint64 `json:"revision"`
	Digest   string `json:"digest"`
}

// Generation is a persistent boot-control selection generation, never a
// process restart count or a newly generated value on each boot. ExpectedRevision
// fences adoption into the existing authoritative journal.
type Selection struct {
	Generation       uint64    `json:"generation"`
	ExpectedRevision uint64    `json:"expected_revision"`
	Snapshot         Ref       `json:"snapshot"`
	Recovery         *Recovery `json:"recovery,omitempty"`
}

// Recovery permits exactly two journal states: the prepared known-good
// revision, or the exact failed selection's adopted revision with no later
// commit. It is a bounded fence, not a general revision range or predicate.
type Recovery struct {
	FailedGeneration uint64 `json:"failed_generation"`
	FailedSnapshot   Ref    `json:"failed_snapshot"`
}

type Adoption struct {
	Selection Selection `json:"selection"`
	Revision  uint64    `json:"revision"`
	Recovered bool      `json:"recovered,omitempty"` // failed candidate had actually been adopted
}

func (s Selection) Clone() Selection {
	if s.Recovery != nil {
		r := *s.Recovery
		s.Recovery = &r
	}
	return s
}

func (s Selection) Equal(other Selection) bool {
	if s.Generation != other.Generation || s.ExpectedRevision != other.ExpectedRevision || s.Snapshot != other.Snapshot {
		return false
	}
	if s.Recovery == nil || other.Recovery == nil {
		return s.Recovery == nil && other.Recovery == nil
	}
	return *s.Recovery == *other.Recovery
}

func (a Adoption) Equal(other Adoption) bool {
	return a.Revision == other.Revision && a.Recovered == other.Recovered && a.Selection.Equal(other.Selection)
}

func ValidHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r Ref) Validate() error {
	if !ValidHex(r.StoreID, 32) || r.Revision == 0 || !ValidHex(r.Digest, 64) {
		return errors.New("invalid configuration snapshot reference")
	}
	return nil
}

func (s Selection) Validate() error {
	if s.Generation == 0 || s.ExpectedRevision == 0 || s.ExpectedRevision == ^uint64(0) || s.Snapshot.Revision > s.ExpectedRevision {
		return errors.New("invalid startup generation or expected configuration revision")
	}
	if err := s.Snapshot.Validate(); err != nil {
		return err
	}
	if s.Recovery != nil {
		r := s.Recovery
		if r.FailedGeneration == 0 || r.FailedGeneration >= s.Generation || s.ExpectedRevision > ^uint64(0)-2 || s.Snapshot.Revision != s.ExpectedRevision || r.FailedSnapshot.StoreID != s.Snapshot.StoreID || r.FailedSnapshot.Revision > s.ExpectedRevision {
			return errors.New("invalid conditional startup recovery fence")
		}
		return r.FailedSnapshot.Validate()
	}
	return nil
}

func (a Adoption) Validate(storeID string, revision uint64) error {
	if err := a.Selection.Validate(); err != nil {
		return err
	}
	want := a.Selection.ExpectedRevision + 1
	if a.Recovered {
		if a.Selection.Recovery == nil {
			return errors.New("recovered receipt requires a recovery selection")
		}
		want++
	}
	if a.Selection.Snapshot.StoreID != storeID || a.Revision != want || a.Revision > revision {
		return errors.New("invalid adopted startup provenance")
	}
	return nil
}

// DecodeSelection is used at the worker boundary. Empty input means normal
// committed-journal startup. It rejects duplicate and case-aliased fields as
// well as oversized input; it does not consult the filesystem or parse resources.
func DecodeSelection(raw []byte) (*Selection, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > MaxSelectionBytes {
		return nil, errors.New("startup selection exceeds its byte budget")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var object func(int) error
	object = func(depth int) error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if token != json.Delim('{') {
			return errors.New("startup selection requires an object")
		}
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok || seen[name] {
				return errors.New("duplicate startup field")
			}
			seen[name] = true
			if depth == 0 && name == "snapshot" || depth == 2 && name == "failed_snapshot" {
				if err := object(1); err != nil {
					return err
				}
				continue
			}
			if depth == 0 && name == "recovery" {
				if err := object(2); err != nil {
					return err
				}
				continue
			}
			if !(depth == 0 && (name == "generation" || name == "expected_revision") || depth == 1 && (name == "store_id" || name == "revision" || name == "digest") || depth == 2 && name == "failed_generation") {
				return errors.New("unknown startup field")
			}
			token, err = d.Token()
			if err != nil {
				return err
			}
			if _, ok := token.(json.Delim); ok {
				return errors.New("invalid startup scalar")
			}
		}
		_, err = d.Token()
		return err
	}
	if err := object(0); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("expected one startup selection")
	}
	type plainSelection Selection
	var plain plainSelection
	if err := json.Unmarshal(raw, &plain); err != nil {
		return nil, err
	}
	s := Selection(plain)
	return &s, s.Validate()
}

// UnmarshalJSON keeps boot and journal embedding from bypassing the strict
// bounded decoder used at the worker boundary.
func (s *Selection) UnmarshalJSON(raw []byte) error {
	selected, err := DecodeSelection(raw)
	if err != nil {
		return err
	}
	if selected == nil {
		return errors.New("startup selection must be present")
	}
	*s = *selected
	return nil
}
