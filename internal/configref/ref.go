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
	Generation       uint64 `json:"generation"`
	ExpectedRevision uint64 `json:"expected_revision"`
	Snapshot         Ref    `json:"snapshot"`
}

type Adoption struct {
	Selection Selection `json:"selection"`
	Revision  uint64    `json:"revision"`
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
	return s.Snapshot.Validate()
}

func (a Adoption) Validate(storeID string, revision uint64) error {
	if err := a.Selection.Validate(); err != nil {
		return err
	}
	if a.Selection.Snapshot.StoreID != storeID || a.Revision != a.Selection.ExpectedRevision+1 || a.Revision > revision {
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
			if depth == 0 && name == "snapshot" {
				if err := object(1); err != nil {
					return err
				}
				continue
			}
			if !(depth == 0 && (name == "generation" || name == "expected_revision") || depth == 1 && (name == "store_id" || name == "revision" || name == "digest")) {
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
