//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Both routerd ELFs run in the verified disposable namespace. Independent UDP
// BFD and TCP BGP speakers live behind a veth in another namespace. The test
// never invokes a host network command, shell, FRR, bfdd, or the BFD machine.
func TestDaemonBFDControlsBGPSessionAndExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBFDControlsBGPSessionAndExpiry") {
		return
	}
	peer, listener, index := newDaemonBFDPeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	peerErrors := make(chan error, 4)
	go peer.run(ctx, peerErrors)
	sessions := make(chan *bfdBGPSession, 4)
	go func() {
		for {
			wire, err := acceptFixturePeer(listener)
			if err != nil {
				if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
					select {
					case peerErrors <- err:
					default:
					}
				}
				return
			}
			session := &bfdBGPSession{wire: wire, closed: make(chan struct{})}
			go session.observe(ctx)
			select {
			case sessions <- session:
			case <-ctx.Done():
				_ = wire.conn.Close()
				return
			}
		}
	}()
	bgp := serviceconfig.BGP{
		Runtime: bgpdaemon.AppliedConfig{
			Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1},
			Peers: map[string]bgpdaemon.AppliedPeer{"192.0.2.2": {
				Address: "192.0.2.2", ASN: 64513, TimersProfile: "fast",
				ImportPolicy:     bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24"}, NextHopRewrite: "unchanged"},
				ExportPolicyName: "bfd-fixture-export", ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: []string{"198.51.100.0/24"}},
			}},
			Paths: []bgpdaemon.AppliedPath{{Source: "StaticRoute/bfd-fixture", Prefix: "198.51.100.0/24", Attrs: bgpdaemon.AppliedPathAttrs{NextHop: "192.0.2.1"}}},
		},
		PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", RemotePort: listener.Addr().(*net.TCPAddr).Port, BindInterface: "wan0"}},
		PeerBFD:        map[string]bgpconfig.BFDReference{"192.0.2.2": {Service: "link-watch"}},
		RouteMetric:    200,
	}
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}}}, Services: []serviceconfig.Service{
		{Name: "link-watch", BFD: &bfd.Config{Interface: "wan0", SourceAddress: "192.0.2.1", Peers: []bfd.Peer{{Address: "192.0.2.2", MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}},
		{Name: "edge", BGP: &bgp},
	}}
	if err := config.Validate(config.FromNative(doc)); err != nil {
		t.Fatal(err)
	}
	fixture := startFixtureDaemon(t, config.FromNative(doc))
	serviceStatus := func() (runtimeplan.InstanceStatus, bfd.Snapshot) {
		t.Helper()
		call, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		var instance runtimeplan.InstanceStatus
		var snapshot bfd.Snapshot
		if err := fixture.control.Do(call, "GET", "/v1/services/service.link-watch", nil, &instance); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(instance.Snapshot, &snapshot); err != nil || len(snapshot.Peers) != 1 {
			t.Fatal("missing real BFD service snapshot", err, string(instance.Snapshot))
		}
		return instance, snapshot
	}
	moduleStatus := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := fixture.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == name && module.Ready && module.PID > 0 {
				return module
			}
		}
		t.Fatal("missing ready worker", name)
		return supervisor.ModuleStatus{}
	}
	modulePID := func(name string) int {
		t.Helper()
		return moduleStatus(name).PID
	}
	checkPeerError := func() {
		t.Helper()
		select {
		case err := <-peerErrors:
			t.Fatal("independent peer:", err)
		default:
		}
	}
	// Merely starting a BGP worker must not open a transport before BFD grants
	// health. No TCP connection means no OPEN, UPDATE, or advertisement can leak.
	until(t, 5*time.Second, func() bool {
		_, snapshot := serviceStatus()
		return snapshot.Active && snapshot.Peers[0].State == bfd.StateDown && peer.received.Load() > 0
	})
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		checkPeerError()
		if listener.accepted.Load() != 0 || fixtureRoute(t, "203.0.113.0/24", "", 0) {
			t.Fatal("initial BFD Down permitted a BGP connection or learned route")
		}
		time.Sleep(25 * time.Millisecond)
	}
	bgpPID, bfdPID := modulePID("service.edge"), modulePID("service.link-watch")
	t.Log("BFD initially Down: no TCP connection or advertisement")
	connect := func() *bfdBGPSession {
		t.Helper()
		peer.enabled.Store(true)
		until(t, 6*time.Second, func() bool {
			checkPeerError()
			instance, snapshot := serviceStatus()
			return snapshot.Peers[0].State == bfd.StateUp && instance.BFD != nil && len(instance.BFD.HealthyPeers) == 1
		})
		var session *bfdBGPSession
		select {
		case session = <-sessions:
		case err := <-peerErrors:
			t.Fatal(err)
		case <-time.After(12 * time.Second):
			var router runtimeplan.InstanceStatus
			_ = fixture.control.Do(ctx, "GET", "/v1/services/service.edge", nil, &router)
			_, source := serviceStatus()
			sourceBytes, _ := json.Marshal(source)
			t.Logf("BFD recovery source=%s BGP=%s", sourceBytes, router.Snapshot)
			t.Fatal("healthy BFD did not enable the independent BGP session")
		}
		if err := session.wire.send(2, fixtureUpdate("203.0.113.0/24", "192.0.2.2", false)); err != nil {
			t.Fatal(err)
		}
		until(t, 6*time.Second, func() bool {
			checkPeerError()
			return fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", index) && session.advertised.Load() && session.keepalives.Load() > 0
		})
		if modulePID("service.edge") != bgpPID {
			t.Fatal("BFD recovery restarted the BGP worker instead of controlling its session")
		}
		return session
	}
	waitWithdrawal := func(session *bfdBGPSession) {
		t.Helper()
		until(t, 5*time.Second, func() bool {
			checkPeerError()
			select {
			case <-session.closed:
				return !fixtureRoute(t, "203.0.113.0/24", "", 0)
			default:
				return false
			}
		})
	}
	first := connect()
	t.Log("BFD Up: BGP established, outbound advertisement observed, learned route installed")
	// A short control delay must not replace valid original wire evidence with
	// a second, shorter transport lease. UDP BFD and TCP keepalives keep running.
	var beforePause bfd.Snapshot
	until(t, 3*time.Second, func() bool {
		_, beforePause = serviceStatus()
		clock, err := bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		return beforePause.Peers[0].State == bfd.StateUp && beforePause.Peers[0].DeadlineBootNanoseconds > clock.BoottimeNanoseconds+int64(700*time.Millisecond)
	})
	controlPID := modulePID("control")
	if err := syscall.Kill(controlPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(controlPID, syscall.SIGCONT)
	time.Sleep(350 * time.Millisecond) // exceeds one real 200 ms projection request
	currentClock, err := bfd.ReadClock()
	if err != nil || currentClock.BoottimeNanoseconds >= beforePause.Peers[0].DeadlineBootNanoseconds {
		t.Fatal("fixture exhausted the original BFD budget", err)
	}
	select {
	case <-first.closed:
		t.Fatal("one delayed control request closed BGP while original BFD evidence remained current")
	default:
	}
	if err := syscall.Kill(controlPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, time.Second, func() bool {
		checkPeerError()
		_, live := serviceStatus()
		return live.Peers[0].LastReceivedBootNanoseconds > beforePause.Peers[0].LastReceivedBootNanoseconds
	})
	select {
	case <-first.closed:
		t.Fatal("queued transport failure reset BGP after fresh BFD recovery")
	default:
	}
	if listener.accepted.Load() != 1 || !fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", index) {
		t.Fatal("short control delay replaced the BGP session or withdrew its route")
	}
	t.Log("350 ms control pause exceeded the projection RPC timeout; original BFD budget preserved the same BGP TCP session and route")
	keepalives := first.keepalives.Load()
	peer.enabled.Store(false) // Only UDP transmission stops; TCP keepalives continue.
	waitWithdrawal(first)
	if first.keepalives.Load() <= keepalives {
		t.Fatal("TCP peer did not remain active during the BFD-only outage")
	}
	until(t, 2*time.Second, func() bool {
		instance, snapshot := serviceStatus()
		return snapshot.Peers[0].State == bfd.StateDown && instance.BFD != nil && len(instance.BFD.HealthyPeers) == 0
	})
	instance, down := serviceStatus()
	if down.Peers[0].State != bfd.StateDown || down.Peers[0].DownTransitions == 0 || instance.BFD == nil || len(instance.BFD.HealthyPeers) != 0 {
		t.Fatal("BFD UDP loss did not publish the independent receive timeout", down.Peers[0])
	}
	t.Log("UDP BFD stopped while TCP KEEPALIVE continued: connection closed and learned route withdrawn")
	second := connect()
	t.Log("BFD resumed: new BGP session, advertisement and route restored")
	beforeInstance, before := serviceStatus()
	if before.Peers[0].State != bfd.StateUp {
		t.Fatal("cannot suspend a BFD worker that is not Up")
	}
	if err := syscall.Kill(bfdPID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(bfdPID, syscall.SIGCONT)
	waitWithdrawal(second)
	frozenInstance, frozen := serviceStatus()
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Peers[0].State != bfd.StateUp || frozen.Peers[0].DownTransitions != before.Peers[0].DownTransitions || frozen.Peers[0].DeadlineBootNanoseconds > clock.BoottimeNanoseconds || frozenInstance.BFD == nil || len(frozenInstance.BFD.HealthyPeers) != 0 {
		t.Fatal("stopped worker's last UP snapshot was not independently expired without a DOWN message", frozen.Peers[0])
	}
	frozenModule := moduleStatus("service.link-watch")
	if frozenInstance.Generation == 0 || frozenInstance.ProducerEpoch == "" || frozenInstance.ConfigDigest == "" || frozenInstance.Generation != beforeInstance.Generation || frozenInstance.ProducerEpoch != beforeInstance.ProducerEpoch || frozenInstance.ConfigDigest != beforeInstance.ConfigDigest || frozenModule.Generation != frozenInstance.Generation || frozenModule.ConfigDigest != frozenInstance.ConfigDigest {
		t.Fatal("stopped BFD snapshot no longer belongs to its current producer", frozenInstance, frozenModule)
	}
	process, err := os.ReadFile("/proc/" + strconv.Itoa(bfdPID) + "/status")
	if err != nil || !strings.Contains(string(process), "State:\tT") || frozenModule.PID != bfdPID {
		t.Fatal("BFD worker did not remain stopped during receive-deadline proof", err)
	}
	t.Log("SIGSTOP BFD worker: last snapshot remains UP, monotonic receive deadline expires, BGP closes and route disappears")
	// The published snapshot can lag the producer's last internal receive.
	// Confirm the whole process is stopped, then hold it past the latest
	// possible pre-stop receive deadline as well as the controller's deadline.
	// This fixture negotiates 200 ms * 5, with 200 ms of scheduling margin.
	if frozen.Peers[0].DetectionTimeMicroseconds != uint64(time.Second/time.Microsecond) {
		t.Fatal("unexpected fixture BFD detection interval", frozen.Peers[0].DetectionTimeMicroseconds)
	}
	stoppedAt, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	holdUntil := stoppedAt.BoottimeNanoseconds + int64(time.Second+200*time.Millisecond)
	until(t, 3*time.Second, func() bool {
		checkPeerError()
		process, err := os.ReadFile("/proc/" + strconv.Itoa(bfdPID) + "/status")
		if err != nil || !strings.Contains(string(process), "State:\tT") {
			t.Fatal("BFD worker resumed before its unpublished receive deadline expired", err)
		}
		now, err := bfd.ReadClock()
		if err != nil || now.BootID != stoppedAt.BootID {
			t.Fatal("BFD stop hold lost its monotonic kernel clock", err)
		}
		return now.BoottimeNanoseconds >= holdUntil
	})
	heldInstance, _ := serviceStatus()
	heldModule := moduleStatus("service.link-watch")
	process, err = os.ReadFile("/proc/" + strconv.Itoa(bfdPID) + "/status")
	if err != nil || !strings.Contains(string(process), "State:\tT") || heldModule.PID != bfdPID || heldModule.Generation != frozenModule.Generation || heldModule.ConfigDigest != frozenModule.ConfigDigest || heldInstance.Generation != frozenInstance.Generation || heldInstance.ProducerEpoch != frozenInstance.ProducerEpoch || heldInstance.ConfigDigest != frozenInstance.ConfigDigest || heldInstance.Sequence != frozenInstance.Sequence || !bytes.Equal(heldInstance.Snapshot, frozenInstance.Snapshot) || heldInstance.BFD == nil || len(heldInstance.BFD.HealthyPeers) != 0 {
		t.Fatal("BFD stopped producer or its frozen UP snapshot changed before SIGCONT", err)
	}
	t.Log("SIGSTOP hold exceeded the latest possible unpublished receive deadline; producer and frozen UP snapshot remained unchanged")
	if err := syscall.Kill(bfdPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	connect()
	recoveredInstance, recovered := serviceStatus()
	recoveredModule := moduleStatus("service.link-watch")
	clock, err = bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.Active || recovered.Peers[0].State != bfd.StateUp || recoveredInstance.BFD == nil || len(recoveredInstance.BFD.HealthyPeers) != 1 || recovered.Peers[0].LastReceivedBootNanoseconds <= frozen.Peers[0].LastReceivedBootNanoseconds || recovered.BootID != frozen.BootID || recovered.BootID != clock.BootID || recovered.Peers[0].LastReceivedBootNanoseconds > clock.BoottimeNanoseconds || recovered.Peers[0].DeadlineBootNanoseconds <= clock.BoottimeNanoseconds {
		t.Fatal("BFD recovery lacks fresh receive evidence and current healthy UP state", recoveredInstance, recovered.Peers[0])
	}
	if recoveredInstance.Name != frozenInstance.Name || recoveredInstance.Role != "bfd" || recoveredInstance.ConfigDigest != frozenInstance.ConfigDigest || recoveredModule.Name != recoveredInstance.Name || recoveredModule.Role != recoveredInstance.Role || recoveredModule.Generation != recoveredInstance.Generation || recoveredModule.ConfigDigest != recoveredInstance.ConfigDigest {
		t.Fatal("recovered BFD snapshot does not match the current ready producer and unchanged configuration", recoveredInstance, recoveredModule)
	}
	if recoveredInstance.Generation == frozenInstance.Generation && recoveredInstance.ProducerEpoch == frozenInstance.ProducerEpoch {
		if recoveredModule.PID != frozenModule.PID || recoveredInstance.Sequence <= frozenInstance.Sequence || recovered.Peers[0].DownTransitions <= frozen.Peers[0].DownTransitions {
			t.Fatal("same BFD producer resumed without recording the receive-loss transition", frozenInstance, recoveredInstance, recovered.Peers[0])
		}
		t.Logf("SIGCONT recovery: same BFD producer PID %d generation %d recorded a loss transition and fresh receive", recoveredModule.PID, recoveredInstance.Generation)
	} else {
		// An in-flight effects call may time out while SIGSTOP suspends the
		// producer. A supervisor restart creates a new counter scope, which
		// must prove its own identity and fresh wire receive before BGP resumes.
		if recoveredInstance.Generation <= frozenInstance.Generation || recoveredInstance.ProducerEpoch == "" || recoveredInstance.ProducerEpoch == frozenInstance.ProducerEpoch || recoveredModule.PID == frozenModule.PID {
			t.Fatal("BFD recovery changed producer identity without a newer worker generation", frozenInstance, recoveredInstance, recoveredModule)
		}
		t.Logf("SIGCONT recovery: supervisor replaced BFD PID %d generation %d with ready PID %d generation %d; new producer epoch and fresh receive verified", frozenModule.PID, frozenInstance.Generation, recoveredModule.PID, recoveredInstance.Generation)
	}
	checkPeerError()
	t.Log("SIGCONT: fresh BFD health recreates BGP session and restores advertisements and learned route")

	// A longer BGP heartbeat cannot keep forwarding after the BFD authority
	// used to resolve that route has expired. Only the writer may progress.
	var bgpObservation bgpconfig.Snapshot
	until(t, 3*time.Second, func() bool {
		var instance runtimeplan.InstanceStatus
		if err := fixture.control.Do(ctx, "GET", "/v1/services/service.edge", nil, &instance); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(instance.Snapshot, &bgpObservation); err != nil {
			t.Fatal(err)
		}
		current, err := bfd.ReadClock()
		return err == nil && bgpObservation.Active && bgpObservation.ExpiresNanos > uint64(current.BoottimeNanoseconds)+uint64(2*time.Second) && fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", index)
	})
	writer := moduleStatus("network")
	for _, module := range []supervisor.ModuleStatus{moduleStatus("control"), moduleStatus("service.edge"), moduleStatus("service.link-watch")} {
		if err := syscall.Kill(module.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(module.PID, syscall.SIGCONT) })
	}
	until(t, 1500*time.Millisecond, func() bool { return !fixtureRoute(t, "203.0.113.0/24", "", 0) })
	current, err := bfd.ReadClock()
	if err != nil || uint64(current.BoottimeNanoseconds) >= bgpObservation.ExpiresNanos {
		t.Fatal("writer only expired the longer BGP lease instead of its BFD prerequisite", err)
	}
	if live := moduleStatus("network"); live.PID != writer.PID || live.Generation != writer.Generation {
		t.Fatal("network worker restarted during independent BFD expiry", writer, live)
	}
	t.Log("surviving network writer withdrew the learned route within its BFD receive budget while BGP, BFD and control stayed paused and the original BGP lease remained current")
}

