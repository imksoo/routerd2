package samadmission

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/samadmissionconfig"
	"github.com/imksoo/routerd2/internal/protocol/samclient"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

func TestAdmissionWithExistingClients(t *testing.T) {
	cfg := admissionFixture(t)
	dir := filepath.Dir(cfg.StatePath)
	token, address := cfg.TokenFile, cfg.Listen.String()
	start := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() { done <- Run(ctx, cfg, func() { close(ready) }) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("admission did not listen")
		}
		return func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("admission did not stop")
			}
		}
	}
	stop := start()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	ctx := context.Background()
	client := controlapi.NewHTTPClient("http://" + address).WithBearerToken("fixture-control")
	claim := func(name, ip string) api.Resource {
		spec := api.SAMEnrollmentClaimSpec{PolicyRef: "SAMEnrollmentPolicy/leaves", RRSetRef: "SAMRRSet/rrs", LeafID: name, JoinNonce: name + "-one", JoinTimestamp: time.Now().UTC().Format(time.RFC3339Nano), JoinAudience: "fixture", TunnelAddress: "10.255.0." + ip, Endpoint: "192.0.2." + ip, DirectMesh: true, Mobility: api.SAMEnrollmentClaimMobilitySpec{OwnedAddresses: []string{"198.51.100." + ip + "/32"}}}
		spec.JoinHMAC = samenrollment.JoinHMAC([]byte("fixture-join"), spec)
		return api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMEnrollmentClaim"}, Metadata: api.ObjectMeta{Name: name}, Spec: spec}
	}
	a := claim("leaf-a", "2")
	as, _ := a.SAMEnrollmentClaimSpec()
	if _, err := controlapi.NewHTTPClient("http://"+address).SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: a}); err == nil {
		t.Fatal("missing bearer accepted")
	}
	denied := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}}).DialContext}}
	response, err := denied.Get("http://" + address + controlapi.Prefix + "/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("configured source boundary was not applied")
	}
	bad := a
	badSpec := as
	badSpec.JoinHMAC = "incorrect"
	bad.Spec = badSpec
	if _, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: bad}); err == nil {
		t.Fatal("invalid join signature accepted")
	}
	receipt, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: a})
	if err != nil {
		t.Fatal(err)
	}
	joined, _ := samenrollment.ParseTime(as.JoinTimestamp)
	if !receipt.ExpiresAt.Equal(joined.Add(30 * time.Second)) {
		t.Fatal("receipt outlives original signed authority")
	}
	b := claim("leaf-b", "3")
	bs, _ := b.SAMEnrollmentClaimSpec()
	bs.JoinNonce = as.JoinNonce
	bs.JoinHMAC = samenrollment.JoinHMAC([]byte("fixture-join"), bs)
	b.Spec = bs
	if _, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: b}); err == nil {
		t.Fatal("another leaf reused an admitted nonce")
	}
	b = claim("leaf-b", "3")
	bs, _ = b.SAMEnrollmentClaimSpec()
	bs.ExpiresAt = time.Now().Add(1500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	b.Spec = bs
	short, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: b})
	if err != nil {
		t.Fatal(err)
	}
	request := controlapi.SAMEnrollmentTopologyGetRequest{Name: "rrs", ClaimRef: "SAMEnrollmentClaim/leaf-a", ClaimDigest: samenrollment.ClaimDigest(as), ClaimIdentityDigest: samenrollment.ClientIdentityDigest(as)}
	topology, err := client.GetSAMEnrollmentTopology(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	group, err := topology.PeerGroup.SAMPeerGroupSpec()
	if err != nil || len(group.Nodes) != 1 || group.Nodes[0].NodeRef != "leaf-b" {
		t.Fatalf("missing admitted direct peer: %v", err)
	}
	again, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: b})
	if err != nil || !again.ExpiresAt.Equal(short.ExpiresAt) {
		t.Fatal("repeat request extended original expiry", err)
	}
	time.Sleep(time.Until(short.ExpiresAt) + 30*time.Millisecond)
	topology, err = client.GetSAMEnrollmentTopology(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	group, _ = topology.PeerGroup.SAMPeerGroupSpec()
	if len(group.Nodes) != 0 {
		t.Fatal("expired peer retained")
	}
	if _, err := client.RevokeSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimRevokeRequest{Name: "leaf-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSAMEnrollmentTopology(ctx, request); err == nil {
		t.Fatal("revoked claim received topology")
	}
	rotated := as
	rotated.JoinNonce = "leaf-a-two"
	rotated.JoinTimestamp = time.Now().UTC().Format(time.RFC3339Nano)
	rotated.JoinHMAC = samenrollment.JoinHMAC([]byte("fixture-join"), rotated)
	a.Spec = rotated
	if _, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: a}); err != nil {
		t.Fatal(err)
	}
	stop()
	stop = nil
	stop = start()
	a.Spec = as
	if _, err := client.SubmitSAMEnrollmentClaim(ctx, controlapi.SAMEnrollmentClaimSubmitRequest{Claim: a}); err == nil {
		t.Fatal("restart/rotation revived revoked identity")
	}
	// Existing native client uses the unchanged routerd HTTP codec against this
	// real listener. No test implementation supplies its admission responses.
	c := claim("leaf-c", "4")
	cs, _ := c.SAMEnrollmentClaimSpec()
	cc := samclientconfig.Config{Node: samclientconfig.Node{ClaimName: "leaf-c", Claim: cs, Endpoints: []string{"http://" + address}, TokenFile: token, RefreshBeforeSeconds: 5, RetryMinSeconds: 1, RetryMaxSeconds: 2}, SocketPath: filepath.Join(dir, "leaf.sock"), StatePath: filepath.Join(dir, "leaf-revoked")}
	clientCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- samclient.Run(clientCtx, cc, nil, func() { close(ready) }) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("native client not ready")
	}
	local := localapi.NewClient(cc.SocketPath)
	until := time.Now().Add(5 * time.Second)
	for {
		var observed samclientconfig.Snapshot
		if err := local.Do(ctx, "GET", "/v1/status", nil, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.RRSet != nil && observed.PeerGroup != nil {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("client did not obtain topology: %+v", observed)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var withdrawn samclientconfig.Snapshot
	if err := local.Do(ctx, "POST", "/v1/revoke", nil, &withdrawn); err != nil {
		t.Fatal(err)
	}
	if withdrawn.RRSet != nil || withdrawn.Phase != "revoked" {
		t.Fatal("client kept withdrawn topology")
	}
	replay := controlapi.SAMEnrollmentClaimSubmitRequest{Claim: c}
	_, err = client.SubmitSAMEnrollmentClaim(ctx, replay)
	var apiErr *controlapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatal("withdrawn client was accepted again", err)
	}
	data, err := os.ReadFile(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted savedState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Revoked) != 2 {
		t.Fatal("revoked identities were lost during replacement")
	}
	t.Log("old HTTP client and native SAM client passed signed admission, CIDR/token/HMAC/nonce enforcement, expiry, withdrawal and restart replay protection")
}

func admissionFixture(t *testing.T) samadmissionconfig.Config {
	t.Helper()
	dir := t.TempDir()
	join := filepath.Join(dir, "join")
	token := filepath.Join(dir, "token")
	for path, value := range map[string]string{join: "fixture-join", token: "fixture-control"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	cfg := samadmissionconfig.Config{Node: samadmissionconfig.Node{Listen: netip.MustParseAddrPort(address), AllowCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, TokenFile: token, Policies: []samadmissionconfig.Policy{{Name: "leaves", Spec: api.SAMEnrollmentPolicySpec{RRSetRef: "SAMRRSet/rrs", JoinTokenFrom: api.SecretValueSourceSpec{File: join}, JoinAudience: "fixture", AllowedLeafIDs: api.SAMEnrollmentLeafIDPolicySpec{Pattern: "^leaf-"}, TunnelAddressPrefixes: []string{"10.255.0.0/24"}, EndpointPrefixes: []string{"192.0.2.0/24"}, MobilityPrefixes: []string{"198.51.100.0/24"}, DirectMesh: api.SAMEnrollmentDirectMeshSpec{PeerGroupRef: "SAMPeerGroup/direct"}, TTL: "30s"}, RRSet: api.SAMRRSetSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/leaves", Nodes: []api.SAMNodeSpec{{NodeRef: "rr-a", RouteReflector: true, SAMEndpoint: "192.0.2.1"}}}, TransportFingerprint: "fixture-ipip"}}}, StatePath: filepath.Join(dir, "admissions.json")}

	return cfg
}
