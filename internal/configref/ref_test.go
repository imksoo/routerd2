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
	}
}
