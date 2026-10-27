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

// Identical addresses behind independent Linux routers must produce separate
// BFD/BGP sessions, FIBs and application responses in each routing domain.
func TestDaemonBFDVRFIsolation(t *testing.T) {
	if isolateFixture(t, "TestDaemonBFDVRFIsolation") {
		testDaemonBFDVRFIsolation(t, "ipv4")
	}
}

func TestDaemonBFDVRFIPv6Isolation(t *testing.T) {
	if isolateFixture(t, "TestDaemonBFDVRFIPv6Isolation") {
		testDaemonBFDVRFIsolation(t, "ipv6")
	}
}

func testDaemonBFDVRFIsolation(t *testing.T, family string) {
	t.Helper()
	suffix, learnedPrefix, application := "4", "203.0.113.0/24", "203.0.113.9"
	fibFamily := netlink.FAMILY_V4
	if family == "ipv6" {
		suffix, learnedPrefix, application, fibFamily = "6", "2001:db8:77::/64", "2001:db8:77::9", netlink.FAMILY_V6
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	failures := make(chan error, 8)
	paths := []routedBFDPeer{newRoutedBFDPeer(t, family, "wan0"), newRoutedBFDPeer(t, family, "wan1"), newRoutedBFDPeer(t, family, "wan2")}
	domains := []struct {
		name, vrf, response string
		table               int
	}{{"blue", "blue", "B", 1001}, {"red", "red", "R", 1002}, {"default", "", "D", 254}}
	sessions := []chan *bfdBGPSession{make(chan *bfdBGPSession, 4), make(chan *bfdBGPSession, 4), make(chan *bfdBGPSession, 4)}
	captures := []*healthPBRCaptureState{healthPBRCapture(t, "wan0"), healthPBRCapture(t, "wan1"), healthPBRCapture(t, "wan2")}
	doc := config.Native{}
	for i, path := range paths {
		domain := domains[i]
		vrrpVIPResponders(t, path.remoteNS, suffix, []string{application}, domain.response)
		if domain.vrf != "" {
			path.desired.Interfaces[0].Master = &domain.vrf
			path.desired.Interfaces[1].Name = domain.name + "-src"
			path.desired.Interfaces[1].Master = &domain.vrf
			path.desired.Interfaces = append(path.desired.Interfaces, network.Interface{Name: domain.vrf})
			doc.Network.Links = append(doc.Network.Links, network.Link{Name: domain.vrf, Kind: "vrf", VRFTable: domain.table}, network.Link{Name: domain.name + "-src", Kind: "dummy"})
		}
		path.desired.Routes[0].Table = domain.table
		doc.Network.Interfaces = append(doc.Network.Interfaces, path.desired.Interfaces...)
		doc.Network.Routes = append(doc.Network.Routes, path.desired.Routes...)
		source, remote := path.source, path.remote
		if family == "ipv6" {
			source, remote = "2001:0DB8:0100:0:0:0:0:1", "2001:0DB8:0200::2"
		}
		doc.Services = append(doc.Services,
			serviceconfig.Service{Name: domain.name + "-watch", BFD: &bfd.Config{VRF: domain.vrf, Multihop: true, SourceAddress: source, Peers: []bfd.Peer{{Address: remote, MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}},
			serviceconfig.Service{Name: domain.name + "-bgp", BGP: &serviceconfig.BGP{
				Table: domain.table, RouteMetric: 200,
				Runtime: bgpdaemon.AppliedConfig{
					Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1},
					Peers: map[string]bgpdaemon.AppliedPeer{path.remote: {
						Address: path.remote, ASN: 64513, TimersProfile: "fast", EbgpMultihop: 5,
						ImportPolicy:     bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{learnedPrefix}, NextHopRewrite: "unchanged"},
						ExportPolicyName: "fixture-export", ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: []string{"198.51.100.0/24"}},
					}},
				},
				PeerTransports: map[string]bgpconfig.PeerTransport{path.remote: {LocalAddress: path.source, RemotePort: path.listener.Addr().(*net.TCPAddr).Port, BindInterface: domain.vrf}},
				PeerBFD:        map[string]bgpconfig.BFDReference{path.remote: {Service: domain.name + "-watch", Address: remote}},
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
	doc.Network.Routes = append(doc.Network.Routes, network.Route{Destination: learnedPrefix, Gateway: decoy.gateway, Interface: "wan2", Metric: 777})
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
		if err := f.control.Do(ctx, "GET", "/v1/services/service."+domains[i].name+"-watch", nil, &instance); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(instance.Snapshot, &state); err != nil {
			t.Fatal(err)
		}
		return instance, state
	}
	route := func(i int) bool {
		t.Helper()
		_, prefix, _ := net.ParseCIDR(learnedPrefix)
		routes, err := netlink.RouteListFiltered(fibFamily, &netlink.Route{Dst: prefix, Table: domains[i].table}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
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
		for _, protocol := range []string{"tcp", "udp"} {
			dialer := net.Dialer{Timeout: 300 * time.Millisecond, LocalAddr: &net.TCPAddr{IP: net.ParseIP(paths[i].source)}, Control: func(_, _ string, raw syscall.RawConn) error {
				var result error
				err := raw.Control(func(fd uintptr) {
					result = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, domains[i].vrf)
				})
				if err != nil {
					return err
				}
				return result
			}}
			port := "18093"
			if protocol == "udp" {
				port, dialer.LocalAddr = "18094", &net.UDPAddr{IP: net.ParseIP(paths[i].source)}
			}
			started := time.Now()
			conn, err := dialer.DialContext(ctx, protocol+suffix, net.JoinHostPort(application, port))
			var response [1]byte
			if err == nil {
				_ = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
				if protocol == "udp" {
					_, err = conn.Write([]byte("domain"))
				}
				if err == nil {
					_, err = io.ReadFull(conn, response[:])
				}
				conn.Close()
			}
			if available && (err != nil || response[0] != domains[i].response[0]) || !available && err == nil {
				t.Log("user traffic duration", time.Since(started))
				t.Fatal("routing-domain user traffic", domains[i].name, protocol, available, response, err)
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
			t.Fatal("routing-domain BGP did not establish", domains[i].name, instance, state)
		}
		if err := session.wire.send(2, fixtureUpdate(learnedPrefix, paths[i].remote, false)); err != nil {
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
	for i, path := range paths {
		until(t, 4*time.Second, func() bool { checkPeer(); return path.peer.received.Load() > 0 })
		if path.listener.accepted.Load() != 0 || route(i) {
			t.Fatal("initial BFD Down allowed BGP", domains[i].name)
		}
	}
	defaultSession := connect(2)
	blue, red := connect(0), connect(1)
	redWatch, redBGP, blueBGP := worker("red-watch"), worker("red-bgp"), worker("blue-bgp")
	defaultWatch, defaultBGP := worker("default-watch"), worker("default-bgp")
	original, _ := snapshot(0)
	paths[0].peer.enabled.Store(false)
	withdraw(0, blue)
	traffic(1, true)
	traffic(2, true)
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
	traffic(2, true)
	blue = connect(0)
	fresh, _ := snapshot(0)
	if fresh.Generation <= original.Generation || fresh.ProducerEpoch == original.ProducerEpoch {
		t.Fatal("VRF BFD restart retained old producer", original, fresh)
	}
	for name, before := range map[string]supervisor.ModuleStatus{"red-watch": redWatch, "red-bgp": redBGP, "blue-bgp": blueBGP, "default-watch": defaultWatch, "default-bgp": defaultBGP} {
		if after := worker(name); after.PID != before.PID || after.Generation != before.Generation {
			t.Fatal("VRF outage restarted unrelated worker", name, before, after)
		}
	}
	for i, session := range map[int]*bfdBGPSession{1: red, 2: defaultSession} {
		traffic(i, true)
		select {
		case <-session.closed:
			t.Fatal("blue worker replacement closed unrelated BGP", domains[i].name)
		default:
		}
		if paths[i].listener.accepted.Load() != 1 || !route(i) {
			t.Fatal("unrelated domain reconnected or lost its learned route", domains[i].name)
		}
	}
	beforeFreeze, before := snapshot(2)
	t.Log("pause default BFD", defaultWatch.PID)
	if err := syscall.Kill(defaultWatch.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(defaultWatch.PID, syscall.SIGCONT) })
	until(t, 5*time.Second, func() bool {
		checkPeer()
		select {
		case <-defaultSession.closed:
			return !route(2)
		default:
			return false
		}
	})
	instance, frozen := snapshot(2)
	t.Log("default BFD authority expired; checking unaffected VRFs")
	clock, err := bfd.ReadClock()
	if err != nil || frozen.Peers[0].State != bfd.StateUp || frozen.Peers[0].DeadlineBootNanoseconds > clock.BoottimeNanoseconds || instance.BFD == nil || len(instance.BFD.HealthyPeers) != 0 {
		t.Fatal("frozen default-domain UP did not independently expire", instance, frozen, err)
	}
	for i, session := range []*bfdBGPSession{blue, red} {
		traffic(i, true)
		select {
		case <-session.closed:
			t.Fatal("default-domain source expiry closed VRF BGP", domains[i].name)
		default:
		}
	}
	if err := syscall.Kill(defaultWatch.PID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	connect(2)
	freshDefault, received := snapshot(2)
	if freshDefault.Generation != beforeFreeze.Generation || freshDefault.ProducerEpoch != beforeFreeze.ProducerEpoch || received.Peers[0].LastReceivedBootNanoseconds <= before.Peers[0].LastReceivedBootNanoseconds {
		t.Fatal("default-domain recovery did not use fresh packets from the same worker", beforeFreeze, freshDefault, received)
	}
	if after := worker("default-bgp"); after.PID != defaultBGP.PID || after.Generation != defaultBGP.Generation {
		t.Fatal("default-domain expiry replaced BGP worker", defaultBGP, after)
	}
	for i, capture := range captures {
		verifyMultihopBFDCapture(t, capture, paths[i].source, paths[i].remote)
	}
	t.Log(family, "three routing domains with identical endpoints: routed BFD/BGP, distinct TCP/UDP responses, no main-table fallback, isolated UDP loss/recovery, BFD replacement and frozen-source expiry/recovery")
}
