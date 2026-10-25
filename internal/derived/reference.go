package derived

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type Reference struct {
	Kind     string `json:"kind"`
	Resource string `json:"resource"`
	Field    string `json:"field"`
}

func (r Reference) Key() string { return r.Kind + "/" + r.Resource + ".status." + r.Field }

func ParseReference(field string) (Reference, bool, error) {
	field = normalize(field)
	at := strings.LastIndex(field, ".status.")
	if at < 0 {
		return Reference{}, false, nil
	}
	left := strings.SplitN(field[:at], "/", 2)
	if len(left) != 2 || !token(left[0], 128) || !token(left[1], 128) || !token(field[at+8:], 128) {
		return Reference{}, true, errors.New("invalid derived status reference")
	}
	return Reference{Kind: left[0], Resource: left[1], Field: field[at+8:]}, true, nil
}

func (p Program) References() []Reference {
	out := make([]Reference, 0, len(p.refs))
	for _, key := range p.refs {
		ref, _, _ := ParseReference(key)
		out = append(out, ref)
	}
	return out
}

// InputIdentity detects changed values/continuity without renewing the age of
// an unchanged observation. The caller must still check the finite deadline.
func (p Program) InputIdentity(s Snapshot) string {
	parts := []any{s.scope, p.target(s).truth, s.authority(p.refs, p.equality)}
	body, _ := json.Marshal(parts)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

type InputProof struct {
	Value      Truth
	UntilNanos uint64
	Authority  string
}

func (p Program) Proof(s Snapshot) InputProof {
	v := p.target(s)
	return InputProof{Value: v.truth, UntilNanos: v.until, Authority: s.authority(p.refs, p.equality)}
}

func (s Snapshot) Timebase() (scope string, now uint64) { return s.scope, s.now }
