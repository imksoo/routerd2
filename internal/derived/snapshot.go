// Package derived evaluates historical assertion transitions from one finite,
// immutable observation snapshot. Returned state/events require an atomic
// versioned commit; they grant no authority to execute an effect.
package derived

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Truth string

const (
	Indeterminate Truth = "indeterminate"
	False         Truth = "false"
	True          Truth = "true"
)

type Value struct {
	Text          string
	Known         bool
	Scope, Origin string
	Continuity    string
	// Optional stronger proof for literal comparisons. Presence-only predicates
	// can stay continuous while a nonempty value changes on every publication.
	EqualityContinuity          string
	ObservedNanos, ExpiresNanos uint64
}

type Snapshot struct {
	id, scope       string
	now             uint64
	wall            time.Time
	values          map[string]Value
	continuity      string
	continuousSince uint64
}

func token(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) < 0
}

// NewSnapshot copies caller-owned values. Scope/Origin must come from the
// accepted current producer registry; supplying them is not authentication.
// Expired, absent, future, zero-expiry and old-scope values read as unknown.
func NewSnapshot(id, scope string, now uint64, wall time.Time, values map[string]Value) (Snapshot, error) {
	if !token(id, 256) || !token(scope, 1024) || wall.IsZero() || len(values) > 1024 {
		return Snapshot{}, errors.New("invalid derived snapshot identity or size")
	}
	s := Snapshot{id: id, scope: scope, now: now, wall: wall.UTC(), values: make(map[string]Value, len(values))}
	bytes := 0
	for key, value := range values {
		if !token(key, 512) || len(value.Text) > 64<<10 || !utf8.ValidString(value.Text) || len(value.Scope) > 1024 || len(value.Origin) > 256 || len(value.Continuity) > 128 || len(value.EqualityContinuity) > 128 || !utf8.ValidString(value.EqualityContinuity) || !utf8.ValidString(value.Continuity) || !utf8.ValidString(value.Scope) || !utf8.ValidString(value.Origin) {
			return Snapshot{}, errors.New("invalid bounded derived observation")
		}
		bytes += len(key) + len(value.Text) + len(value.Scope) + len(value.Origin) + len(value.Continuity) + len(value.EqualityContinuity)
		if bytes > 2<<20 {
			return Snapshot{}, errors.New("derived snapshot exceeds 2 MiB")
		}
		s.values[key] = value
	}
	return s, nil
}

func normalize(field string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(field, "${"), "}"))
}

type evaluated struct {
	truth                Truth
	until                uint64 // union validity for hold continuity
	observed, proofUntil uint64 // a coherent proof for published freshness
}

func (s Snapshot) read(field string) (string, bool, uint64, uint64) {
	field = normalize(field)
	if !strings.Contains(field, ".status.") {
		return field, true, ^uint64(0), s.now
	}
	value, ok := s.values[field]
	if !ok || !value.Known || value.Scope != s.scope || value.Origin == "" || value.ExpiresNanos == 0 || value.ExpiresNanos <= value.ObservedNanos || value.ObservedNanos > s.now || s.now >= value.ExpiresNanos {
		return "", false, 0, 0
	}
	return value.Text, true, value.ExpiresNanos, value.ObservedNanos
}

func (s Snapshot) authority(refs []string, equality map[string]bool) string {
	tokens := make([]string, 0, len(refs))
	for _, ref := range refs {
		value := s.values[ref]
		continuity := value.Continuity
		if equality[ref] && value.EqualityContinuity != "" {
			continuity = value.EqualityContinuity
		}
		tokens = append(tokens, continuity)
	}
	body, _ := json.Marshal([]any{s.producerAuthority(refs), s.continuity, s.continuousSince, tokens})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func (s Snapshot) producerAuthority(refs []string) string {
	parts := []any{s.scope}
	for _, ref := range refs {
		value := s.values[ref]
		parts = append(parts, ref, value.Scope, value.Origin)
	}
	body, _ := json.Marshal(parts)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// WithContinuity attaches a trusted evaluator's proof that the predicate and
// producer authority remained continuous between sampled evaluations. This
// cannot authenticate a caller or extend the current observation's lease.
// The immutable value map is shared, never exposed to the caller.
func (s Snapshot) WithContinuity(identity string, since uint64) (Snapshot, error) {
	if !token(identity, 128) || since > s.now {
		return Snapshot{}, errors.New("invalid derived continuity proof")
	}
	s.continuity = identity
	s.continuousSince = since
	body, _ := json.Marshal([]any{s.id, identity, since})
	sum := sha256.Sum256(body)
	s.id = hex.EncodeToString(sum[:])
	return s, nil
}

func combine(values []evaluated, all bool, now uint64) evaluated {
	if len(values) == 0 {
		return evaluated{truth: False, until: ^uint64(0), proofUntil: ^uint64(0), observed: now}
	}
	result := evaluated{truth: False, until: ^uint64(0), proofUntil: ^uint64(0), observed: ^uint64(0)}
	if all {
		result.truth = True
	}
	for _, v := range values {
		if all && v.truth == False || !all && v.truth == True {
			result.truth = v.truth
			break
		}
		if v.truth == Indeterminate {
			result.truth = Indeterminate
		}
	}
	if result.truth == Indeterminate {
		return evaluated{truth: Indeterminate}
	}
	witness := all && result.truth == False || !all && result.truth == True
	if witness {
		result.until = 0
		chosen := false
		for _, v := range values {
			if v.truth == result.truth {
				result.until = max(result.until, v.until)
				// Freshness and expiry must belong to the same witness. Mixing
				// a fresh short lease with an old long lease invents evidence.
				if !chosen || v.observed > result.observed || v.observed == result.observed && v.proofUntil > result.proofUntil {
					result.observed, result.proofUntil = v.observed, v.proofUntil
					chosen = true
				}
			}
		}
	} else {
		for _, v := range values {
			result.until = min(result.until, v.until)
			result.observed = min(result.observed, v.observed)
			result.proofUntil = min(result.proofUntil, v.proofUntil)
		}
	}
	return result
}