type bfdFixtureListener struct {
	net.Listener
	accepted atomic.Uint64
	mu       sync.Mutex
	conns    []net.Conn
	closed   bool
}

func (l *bfdFixtureListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.conns = append(l.conns, conn)
	l.accepted.Add(1)
	return conn, nil
}
func (l *bfdFixtureListener) Close() error {
	err := l.Listener.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for _, conn := range l.conns {
		_ = conn.Close()
	}
	return err
}

type bfdBGPSession struct {
	wire       *fixtureBGPPeer
	closed     chan struct{}
	advertised atomic.Bool
	keepalives atomic.Uint64
}

func (s *bfdBGPSession) observe(ctx context.Context) {
	go func() {
		timer := time.NewTicker(200 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.closed:
				return
			case <-timer.C:
				if s.wire.send(4, nil) == nil {
					s.keepalives.Add(1)
				}
			}
		}
	}()
	defer close(s.closed)
	for {
		kind, payload, err := readFixtureMessage(s.wire.conn)
		if err != nil {
			return
		}
		if kind == 2 && bfdFixtureAdvertised(payload, netip.MustParsePrefix("198.51.100.0/24")) {
			s.advertised.Store(true)
		}
		// A NOTIFICATION alone is not a closed TCP transport. Keep reading until
		// routerd actually closes; this fixture never closes a live session here.
	}
}

