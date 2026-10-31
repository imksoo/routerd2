package runtimeplan

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestSAMClientSnapshotMatchesConfiguredClaim(t *testing.T) {
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	cfg := samclientconfig.Config{Node: samclientconfig.Node{ClaimName: "leaf"}}
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := samclientconfig.Snapshot{
		ClaimRef:      "SAMEnrollmentClaim/leaf",
		ClaimDigest:   "sha256:fixture",
		Phase:         "ready",
		ObservedAt:    now,
		ExpiresAt:     now.Add(time.Minute),
		BootID:        clock.BootID,
		ObservedNanos: uint64(clock.BoottimeNanoseconds),
		ExpiresNanos:  uint64(clock.BoottimeNanoseconds + int64(time.Minute)),
		RRSet:         &api.SAMRRSetSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/leaves"},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	instance := serviceconfig.Instance{Role: "sam_client", Config: configBytes}
	if err := validateSnapshot(instance, data); err != nil {
		t.Fatal(err)
	}
	snapshot.ClaimRef = "SAMEnrollmentClaim/other"
	data, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSnapshot(instance, data); err == nil {
		t.Fatal("accepted an observation for a different enrollment claim")
	}
}
