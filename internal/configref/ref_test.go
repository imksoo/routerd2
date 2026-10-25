package configref

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSelectionDecoderRejectsAmbiguousAndUnboundedInput(t *testing.T) {
	s := Selection{Generation: 10, ExpectedRevision: 2, Snapshot: Ref{StoreID: strings.Repeat("a", 32), Revision: 1, Digest: strings.Repeat("b", 64)}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeSelection(raw)
	if err != nil || got == nil || *got != s {
		t.Fatal("canonical selection rejected", err)
	}
	if got, err := DecodeSelection(nil); err != nil || got != nil {
		t.Fatal("absent explicit startup must retain committed startup", err)
	}
	for _, bad := range []string{
		`null`, `{}`, string(raw) + ` {}`, strings.Repeat(" ", MaxSelectionBytes+1),
		strings.Replace(string(raw), `"generation":10`, `"generation":10,"generation":10`, 1),
		strings.Replace(string(raw), `"generation":10`, `"Generation":10`, 1),
		strings.Replace(string(raw), `"revision":1`, `"Revision":1`, 1),
		strings.Replace(string(raw), `"revision":1`, `"revision":1,"revision":1`, 1),
		strings.Replace(string(raw), `"generation":10`, `"generation":-1`, 1),
		strings.Replace(string(raw), `"expected_revision":2`, `"expected_revision":18446744073709551615`, 1),
		strings.Replace(string(raw), `"generation":10`, `"generation":{}`, 1),
	} {
		if _, err := DecodeSelection([]byte(bad)); err == nil {
			t.Fatal("accepted ambiguous startup input")
		}
		var selection Selection
		if err := json.Unmarshal([]byte(bad), &selection); err == nil {
			t.Fatal("typed boot/journal decoder bypassed strict startup input")
		}
	}
}

func TestRecoverySelectionDecoderAndDeepCopy(t *testing.T) {
	s := Selection{Generation: 21, ExpectedRevision: 7, Snapshot: Ref{StoreID: strings.Repeat("a", 32), Revision: 7, Digest: strings.Repeat("b", 64)}, Recovery: &Recovery{FailedGeneration: 20, FailedSnapshot: Ref{StoreID: strings.Repeat("a", 32), Revision: 3, Digest: strings.Repeat("c", 64)}}}
	raw, err := json.Marshal(s)
	if err != nil || len(raw) > MaxSelectionBytes {
		t.Fatal("bounded recovery does not fit worker contract", err)
	}
	got, err := DecodeSelection(raw)
	if err != nil || !s.Equal(*got) {
		t.Fatal("recovery JSON changed semantic identity", err)
	}
	clone := s.Clone()
	clone.Recovery.FailedGeneration--
	if s.Recovery.FailedGeneration != 20 || s.Equal(clone) {
		t.Fatal("recovery clone shares mutable authority")
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"failed_generation":20`, `"Failed_generation":20`, 1),
		strings.Replace(string(raw), `"failed_generation":20`, `"failed_generation":20,"failed_generation":20`, 1),
		strings.Replace(string(raw), `"failed_generation":20`, `"failed_generation":21`, 1),
		strings.Replace(string(raw), `"revision":7`, `"revision":6`, 1),
	} {
		if _, err := DecodeSelection([]byte(bad)); err == nil {
			t.Fatal("invalid recovery fence accepted")
		}
	}
}
