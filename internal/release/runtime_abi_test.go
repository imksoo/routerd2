//go:build linux

package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/imksoo/routerd2/internal/runtimeabi"
)

func TestSignedManifestABIMustMatchEveryNativeArtifact(t *testing.T) {
	m, entries := entriesFor(t, "fixture-r1", 1)
	if m.Version != 3 || !m.RuntimeABI.Equal(runtimeabi.Current()) {
		t.Fatal("manifest omitted executable ABI declaration")
	}
	m.RuntimeABI.ConfigurationContract = "other-valid-contract"
	raw, sig, err := SignManifest(m, testKey())
	if err != nil {
		t.Fatal(err)
	}
	entries[0].body, entries[1].body = raw, sig
	s := newTestStore(t)
	if _, err := s.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries))); err == nil {
		t.Fatal("signed claim differing from executable ABI accepted")
	}
	if state := mustState(t, s); state.Generation != 0 || state.Active != nil {
		t.Fatal("bad ABI changed boot authority")
	}
}

func TestLegacyManifestV2AndMissingABIHaveNoCompatibilityReader(t *testing.T) {
	m, raw, _ := signedFixture(t, "fixture-r1", 1, nil)
	abi, _ := json.Marshal(m.RuntimeABI)
	old := bytes.Replace(raw, append([]byte(`,"runtime_abi":`), abi...), nil, 1)
	old = bytes.Replace(old, []byte(`"version":3`), []byte(`"version":2`), 1)
	sig := ed25519.Sign(testKey(), append([]byte("routerd2 signed extracted runtime manifest v2\x00"), old...))
	public := testKey().Public().(ed25519.PublicKey)
	if _, _, err := VerifyManifest(old, sig, map[string]ed25519.PublicKey{KeyID(public): public}, "linux", runtime.GOARCH, DefaultMaxArtifactBytes); err == nil {
		t.Fatal("old unsigned-ABI manifest accepted")
	}
	m.RuntimeABI = runtimeabi.Descriptor{}
	if _, _, err := SignManifest(m, testKey()); err == nil {
		t.Fatal("manifest without ABI declaration signed")
	}
}