func bfdFixtureAdvertised(payload []byte, prefix netip.Prefix) bool {
	if len(payload) < 4 {
		return false
	}
	withdrawn := int(binary.BigEndian.Uint16(payload[:2]))
	if withdrawn+4 > len(payload) {
		return false
	}
	offset := withdrawn + 2
	length := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
	offset += 2
	if length > len(payload)-offset {
		return false
	}
	nlriContains := func(data []byte) bool {
		for len(data) > 0 {
			bits := int(data[0])
			count := (bits + 7) / 8
			if bits > 32 || count+1 > len(data) {
				return false
			}
			if bits == prefix.Bits() && bytes.Equal(data[1:count+1], prefix.Addr().AsSlice()[:count]) {
				return true
			}
			data = data[count+1:]
		}
		return false
	}
	if nlriContains(payload[offset+length:]) {
		return true
	}
	attrs := payload[offset : offset+length]
	for len(attrs) >= 3 {
		flags, kind, n, header := attrs[0], attrs[1], int(attrs[2]), 3
		if flags&0x10 != 0 {
			if len(attrs) < 4 {
				return false
			}
			n, header = int(binary.BigEndian.Uint16(attrs[2:4])), 4
		}
		if n > len(attrs)-header {
			return false
		}
		value := attrs[header : header+n]
		if kind == 14 && len(value) >= 5 && binary.BigEndian.Uint16(value[:2]) == 1 && value[2] == 1 {
			start := 5 + int(value[3])
			if start <= len(value) && nlriContains(value[start:]) {
				return true
			}
		}
		attrs = attrs[header+n:]
	}
	return false
}

