//go:build linux

package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
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

func stageDifferentABI(t *testing.T, s *Store, id string, sequence uint64) Ref {
	t.Helper()
	profile := runtimeabi.Current()
	profile.ConfigurationContract = "resource-program-native-2"
	record, err := runtimeabi.EncodeRecord(profile)
	if err != nil || len(record) != len(runtimeabi.CurrentRecord) {
		t.Fatal("bad ABI mutation fixture", err)
	}
	files := map[string][]byte{}
	for name, data := range binariesFor(t, id) {
		if bytes.Count(data, []byte(runtimeabi.CurrentRecord)) != 1 {
			t.Fatal("fixture needs one compiled ABI record")
		}
		files[name] = bytes.Replace(data, []byte(runtimeabi.CurrentRecord), []byte(record), 1)
	}
	m, _, _ := signedFixture(t, id, sequence, files)
	m.RuntimeABI = profile
	raw, sig, err := SignManifest(m, testKey())
	if err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{{name: "manifest.json", body: raw}, {name: "manifest.sig", body: sig}, {name: "routerd", body: files["routerd"]}, {name: "routerd-modules", body: files["routerd-modules"]}}
	ref, err := s.Stage(context.Background(), bytes.NewReader(fixtureArchive(t, entries)))
	if err != nil {
		t.Fatal("valid different ABI should stage for inspection", err)
	}
	return ref
}

func TestFormatChangingPrepareNeedsQualifiedMigrationNotJustCallbackSuccess(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	initializeFixture(t, s)
	target := stageDifferentABI(t, s, "fixture-r2", 2)
	before := mustState(t, s)
	if _, err := s.Prepare(ctx, target, before.Generation, PrepareOptions{}); !errors.Is(err, runtimeabi.ErrIncompatible) {
		t.Fatal("raw prepare accepted changed formats", err)
	}
	called := false
	if _, err := s.PrepareChecked(ctx, target, before.Generation, PrepareOptions{}, func(context.Context, State, *Image) error { called = true; return nil }); !errors.Is(err, runtimeabi.ErrIncompatible) || called {
		t.Fatal("callback success bypassed missing migration policy", err)
	}
	if !reflect.DeepEqual(mustState(t, s), before) {
		t.Fatal("refused ABI transition changed authority")
	}
}

func TestConfirmedRollbackCannotSelectDifferentFormatContract(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	target := stageDifferentABI(t, s, "fixture-r2", 2)
	// Simulate an installer's later, separately qualified state transition.
	// A direct rollback still needs its own compatibility/migration policy.
	state, err := s.next(mustState(t, s), "rollback")
	if err != nil {
		t.Fatal(err)
	}
	state.Active = &target
	state.Previous = &base
	state.HighWatermark = 2
	if err := s.commit(state); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollback(ctx, state.Generation, "changed format rollback"); !errors.Is(err, runtimeabi.ErrIncompatible) {
		t.Fatal("rollback accepted changed formats", err)
	}
	if !reflect.DeepEqual(mustState(t, s), state) {
		t.Fatal("refused rollback changed selector")
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
