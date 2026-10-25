package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestControlAuthorityUsesBoundedCanonicalPrivateInput(t *testing.T) {
	a := Authority{Root: "/state/releases", TrustFile: "/boot/keys.json", Digest: strings.Repeat("a", 64)}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAuthority(raw)
	if err != nil || got == nil || *got != a {
		t.Fatal(got, err)
	}
	if got, err := DecodeAuthority(nil); got != nil || err != nil {
		t.Fatal(got, err)
	}
	for _, bad := range []string{
		`null`, `{}`, string(raw) + `{}`, ` ` + string(raw),
		strings.Replace(string(raw), `"root":`, `"Root":`, 1),
		strings.Replace(string(raw), `"root":`, `"root":"/other","root":`, 1),
		strings.Replace(string(raw), `/state/releases`, `relative`, 1),
		strings.Replace(string(raw), `/state/releases`, `/state/../releases`, 1),
		strings.Replace(string(raw), strings.Repeat("a", 64), strings.Repeat("A", 64), 1),
		strings.Repeat(" ", MaxAuthorityBytes+1),
	} {
		if _, err := DecodeAuthority([]byte(bad)); err == nil {
			t.Fatalf("accepted invalid authority: %.100s", bad)
		}
	}
}
