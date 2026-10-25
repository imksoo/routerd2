//go:build linux

package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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

func TestBootAndAutomaticFallbackRejectUnqualifiedFormatChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := initializeFixture(t, s)
	target := stageDifferentABI(t, s, "fixture-r2", 2)
	prepared, err := s.next(mustState(t, s), "prepare")
	if err != nil {
		t.Fatal(err)
	}
	prepared.Pending = &Pending{Release: target, MaxAttempts: 1}
	prepared.HighWatermark = 2
	if err := s.commit(prepared); err != nil {
		t.Fatal(err)
	}
	boot, err := s.BeginBoot(ctx, "unqualified-boot")
	if err == nil {
		boot.Image.Close()
	}
	if !errors.Is(err, runtimeabi.ErrIncompatible) || !reflect.DeepEqual(mustState(t, s), prepared) {
		t.Fatal("boot activated unqualified format change", err)
	}
	trial, err := s.next(prepared, "begin-boot")
	if err != nil {
		t.Fatal(err)
	}
	trial.Active = &target
	trial.Previous = &base
	trial.Attempt = &Attempt{BootID: "unqualified-boot", Token: strings.Repeat("a", 32), Number: 1, StartedAt: s.now().UTC()}
	if err := s.commit(trial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FailBoot(ctx, "unqualified-boot", trial.Attempt.Token, "unsafe fallback"); !errors.Is(err, runtimeabi.ErrIncompatible) {
		t.Fatal("explicit failure selected incompatible fallback", err)
	}
	boot, err = s.BeginBoot(ctx, "later-boot")
	if err == nil {
		boot.Image.Close()
	}
	if !errors.Is(err, runtimeabi.ErrIncompatible) || !reflect.DeepEqual(mustState(t, s), trial) {
		t.Fatal("automatic fallback bypassed ABI policy", err)
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

func TestOfflinePublisherRejectsMixedNativeABIsBeforeOutput(t *testing.T) {
	profile := runtimeabi.Current()
	profile.ConfigurationContract = "resource-program-native-2"
	record, err := runtimeabi.EncodeRecord(profile)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*os.File{}
	for name, data := range fixtureBinaries(t) {
		if name == "routerd-modules" {
			data = bytes.Replace(data, []byte(runtimeabi.CurrentRecord), []byte(record), 1)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, data, 0500); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files[name] = file
	}
	m, _, _ := signedFixture(t, fixtureRelease, 1, nil)
	var out bytes.Buffer
	_, err = WriteBundle(context.Background(), &out, BundleOptions{ReleaseID: fixtureRelease, Sequence: 1, GOOS: "linux", GOARCH: runtime.GOARCH, BootABI: 1, WorkerABI: 1, Entrypoint: m.Entrypoint, Roles: m.Roles, Artifacts: m.Artifacts}, testKey(), files)
	if err == nil || out.Len() != 0 {
		t.Fatal("mixed native declarations reached published output", err)
	}
}
