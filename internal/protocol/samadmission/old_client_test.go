package samadmission

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	routerstate "github.com/imksoo/routerd/pkg/state"
	"gopkg.in/yaml.v3"
)

func TestAdmissionAgainstRouterdClient(t *testing.T) {
	binary := os.Getenv("OLD_ROUTERD")
	if binary == "" {
		t.Skip("set OLD_ROUTERD to the independent Linux routerd executable")
	}
	cfg := admissionFixture(t)
	dir := filepath.Dir(cfg.StatePath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, func() { close(ready) }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("native admission not ready")
	}
	claim := api.SAMEnrollmentClaimSpec{PolicyRef: "SAMEnrollmentPolicy/leaves", RRSetRef: "SAMRRSet/rrs", LeafID: "leaf-old", JoinAudience: "fixture", JoinNonce: "old-client-one", JoinTimestamp: time.Now().UTC().Format(time.RFC3339Nano), TunnelAddress: "10.255.0.9/32", Endpoint: "192.0.2.9", DirectMesh: true, Mobility: api.SAMEnrollmentClaimMobilitySpec{OwnedAddresses: []string{"198.51.100.9/32"}}}
	claim.JoinHMAC = samenrollment.JoinHMAC([]byte("fixture-join"), claim)
	router := api.Router{TypeMeta: api.TypeMeta{APIVersion: "routerd.net/v1alpha1", Kind: "Router"}, Metadata: api.ObjectMeta{Name: "old-leaf"}, Spec: api.RouterSpec{Resources: []api.Resource{
		{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMEnrollmentClaim"}, Metadata: api.ObjectMeta{Name: "leaf-old"}, Spec: claim},
		{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMEnrollmentClient"}, Metadata: api.ObjectMeta{Name: "leaf-old"}, Spec: api.SAMEnrollmentClientSpec{ClaimRef: "SAMEnrollmentClaim/leaf-old", BootstrapEndpoints: []string{"http://" + cfg.Listen.String()}, ControlAPITokenFrom: api.SecretValueSourceSpec{File: cfg.TokenFile}, StateTTLRefreshBefore: "5s", RetryBackoff: api.SAMEnrollmentRetryBackoffSpec{Min: "1s", Max: "2s"}}},
	}}}
	data, err := yaml.Marshal(router)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "old-leaf.yaml")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "old-leaf.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	root := filepath.Join(dir, "old-leaf")
	statePath := filepath.Join(dir, "old-leaf.db")
	oldCtx, oldCancel := context.WithCancel(ctx)
	defer oldCancel()
	command := exec.CommandContext(oldCtx, binary, "serve", "--sandbox", "--root", root, "--config", source, "--state-file", statePath, "--http-listen", "127.0.0.1:0", "--controllers", "sam-enrollment-client", "--graceful-stop-timeout", "0")
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	oldDone := make(chan error, 1)
	go func() { oldDone <- command.Wait() }()
	defer func() {
		oldCancel()
		<-oldDone
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	}()
	until := time.Now().Add(15 * time.Second)
	for {
		store, err := routerstate.OpenSQLiteReadOnly(statePath)
		if err == nil {
			status := store.ObjectStatus(api.MobilityAPIVersion, "SAMEnrollmentClient", "leaf-old")
			records, readErr := store.GetDynamicConfigPartsBySource("SAMRRSet/rrs")
			store.Close()
			if status["phase"] == "Ready" && readErr == nil && len(records) == 1 && strings.Contains(records[0].ResourcesJSON, "SAMPeerGroup") {
				break
			}
		}
		if time.Now().After(until) {
			t.Fatal("independent old client did not accept/persist native RR and direct topology")
		}
		time.Sleep(30 * time.Millisecond)
	}
	client := controlapi.NewHTTPClient("http://" + cfg.Listen.String()).WithBearerToken("fixture-control")
	if _, err := client.RevokeSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimRevokeRequest{Name: "leaf-old"}); err != nil {
		t.Fatal(err)
	}
	_, err = client.GetSAMEnrollmentTopology(ctx, controlapi.SAMEnrollmentTopologyGetRequest{Name: "rrs", ClaimRef: "SAMEnrollmentClaim/leaf-old", ClaimDigest: samenrollment.ClaimDigest(claim), ClaimIdentityDigest: samenrollment.ClientIdentityDigest(claim)})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("withdrawn identity remained admitted: %v", err)
	}
	t.Log("independent routerd process submitted its signed claim, validated and persisted the native RR/direct topology; native revocation rejected that identity")
}
