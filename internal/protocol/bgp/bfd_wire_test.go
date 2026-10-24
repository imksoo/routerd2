//go:build linux

package bgp

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
)

// This library-level test supplies finite, synthetic BFD projections and an
// independent real BGP wire speaker. The appliance integration fixture also
// verifies the actual UDP BFD producer and exact interface/address binding.
func TestBFDWireExpiryClosesActualTCPWhileRouteEffectsBlocked(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	peerReady, peerErr := make(chan *wirePeer, 1), make(chan error, 1)
	go func() {
		p, err := acceptWirePeer(l)
		if err != nil {
			peerErr <- err
			return
		}
		peerReady <- p
	}()
	cfg := moduleConfig(t)
	cfg.Runtime.Peers = map[string]bgpdaemon.AppliedPeer{"127.0.0.1": {Address: "127.0.0.1", ASN: 64513, TimersProfile: "fast", ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24"}, NextHopRewrite: "unchanged"}}}
	cfg.PeerTransports = map[string]PeerTransport{"127.0.0.1": {LocalAddress: "127.0.0.1", RemotePort: l.Addr().(*net.TCPAddr).Port}}
	cfg.PeerBFD = map[string]bgpconfig.BFDReference{"127.0.0.1": {Service: "health", Address: gatePeer}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan BFDUpdate, 1)
	capture := &snapshotCapture{changed: make(chan struct{}, 1)}
	entered, release := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if s.Active && len(s.Routes) != 0 {
			blockOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return capture.Update(ctx, s)
	})
	done, ready := make(chan error, 1), make(chan struct{})
	go func() {
		done <- RunWithBFD(ctx, cfg, strings.Repeat("a", 64), 9, updates, effects, func() { close(ready) })
	}()
	finished := false
	defer func() {
		unblock()
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Error("BFD-gated BGP did not stop")
			}
		}
	}()
	select {
	case <-ready:
	case err := <-done:
		finished = true
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial down ownership did not become ready")
	}
	select {
	case p := <-peerReady:
		_ = p.conn.Close()
		t.Fatal("BGP connected before its first BFD grant")
	case err := <-peerErr:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	initial := capture.get()
	if len(initial.Routes) != 0 || len(initial.Peers) != 1 || initial.Peers[0].AdminState != "ADMIN_STATE_DOWN" || initial.BFD["127.0.0.1"].Enabled {
		t.Fatalf("initial peer was not gated: %+v", initial)
	}

	var replay atomic.Bool
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var grant bgpconfig.BFDGrant
		for {
			clock, err := bfd.ReadClock()
			if err != nil {
				peerErr <- err
				return
			}
			if !replay.Load() || grant.Sequence == 0 {
				sequence := grant.Sequence + 1
				grant = gateGrant(clock.BoottimeNanoseconds, sequence, 0)
				grant.BootID = clock.BootID
				grant.DeadlineBootNanoseconds = clock.BoottimeNanoseconds + int64(250*time.Millisecond)
			}
			update := BFDUpdate{Projection: bgpconfig.BFDProjection{ConfigDigest: strings.Repeat("a", 64), Generation: 9, Peers: map[string]bgpconfig.BFDGrant{"127.0.0.1": grant}}, Received: clock, Available: true}
			select {
			case updates <- update:
			case <-ctx.Done():
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-feedDone }()
	var peer *wirePeer
	select {
	case peer = <-peerReady:
	case err := <-peerErr:
		t.Fatal(err)
	case err := <-done:
		finished = true
		t.Fatal(err)
	case <-time.After(7 * time.Second):
		t.Fatal("healthy BFD did not establish actual BGP TCP")
	}
	defer peer.conn.Close()
	closed, advertised := make(chan struct{}), make(chan struct{})
	var advertisementOnce sync.Once
	go func() {
		defer close(closed)
		for {
			typ, body, err := readWireMessage(peer.conn)
			if err != nil {
				return
			}
			if typ == 2 && bytes.Contains(body, wireNLRI("10.20.0.0/24")) {
				advertisementOnce.Do(func() { close(advertised) })
			}
		}
	}()
	select {
	case <-advertised:
	case <-time.After(3 * time.Second):
		t.Fatal("actual BGP speaker did not receive the configured advertisement")
	}
	if err := peer.send(2, wireUpdate("203.0.113.0/24", "127.0.0.1", false)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("learned route did not reach the blocked ownership effect")
	}
	// Keep HTTP control observations fresh while replaying the last wire
	// receipt. Only the finite source deadline can close this live TCP session.
	replay.Store(true)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("expired BFD could not close actual TCP while route effects were blocked")
	}
	unblock()
	waitSnapshot(t, capture, func(s Snapshot) bool {
		return s.Active && len(s.Routes) == 0 && len(s.Peers) == 1 && s.Peers[0].AdminState == "ADMIN_STATE_DOWN" && !s.BFD["127.0.0.1"].Enabled
	})
	cancel()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("native BFD-gated BGP exceeded shutdown bound")
	}
}
