package runtimeplan

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

func TestBGPExpiryKeepsOriginalObservationAndRefusesResequencedReplay(t *testing.T) {
	doc := bfdBGPDocument()
	doc.Services = doc.Services[1:]
	doc.Services[0].BGP.PeerBFD = nil
	r, network, _ := fixture(t, doc)
	sample := observedBGPRoutes()
	u := update(t, r, "service.router", 1, sample)
	ctx := context.Background()
	if err := observeAndProject(ctx, r, u); err != nil || len(network.applied.Routes) != 1 {
		t.Fatal("fresh original observation did not install its route", err)
	}
	time.Sleep(time.Until(sample.ObservedAt.Add(bgpconfig.ObservationLifetime + 50*time.Millisecond)))
	r.mu.Lock()
	effective := r.observationsLocked()
	r.mu.Unlock()
	if !bytes.Equal(effective[u.Name].Snapshot, u.Snapshot) {
		t.Fatal("expiry rewrote the accepted protocol observation")
	}
	if err := r.reconcileDynamic(ctx); err != nil || len(network.applied.Routes) != 0 {
		t.Fatal("expired original observation retained its FIB route", err)
	}
	status := r.State()
	if len(status.Instances) != 1 || !status.Instances[0].OwnershipExpired {
		t.Fatal("original boot expiry was not visible", status)
	}
	var original bgpconfig.Snapshot
	if err := json.Unmarshal(status.Instances[0].Snapshot, &original); err != nil || !original.Active || original.ExpiresNanos != sample.ExpiresNanos {
		t.Fatal("status changed the publisher's original observation", err, original)
	}
	u.Sequence++
	if err := r.Observe(ctx, u); err == nil {
		t.Fatal("a higher transport sequence revived an expired BGP observation")
	}
}