type daemonBFDPeer struct {
	receive, send *net.UDPConn
	enabled       atomic.Bool
	received      atomic.Uint64
}

// This codec/state responder is independent of routerd's BFD implementation.
// It emits RFC control fields with a 1-second receive budget (200 ms * 5).
func (p *daemonBFDPeer) run(ctx context.Context, failures chan<- error) {
	state, your := byte(1), uint32(0)
	last := time.Time{}
	report := func(err error) {
		if ctx.Err() == nil {
			select {
			case failures <- err:
			default:
			}
		}
	}
	transmit := func(flags byte) error {
		if !p.enabled.Load() {
			return nil
		}
		frame := make([]byte, 24)
		frame[0], frame[1], frame[2], frame[3] = 0x20, state<<6|flags, 5, 24
		binary.BigEndian.PutUint32(frame[4:8], 0x51310001)
		binary.BigEndian.PutUint32(frame[8:12], your)
		interval := uint32(200000)
		if state != 3 {
			interval = 1000000
		}
		binary.BigEndian.PutUint32(frame[12:16], interval)
		binary.BigEndian.PutUint32(frame[16:20], 200000)
		_, err := p.send.WriteToUDP(frame, &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3784})
		last = time.Now()
		return err
	}
	for ctx.Err() == nil {
		interval := 200 * time.Millisecond
		if state != 3 {
			interval = time.Second
		}
		if time.Since(last) >= interval {
			if err := transmit(0); err != nil {
				report(err)
				return
			}
		}
		_ = p.receive.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
		packet, oob := make([]byte, 512), make([]byte, 256)
		n, on, flags, from, err := p.receive.ReadMsgUDP(packet, oob)
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				continue
			}
			report(err)
			return
		}
		if n != 24 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || packet[0]>>5 != 1 || packet[3] != 24 || packet[2] != 5 || packet[1]&0x0f != 0 || !from.IP.Equal(net.ParseIP("192.0.2.1")) || from.Port < 49152 {
			report(fmt.Errorf("invalid independent BFD wire envelope: %x from=%v", packet[:n], from))
			return
		}
		controls, err := unix.ParseSocketControlMessage(oob[:on])
		if err != nil {
			report(err)
			return
		}
		ttl := 0
		for _, control := range controls {
			if control.Header.Level == unix.IPPROTO_IP && control.Header.Type == unix.IP_TTL && len(control.Data) == 4 {
				ttl = int(binary.NativeEndian.Uint32(control.Data))
			}
		}
		if ttl != 255 {
			report(fmt.Errorf("outbound BFD TTL = %d", ttl))
			return
		}
		remote, previous := packet[1]>>6, state
		your = binary.BigEndian.Uint32(packet[4:8])
		if your == 0 {
			report(errors.New("outbound BFD discriminator is zero"))
			return
		}
		switch {
		case remote == 0:
			state = 1
		case state == 1 && remote == 1:
			state = 2
		case state == 1 && remote == 2:
			state = 3
		case state == 2 && (remote == 2 || remote == 3):
			state = 3
		case state == 3 && remote == 1:
			state = 1
		}
		p.received.Add(1)
		final := byte(0)
		if packet[1]&0x20 != 0 {
			final = 0x10
		}
		if state != previous || final != 0 {
			if err := transmit(final); err != nil {
				report(err)
				return
			}
		}
	}
}

