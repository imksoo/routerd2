package samclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/controlapi"
	"github.com/imksoo/routerd/pkg/samenrollment"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/samclientconfig"
)

// The peer uses routerd's HTTP server codec, independently of the native
// worker's client, while the callbacks define this test's admission responses.
type enrollmentPeer struct {
	mu                                        sync.Mutex
	claim                                     api.SAMEnrollmentClaimSpec
	admitted, revoked, unavailable, badDirect bool
	submits, revokes                          int
}

func (p *enrollmentPeer) handler() http.Handler {
	h := controlapi.Handler{
		SubmitSAMEnrollmentClaim: func(r *http.Request, request controlapi.SAMEnrollmentClaimSubmitRequest) (*controlapi.SAMEnrollmentClaimSubmitResult, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			claim, err := request.Claim.SAMEnrollmentClaimSpec()
			if err != nil {
				return nil, err
			}
			if claim.JoinHMAC != samenrollment.JoinHMAC([]byte("test-join-token"), claim) {
				return nil, fmt.Errorf("%w: invalid join signature", controlapi.ErrBadRequest)
			}
			if p.revoked {
				return nil, fmt.Errorf("%w: identity revoked", controlapi.ErrBadRequest)
			}
			p.claim = claim
			p.admitted = true
			p.submits++
			now := time.Now().UTC()
			result := controlapi.NewSAMEnrollmentClaimSubmitResult("SAMEnrollmentClaim/leaf", "SAMEnrollmentClaim/leaf", 1, now, now.Add(3*time.Second))
			return &result, nil
		},
		GetSAMEnrollmentTopology: func(r *http.Request, request controlapi.SAMEnrollmentTopologyGetRequest) (*controlapi.SAMEnrollmentTopologyGetResult, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.revoked {
				return nil, fmt.Errorf("%w: identity revoked", controlapi.ErrBadRequest)
			}
			if !p.admitted {
				return nil, fmt.Errorf("%w: accepted SAMEnrollmentClaim/leaf %s", controlapi.ErrBadRequest, controlapi.SAMEnrollmentTopologyIdentityAbsentMessage)
			}
			if request.ClaimIdentityDigest != samenrollment.ClientIdentityDigest(p.claim) || request.ClaimDigest != samenrollment.ClaimDigest(p.claim) {
				return nil, errors.New("client omitted current claim identity")
			}
			rr := api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMRRSet"}, Metadata: api.ObjectMeta{Name: "rrs"}, Spec: api.SAMRRSetSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/leaves", Nodes: []api.SAMNodeSpec{{NodeRef: "rr-a", SAMEndpoint: "192.0.2.1", RouteReflector: true}}}}
			group := api.Resource{TypeMeta: api.TypeMeta{APIVersion: api.MobilityAPIVersion, Kind: "SAMPeerGroup"}, Metadata: api.ObjectMeta{Name: "direct"}, Spec: api.SAMPeerGroupSpec{EnrollmentPolicyRef: "SAMEnrollmentPolicy/leaves", TransportFingerprint: "test-ipip", Nodes: []api.SAMNodeSpec{{NodeRef: "leaf-b", SAMEndpoint: "192.0.2.3"}}, OwnedPrefixesByNode: map[string][]string{"leaf-b": {"198.51.100.3/32"}}}}
			result := controlapi.NewSAMEnrollmentTopologyGetResult("rrs", samenrollment.ClaimDigest(p.claim), rr, &group)
			if p.badDirect {
				result.ClaimDigest = "different-identity"
			}
			return &result, nil
		},
		RevokeSAMEnrollmentClaim: func(r *http.Request, request controlapi.SAMEnrollmentClaimRevokeRequest) (*controlapi.SAMEnrollmentClaimRevokeResult, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.revoked = true
			p.revokes++
			now := time.Now().UTC()
			result := controlapi.NewSAMEnrollmentClaimRevokeResult("SAMEnrollmentClaim/leaf", "SAMEnrollmentClaim/leaf", 2, now, now, "operator withdrawal")
			return &result, nil
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-control-token" {
			http.Error(w, "token required", http.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		unavailable := p.unavailable
		p.mu.Unlock()
		if unavailable {
			http.Error(w, "RR unavailable", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func TestEnrollmentHTTPRenewalExpiryAndWithdrawal(t *testing.T) {
	var peers [2]enrollmentPeer
	a := httptest.NewServer(peers[0].handler())
	defer a.Close()
	b := httptest.NewServer(peers[1].handler())
	defer b.Close()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("test-control-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	claim := api.SAMEnrollmentClaimSpec{PolicyRef: "SAMEnrollmentPolicy/leaves", RRSetRef: "SAMRRSet/rrs", LeafID: "leaf-a", TunnelAddress: "10.255.0.2", Endpoint: "192.0.2.2", DirectMesh: true, JoinNonce: "test-nonce", JoinTimestamp: time.Now().UTC().Format(time.RFC3339)}
	claim.JoinHMAC = samenrollment.JoinHMAC([]byte("test-join-token"), claim)
	cfg := samclientconfig.Config{Node: samclientconfig.Node{ClaimName: "leaf", Claim: claim, Endpoints: []string{a.URL, b.URL}, TokenFile: token, RefreshBeforeSeconds: 1, RetryMinSeconds: 1, RetryMaxSeconds: 2}, SocketPath: filepath.Join(dir, "sam.sock"), StatePath: filepath.Join(dir, "withdrawn")}
	var observedMu sync.Mutex
	var observed samclientconfig.Snapshot
	start := func() func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			done <- Run(ctx, cfg, func(_ context.Context, s samclientconfig.Snapshot) error {
				observedMu.Lock()
				observed = s
				observedMu.Unlock()
				return nil
			}, func() { close(ready) })
		}()
		select {
		case <-ready:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("service not ready")
		}
		return func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("service did not stop")
			}
		}
	}
	stop := start()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	local := localapi.NewClient(cfg.SocketPath)
	get := func() samclientconfig.Snapshot {
		var s samclientconfig.Snapshot
		if err := local.Do(context.Background(), "GET", "/v1/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	wait := func(check func() bool) {
		t.Helper()
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("enrollment condition not reached: %+v", get())
	}
	wait(func() bool { return get().PeerGroup != nil })
	first := get()
	if err := samclientconfig.ValidateSnapshot(first); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return get().ExpiresAt.After(first.ExpiresAt) })
	peers[1].mu.Lock()
	peers[1].badDirect = true
	peers[1].mu.Unlock()
	wait(func() bool { s := get(); return s.RRSet != nil && s.PeerGroup == nil })
	for i := range peers {
		peers[i].mu.Lock()
		peers[i].unavailable = true
		peers[i].mu.Unlock()
	}
	beforeFailure := get().ExpiresAt
	wait(func() bool { s := get(); return s.Phase == "expired" && s.RRSet == nil })
	if !get().ExpiresAt.Equal(beforeFailure) {
		t.Fatal("failed requests extended admission lifetime")
	}
	for i := range peers {
		peers[i].mu.Lock()
		peers[i].unavailable = false
		peers[i].badDirect = false
		peers[i].mu.Unlock()
	}
	wait(func() bool { return get().PeerGroup != nil })
	var revoked samclientconfig.Snapshot
	if err := local.Do(context.Background(), "POST", "/v1/revoke", nil, &revoked); err != nil {
		t.Fatal(err)
	}
	observedMu.Lock()
	published := observed
	observedMu.Unlock()
	if published.Phase != "revoked" || published.RRSet != nil {
		t.Fatal("HTTP withdrawal succeeded before its shared observation")
	}
	if revoked.Phase != "revoked" || revoked.RRSet != nil {
		t.Fatal("withdrawal retained topology")
	}
	stop()
	stop = nil
	var submits [2]int
	for i := range peers {
		peers[i].mu.Lock()
		submits[i] = peers[i].submits
		if peers[i].revokes == 0 {
			t.Fatal("missing remote revoke")
		}
		peers[i].mu.Unlock()
	}
	stop = start()
	wait(func() bool { return get().Phase == "revoked" })
	for i := range peers {
		peers[i].mu.Lock()
		if peers[i].submits != submits[i] {
			t.Fatal("restart revived withdrawn identity")
		}
		peers[i].mu.Unlock()
	}
}
