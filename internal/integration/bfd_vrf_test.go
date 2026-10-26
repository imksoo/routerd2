//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonSinglehopBFDVRF(t *testing.T) {
	if !isolateFixture(t, "TestDaemonSinglehopBFDVRF") {
		return
	}
	peer, _, _ := newDaemonBFDPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	failures := make(chan error, 4)
	go peer.run(ctx, failures)
	blue := "blue"
	doc := config.Native{Network: network.Desired{
		Links:      []network.Link{{Name: blue, Kind: "vrf", VRFTable: 1001}},
		Interfaces: []network.Interface{{Name: blue}, {Name: "wan0", Master: &blue, Addresses: []string{"192.0.2.1/24"}}},
	}, Services: []serviceconfig.Service{{Name: "watch", BFD: &bfd.Config{Interface: "wan0", VRF: blue, SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2", MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}}}}
	f := startFixtureDaemon(t, config.FromNative(doc))
	wait := func(state string) {
		t.Helper()
		until(t, 5*time.Second, func() bool {
			select {
			case err := <-failures:
				t.Fatal("independent single-hop peer", err)
			default:
			}
			var instance runtimeplan.InstanceStatus
			var snapshot bfd.Snapshot
			if err := f.control.Do(ctx, "GET", "/v1/services/service.watch", nil, &instance); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(instance.Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
			return len(snapshot.Peers) == 1 && snapshot.Peers[0].State == state
		})
	}
	peer.enabled.Store(true)
	wait(bfd.StateUp)
	peer.enabled.Store(false)
	wait(bfd.StateDown)
	peer.enabled.Store(true)
	wait(bfd.StateUp)
	t.Log("VRF member-bound single-hop BFD: UDP 3784, TTL 255, direct route, loss and recovery")
}

// Identical addresses behind two independent Linux routers must produce two
// independent BFD/BGP sessions, FIBs and application responses.
func TestDaemonBFDVRFIsolation(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBFDVRFIsolation") {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	failures := make(chan error, 8)
	paths := []routedBFDPeer{newRoutedBFDPeer(t, "ipv4", "wan0"), newRoutedBFDPeer(t, "ipv4", "wan1"), newRoutedBFDPeer(t, "ipv4", "wan2")}
	names := []string{"blue", "red", ""}
	sessions := []chan *bfdBGPSession{make(chan *bfdBGPSession, 4), make(chan *bfdBGPSession, 4)}
	captures := []*healthPBRCaptureState{healthPBRCapture(t, "wan0"), healthPBRCapture(t, "wan1")}
	doc := config.Native{}
	for i, path := range paths[:2] {
		name, table := names[i], 1001+i
		vrrpVIPResponders(t, path.remoteNS, "4", []string{"203.0.113.9"}, []string{"B", "R"}[i])
		path.desired.Interfaces[0].Master = &name
		path.desired.Interfaces[1].Name = name + "-src"
		path.desired.Interfaces[1].Master = &name
		path.desired.Interfaces = append(path.desired.Interfaces, network.Interface{Name: name})
		path.desired.Routes[0].Table = table
		doc.Network.Interfaces = append(doc.Network.Interfaces, path.desired.Interfaces...)
		doc.Network.Routes = append(doc.Network.Routes, path.desired.Routes...)
		doc.Network.Links = append(doc.Network.Links, network.Link{Name: name, Kind: "vrf", VRFTable: table}, network.Link{Name: name + "-src", Kind: "dummy"})
		doc.Services = append(doc.Services,
			serviceconfig.Service{Name: name + "-watch", BFD: &bfd.Config{VRF: name, Multihop: true, SourceAddress: path.source, Peers: []bfd.Peer{{Address: path.remote, MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}},
			serviceconfig.Service{Name: name + "-bgp", BGP: &serviceconfig.BGP{
				Table: table, RouteMetric: 200,
				Runtime: bgpdaemon.AppliedConfig{
					Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: path.source, ListenPort: -1},
					Peers: map[string]bgpdaemon.AppliedPeer{path.remote: {
						Address: path.remote, ASN: 64513, TimersProfile: "fast", EbgpMultihop: 5,
						ImportPolicy:     bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24"}, NextHopRewrite: "unchanged"},
						ExportPolicyName: "fixture-export", ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: []string{"198.51.100.0/24"}},
					}},
				},
				PeerTransports: map[string]bgpconfig.PeerTransport{path.remote: {LocalAddress: path.source, RemotePort: path.listener.Addr().(*net.TCPAddr).Port, BindInterface: name}},
				PeerBFD:        map[string]bgpconfig.BFDReference{path.remote: {Service: name + "-watch"}},
			}},
		)
		go path.peer.run(ctx, failures)
		go func() {
			for {
				wire, err := acceptFixturePeer(path.listener)
				if err != nil {
					if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
						failures <- err
					}
					return
				}
				session := &bfdBGPSession{wire: wire, closed: make(chan struct{})}
				go session.observe(ctx)
				select {
				case sessions[i] <- session:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	// A reachable main-table destination with the same source and peer pair
	// makes accidental fallback observable as a distinct application response.
	decoy := paths[2]
	vrrpVIPResponders(t, decoy.remoteNS, "4", []string{"203.0.113.9"}, "D")
	doc.Network.Interfaces = append(doc.Network.Interfaces, decoy.desired.Interfaces...)
	doc.Network.Routes = append(doc.Network.Routes, decoy.desired.Routes...)
	doc.Network.Routes = append(doc.Network.Routes, network.Route{Destination: "203.0.113.0/24", Gateway: decoy.gateway, Interface: "wan2", Metric: 777})
	if err := config.Validate(config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	f := startFixtureDaemon(t, config.FromNative(doc))
	checkPeer := func() {
		t.Helper()
		select {
		case err := <-failures:
			t.Fatal("independent peer", err)
		default:
		}
	}
	worker := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, m := range status.Modules {
			if m.Name == "service."+name {
				return m
			}
		}
		t.Fatal("missing worker", name)
		return supervisor.ModuleStatus{}
	}
	snapshot := func(i int) (runtimeplan.InstanceStatus, bfd.Snapshot) {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		var state bfd.Snapshot
		if err := f.control.Do(ctx, "GET", "/v1/services/service."+names[i]+"-watch", nil, &instance); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(instance.Snapshot, &state); err != nil {
			t.Fatal(err)
		}
		return instance, state
	}
	route := func(i int) bool {
		t.Helper()
		_, prefix, _ := net.ParseCIDR("203.0.113.0/24")
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: prefix, Table: 1001 + i}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range routes {
			if r.Protocol == network.RouteProtocol && r.Priority == 200 && r.LinkIndex == paths[i].index && r.Gw.Equal(net.ParseIP(paths[i].gateway)) {
				return true
			}
		}
		return false
	}
	traffic := func(i int, available bool) {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			dialer := net.Dialer{Timeout: 300 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: net.ParseIP(paths[i].source)}, Control: func(_, _ string, raw syscall.RawConn) error {
				var result error
				err := raw.Control(func(fd uintptr) {
					result = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, names[i])
				})
				if err != nil {
					return err
				}
				return result
			}}
			port := "18093"
			if protocol == "udp4" {
				port, dialer.LocalAddr = "18094", &net.UDPAddr{IP: net.ParseIP(paths[i].source)}
			}
			conn, err := dialer.DialContext(ctx, protocol, net.JoinHostPort("203.0.113.9", port))
			var response [1]byte
			if err == nil {
				_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
				if protocol == "udp4" {
					_, err = conn.Write([]byte("domain"))
				}
				if err == nil {
					_, err = io.ReadFull(conn, response[:])
				}
				conn.Close()
			}
			if available && (err != nil || response[0] != []byte{'B', 'R', 'D'}[i]) || !available && err == nil {
				t.Fatal("VRF user traffic", names[i], protocol, available, response, err)
			}
		}
	}
	connect := func(i int) *bfdBGPSession {
		t.Helper()
		paths[i].peer.enabled.Store(true)
		var session *bfdBGPSession
		select {
		case session = <-sessions[i]:
		case err := <-failures:
			t.Fatal(err)
		case <-time.After(12 * time.Second):
			instance, state := snapshot(i)
			t.Fatal("VRF BGP did not establish", names[i], instance, state)
		}
		if err := session.wire.send(2, fixtureUpdate("203.0.113.0/24", paths[i].remote, false)); err != nil {
			t.Fatal(err)
		}
		until(t, 6*time.Second, func() bool { checkPeer(); return route(i) })
		traffic(i, true)
		return session
	}
	withdraw := func(i int, session *bfdBGPSession) {
		t.Helper()
		until(t, 5*time.Second, func() bool {
			checkPeer()
			select {
			case <-session.closed:
				return !route(i)
			default:
				return false
			}
		})
		traffic(i, false)
	}
	traffic(2, true)
	for i, path := range paths[:2] {
		until(t, 4*time.Second, func() bool { checkPeer(); return path.peer.received.Load() > 0 })
		if path.listener.accepted.Load() != 0 || route(i) {
			t.Fatal("initial BFD Down allowed VRF BGP", names[i])
		}
	}
	blue, red := connect(0), connect(1)
	redWatch, redBGP, blueBGP := worker("red-watch"), worker("red-bgp"), worker("blue-bgp")
	original, _ := snapshot(0)
	paths[0].peer.enabled.Store(false)
	withdraw(0, blue)
	traffic(1, true)
	select {
	case <-red.closed:
		t.Fatal("blue BFD loss closed red BGP")
	default:
	}
	blue = connect(0)
	blueWatch := worker("blue-watch")
	if err := syscall.Kill(blueWatch.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	withdraw(0, blue)
	traffic(1, true)
	connect(0)
	fresh, _ := snapshot(0)
	if fresh.Generation <= original.Generation || fresh.ProducerEpoch == original.ProducerEpoch {
		t.Fatal("VRF BFD restart retained old producer", original, fresh)
	}
	for name, before := range map[string]supervisor.ModuleStatus{"red-watch": redWatch, "red-bgp": redBGP, "blue-bgp": blueBGP} {
		if after := worker(name); after.PID != before.PID || after.Generation != before.Generation {
			t.Fatal("VRF outage restarted unrelated worker", name, before, after)
		}
	}
	traffic(1, true)
	select {
	case <-red.closed:
		t.Fatal("blue worker replacement closed red BGP")
	default:
	}
	if paths[1].listener.accepted.Load() != 1 {
		t.Fatal("red BGP reconnected during an unrelated VRF outage")
	}
	if fixtureRoute(t, "203.0.113.0/24", "", 0) {
		t.Fatal("VRF BGP route escaped into the main table")
	}
	for i, capture := range captures {
		verifyMultihopBFDCapture(t, capture, paths[i].source, paths[i].remote)
	}
	t.Log("two VRFs and default domain with identical endpoints: routed BFD/BGP, distinct TCP/UDP responses, no main-table fallback, isolated UDP loss/recovery and fresh BFD worker replacement")
}
