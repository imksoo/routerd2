package samclient

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

// This check uses the actual routerd server, claim validation, signature check,
// SQLite admission/revocation and topology projection. Sandbox mode disables
// unrelated kernel controllers; enrollment and its HTTP/store path remain real.
func TestEnrollmentAgainstRouterd(t *testing.T) {
	binary := os.Getenv("OLD_ROUTERD")
	if binary == "" {
		t.Skip("set OLD_ROUTERD to the independent Linux routerd server")
	}
	dir := t.TempDir()
	token := filepath.Join(dir, "control-token")
	join := filepath.Join(dir, "join-token")
	for name, value := range map[string]string{token: "isolated-control-token", join: "isolated-join-token"} {
		if err := os.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	source := fmt.Sprintf(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: independent-rr}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: underlay}
      spec: {ifname: lo, managed: false}
    - apiVersion: net.routerd.net/v1alpha1
      kind: BGPRouter
      metadata: {name: fabric}
      spec: {asn: 64512, routerID: 192.0.2.1}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMNodeSet
      metadata: {name: rrs}
      spec:
        nodes: [{nodeRef: rr-a, routeReflector: true, samEndpoint: 192.0.2.1}]
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMTransportProfile
      metadata: {name: rr-a}
      spec:
        selfNodeRef: rr-a
        mode: ipip
        addressingMode: pair-stable
        innerPrefix: 10.255.0.0/24
        underlayInterface: underlay
        localEndpoint: 192.0.2.1
        peersFrom: [{resource: SAMEnrollmentPolicy/leaves}]
        bgp: {routerRef: BGPRouter/fabric, peerASN: 64512, routeReflectorClient: true, importPolicy: {allowedPrefixes: [198.51.100.0/24]}}
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMEnrollmentPolicy
      metadata: {name: leaves}
      spec:
        transportProfileRef: SAMTransportProfile/rr-a
        rrSetRef: SAMRRSet/rrs
        rrNodeSetRef: SAMNodeSet/rrs
        joinTokenFrom: {file: %q}
        joinAudience: native-test
        tunnelAddressPrefixes: [10.255.0.0/24]
        endpointPrefixes: [192.0.2.0/24]
        mobilityPrefixes: [198.51.100.0/24]
        directMesh: {peerGroupRef: SAMPeerGroup/direct}
        ttl: 1m
`, join)
	configPath := filepath.Join(dir, "rr.yaml")
	if err := os.WriteFile(configPath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "rr.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	command := exec.CommandContext(serverCtx, binary, "serve", "--sandbox", "--root", filepath.Join(dir, "sandbox"), "--config", configPath, "--http-listen", address, "--http-token-file", token, "--controllers", "none", "--graceful-stop-timeout", "0")
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() { serverDone <- command.Wait() }()
	defer func() {
		serverCancel()
		<-serverDone
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	}()
	client := controlapi.NewHTTPClient("http://" + address).WithBearerToken("isolated-control-token")
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err = client.Status(ctx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("independent RR failed to start", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	claim := api.SAMEnrollmentClaimSpec{PolicyRef: "SAMEnrollmentPolicy/leaves", RRSetRef: "SAMRRSet/rrs", LeafID: "leaf-a", JoinAudience: "native-test", JoinNonce: "isolated-one", JoinTimestamp: time.Now().UTC().Format(time.RFC3339Nano), TunnelAddress: "10.255.0.2/32", Endpoint: "192.0.2.2", DirectMesh: true, Mobility: api.SAMEnrollmentClaimMobilitySpec{OwnedAddresses: []string{"198.51.100.2/32"}}, BGP: api.SAMEnrollmentClaimBGPSpec{ASN: 64512, RouterID: "10.255.0.2"}}
	claim.JoinHMAC = samenrollment.JoinHMAC([]byte("isolated-join-token"), claim)
	cfg := samclientconfig.Config{Node: samclientconfig.Node{ClaimName: "leaf", Claim: claim, Endpoints: []string{"http://" + address}, TokenFile: token, RefreshBeforeSeconds: 10, RetryMinSeconds: 900, RetryMaxSeconds: 900}, SocketPath: filepath.Join(dir, "sam.sock"), StatePath: filepath.Join(dir, "withdrawn")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	var sharedMu sync.Mutex
	var shared samclientconfig.Snapshot
	go func() {
		done <- Run(ctx, cfg, func(_ context.Context, s samclientconfig.Snapshot) error {
			sharedMu.Lock()
			shared = s
			sharedMu.Unlock()
			return nil
		}, func() { close(ready) })
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("native service not ready")
	}
	local := localapi.NewClient(cfg.SocketPath)
	var observation samclientconfig.Snapshot
	deadline = time.Now().Add(10 * time.Second)
	for {
		if err := local.Do(ctx, "GET", "/v1/status", nil, &observation); err != nil {
			t.Fatal(err)
		}
		if observation.RRSet != nil && observation.PeerGroup != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("RR admission not observed: %+v", observation)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if observation.RRSet.Nodes[0].NodeRef != "rr-a" || observation.RRSet.EnrollmentPolicyRef != claim.PolicyRef {
		t.Fatal("wrong independently projected topology")
	}
	t.Log("real routerd accepted signed claim and projected RR/direct topology with finite original expiry")
	observation = samclientconfig.Snapshot{}
	if err := local.Do(ctx, "POST", "/v1/revoke", nil, &observation); err != nil {
		t.Fatal(err)
	}
	sharedMu.Lock()
	published := shared
	sharedMu.Unlock()
	if published.Phase != "revoked" || published.RRSet != nil {
		t.Fatal("withdrawal did not immediately reach the observation consumer")
	}
	if observation.RRSet != nil || observation.Phase != "revoked" {
		t.Fatal("native withdrawal retained topology")
	}
	_, err = client.GetSAMEnrollmentTopology(ctx, controlapi.SAMEnrollmentTopologyGetRequest{Name: "rrs", ClaimRef: "SAMEnrollmentClaim/leaf", ClaimDigest: samenrollment.ClaimDigest(claim), ClaimIdentityDigest: samenrollment.ClientIdentityDigest(claim)})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("independent server did not persist revocation: %v", err)
	}
	t.Log("real routerd stored revocation and rejected the withdrawn identity")
}