func newDaemonBFDPeer(t *testing.T) (*daemonBFDPeer, *bfdFixtureListener, int) {
	t.Helper()
	runtime.LockOSThread()
	current, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	namespace, err := netns.New()
	if err != nil {
		current.Close()
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	err = netns.Set(current)
	current.Close()
	runtime.UnlockOSThread()
	if err != nil {
		namespace.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = namespace.Close() })
	h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wan0"}, PeerName: "bfdpeer0"}); err != nil {
		t.Fatal(err)
	}
	peerLink, err := h.LinkByName("bfdpeer0")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetNsFd(peerLink, int(namespace)); err != nil {
		t.Fatal(err)
	}
	local, err := h.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	// The production network worker owns the local address. The fixture only
	// creates the disposable veth and configures the independent peer endpoint.
	if err := h.LinkSetUp(local); err != nil {
		t.Fatal(err)
	}
	peer := &daemonBFDPeer{}
	var listener net.Listener
	err = inDaemonBFDNamespace(namespace, func() error {
		h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
		if err != nil {
			return err
		}
		defer h.Close()
		link, err := h.LinkByName("bfdpeer0")
		if err != nil {
			return err
		}
		if err := h.LinkSetUp(link); err != nil {
			return err
		}
		address, err := netlink.ParseAddr("192.0.2.2/24")
		if err != nil {
			return err
		}
		if err := h.AddrAdd(link, address); err != nil {
			return err
		}
		peer.receive, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 3784})
		if err != nil {
			return err
		}
		peer.send, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 49170})
		if err != nil {
			return err
		}
		for _, option := range []struct {
			conn        *net.UDPConn
			name, value int
		}{{peer.receive, unix.IP_RECVTTL, 1}, {peer.send, unix.IP_TTL, 255}} {
			raw, err := option.conn.SyscallConn()
			if err != nil {
				return err
			}
			var optionErr error
			err = raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, option.name, option.value) })
			if err = errors.Join(err, optionErr); err != nil {
				return err
			}
		}
		listener, err = net.Listen("tcp4", "192.0.2.2:0")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := &bfdFixtureListener{Listener: listener}
	t.Cleanup(func() { _ = peer.receive.Close(); _ = peer.send.Close(); _ = counted.Close() })
	return peer, counted, local.Attrs().Index
}

func inDaemonBFDNamespace(namespace netns.NsHandle, fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	current, err := netns.Get()
	if err != nil {
		return err
	}
	defer current.Close()
	if err := netns.Set(namespace); err != nil {
		return err
	}
	return errors.Join(fn(), netns.Set(current))
}
