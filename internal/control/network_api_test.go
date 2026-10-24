package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
)

type observedNetwork struct {
	snapshot network.Snapshot
	called   int
}

func (n *observedNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	return n.snapshot, nil
}
func (n *observedNetwork) Apply(_ context.Context, revision uint64, _ network.Desired) (network.Snapshot, error) {
	n.called++
	n.snapshot.Revision, n.snapshot.AttemptedRevision, n.snapshot.Status = revision, revision, "applied"
	return n.snapshot, nil
}

func TestNetworkApplyAcknowledgementDoesNotEchoLargeObservations(t *testing.T) {
	n := &observedNetwork{}
	// Kernel observations can exceed the request limit independently of the
	// requested delta. The native writer has already completed at this point.
	for i := 0; i < 16; i++ {
		device := network.WireGuardState{Interface: fmt.Sprintf("wg%d", i)}
		for j := 0; j < network.MaxWireGuardPeers; j++ {
			device.Peers = append(device.Peers, network.WireGuardPeerState{
				PublicKey: "QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI=", Endpoint: "[2001:db8:1234:5678:abcd:1234:5678:abcd]:51820",
				AllowedIPs: []string{fmt.Sprintf("2001:db8:%x:%x::/64", i, j)},
			})
		}
		n.snapshot.WireGuard = append(n.snapshot.WireGuard, device)
	}
	full, err := json.Marshal(n.snapshot)
	if err != nil || len(full) <= localapi.MaxBody {
		t.Fatalf("fixture did not exceed the API bound: %d, %v", len(full), err)
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(`{"revision":7,"desired":{}}`))
	networkHandler(n).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || n.called != 1 || recorder.Body.Len() > 512 {
		t.Fatalf("apply acknowledgement status=%d called=%d bytes=%d", recorder.Code, n.called, recorder.Body.Len())
	}
	var result ApplyResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || result.Revision != 7 || result.AttemptedRevision != 7 || result.Status != "applied" {
		t.Fatalf("invalid acknowledgement: %+v, %v", result, err)
	}
	if strings.Contains(recorder.Body.String(), "public_key") || strings.Contains(recorder.Body.String(), "desired") {
		t.Fatal("acknowledgement echoed observations or desired configuration")
	}
}
