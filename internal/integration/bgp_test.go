//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/clocktest"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/configref"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// This test proves the complete learned-route path: wire protocol -> worker
// effects -> fenced coordinator -> sole kernel writer. It never changes the
// caller's network namespace and uses an independent minimal BGP wire peer.
func TestDaemonBGPFIBAndOwnershipExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBGPFIBAndOwnershipExpiry") {
		return
	}
	link := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wan0"}, PeerName: "stub0"}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatal(err)
	}
	ownedLink, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	stub, err := netlink.LinkByName("stub0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(stub); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peerReady := make(chan *fixtureBGPPeer, 1)
	peerErrors := make(chan error, 1)
	go func() {
		peer, err := acceptFixturePeer(listener)
		if err != nil {
			peerErrors <- err
			return
		}
		peerReady <- peer
	}()
	c := serviceconfig.BGP{
		Runtime: bgpdaemon.AppliedConfig{
			Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1},
			Peers: map[string]bgpdaemon.AppliedPeer{"127.0.0.1": {
				Address: "127.0.0.1", ASN: 64513, TimersProfile: "fast",
				ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24", "2001:db8:77::/64", "198.18.0.0/24"}, NextHopRewrite: "unchanged"},
			}},
		},
		PeerTransports: map[string]bgpconfig.PeerTransport{"127.0.0.1": {LocalAddress: "127.0.0.1", RemotePort: listener.Addr().(*net.TCPAddr).Port}},
		RouteMetric:    200,
	}
	doc := config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24", "2001:db8::1/64"}, AddressOptions: map[string]network.AddressOptions{"2001:db8::1/64": {NoDAD: true}}}}},
		Services: []serviceconfig.Service{{Name: "edge", BGP: &c}},
	}
	fixture := startFixtureDaemon(t, config.FromNative(doc))
	var peer *fixtureBGPPeer
	select {
	case peer = <-peerReady:
	case err := <-peerErrors:
		t.Fatal(err)
	case <-time.After(12 * time.Second):
		t.Fatal("native BGP worker did not establish the independent TCP session")
	}
	defer peer.conn.Close()
	go func() {
		for {
			typ, data, err := readFixtureMessage(peer.conn)
			if err != nil {
				return
			}
			if typ == 3 {
				select {
				case peerErrors <- fmt.Errorf("BGP notification: %x", data):
				default:
				}
				return
			}
		}
	}()
	announce := func(prefix, hop string, withdraw bool) {
		t.Helper()
		if err := peer.send(2, fixtureUpdate(prefix, hop, withdraw)); err != nil {
			t.Fatal(err)
		}
	}
	announce("203.0.113.0/24", "192.0.2.2", false)
	announce("2001:db8:77::/64", "2001:db8::2", false)
	until(t, 8*time.Second, func() bool {
		return fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", ownedLink.Attrs().Index) && fixtureRoute(t, "2001:db8:77::/64", "2001:db8::2", ownedLink.Attrs().Index)
	})
	// An unresolved path stays visible without installing an onlink shortcut or
	// restarting the session. Replacing its next hop then resolves the route.
	announce("198.18.0.0/24", "198.51.100.254", false)
	until(t, 5*time.Second, func() bool {
		var status runtimeplan.Status
		return fixture.control.Do(context.Background(), "GET", "/v1/services", nil, &status) == nil && len(status.RouteIssues) == 1 && !fixtureRoute(t, "198.18.0.0/24", "", 0)
	})
	announce("198.18.0.0/24", "192.0.2.2", false)
	until(t, 5*time.Second, func() bool { return fixtureRoute(t, "198.18.0.0/24", "192.0.2.2", ownedLink.Attrs().Index) })
	announce("203.0.113.0/24", "", true)
	announce("2001:db8:77::/64", "", true)
	announce("198.18.0.0/24", "", true)
	until(t, 5*time.Second, func() bool {
		return !fixtureRoute(t, "203.0.113.0/24", "", 0) && !fixtureRoute(t, "2001:db8:77::/64", "", 0) && !fixtureRoute(t, "198.18.0.0/24", "", 0)
	})
	announce("203.0.113.0/24", "192.0.2.2", false)
	until(t, 5*time.Second, func() bool { return fixtureRoute(t, "203.0.113.0/24", "192.0.2.2", ownedLink.Attrs().Index) })
	var status supervisor.Status
	if err := fixture.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	pid, controlPID := 0, 0
	var writer supervisor.ModuleStatus
	for _, module := range status.Modules {
		if module.Name == "service.edge" {
			pid = module.PID
		}
		if module.Name == "control" {
			controlPID = module.PID
		}
		if module.Name == "network" {
			writer = module
		}
	}
	if pid == 0 || controlPID == 0 || writer.PID == 0 {
		t.Fatal("missing BGP, control or network worker")
	}
	// Only the network writer can progress. Neither protocol/control callbacks
	// nor supervisor reaping can remove or renew the original route authority.
	for _, stopped := range []int{controlPID, pid} {
		if err := syscall.Kill(stopped, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer syscall.Kill(stopped, syscall.SIGCONT)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	// The writer must recover the same deadline from its ownership journal,
	// without a control apply or a fresh BGP observation to authorize a route.
	if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 7*time.Second, func() bool {
		var status supervisor.Status
		if err := fixture.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			return false
		}
		for _, module := range status.Modules {
			if module.Name == "network" {
				return module.Ready && module.PID != writer.PID && module.Generation > writer.Generation
			}
		}
		return false
	})
	if err := syscall.Kill(fixture.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(fixture.pid, syscall.SIGCONT)
	until(t, 7*time.Second, func() bool { return !fixtureRoute(t, "203.0.113.0/24", "", 0) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("worker exited before independent ownership-expiry proof: %v", err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Log("restarted network writer expired the original BGP route while supervisor, control and BGP were suspended")
}

func TestDaemonBGPRecursivePrerequisiteExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonBGPRecursivePrerequisiteExpiry") {
		return
	}
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "wan0"}, PeerName: "stub0"}); err != nil {
		t.Fatal(err)
	}
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	stub, err := netlink.LinkByName("stub0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(stub); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peers := make(chan *fixtureBGPPeer, 1)
	failures := make(chan error, 1)
	go func() {
		peer, err := acceptFixturePeer(listener)
		if err != nil {
			failures <- err
			return
		}
		peers <- peer
	}()
	untilWall := time.Now().Add(8 * time.Second)
	c := serviceconfig.BGP{
		Runtime: bgpdaemon.AppliedConfig{
			Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1},
			Peers:  map[string]bgpdaemon.AppliedPeer{"127.0.0.1": {Address: "127.0.0.1", ASN: 64513, TimersProfile: "fast", ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"2001:db8:77::/64"}, NextHopRewrite: "unchanged"}}},
		},
		PeerTransports: map[string]bgpconfig.PeerTransport{"127.0.0.1": {LocalAddress: "127.0.0.1", RemotePort: listener.Addr().(*net.TCPAddr).Port}},
		RouteMetric:    200,
	}
	f := startFixtureDaemon(t, config.FromNative(config.Native{
		Network: network.Desired{
			Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: map[string]network.AddressOptions{"2001:db8::1/64": {NoDAD: true}}}},
			Routes:     []network.Route{{Destination: "2001:db8:88::/64", Gateway: "2001:db8::2", Interface: "wan0", ValidUntil: untilWall}},
		},
		Services: []serviceconfig.Service{{Name: "edge", BGP: &c}},
	}))
	var peer *fixtureBGPPeer
	select {
	case peer = <-peers:
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(12 * time.Second):
		t.Fatal("BGP did not establish its independent session")
	}
	defer peer.conn.Close()
	if err := peer.send(2, fixtureUpdate("2001:db8:77::/64", "2001:db8:88::9", false)); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool { return fixtureRoute(t, "2001:db8:77::/64", "2001:db8::2", wan.Attrs().Index) })
	// Flattening the gateway removes the kernel's dependency on the static
	// route. The writer must retain that route's expiry explicitly.
	time.Sleep(time.Until(untilWall.Add(-time.Second)))
	var instance runtimeplan.InstanceStatus
	if err := f.control.Do(context.Background(), "GET", "/v1/services/service.edge", nil, &instance); err != nil {
		t.Fatal(err)
	}
	var observed bgpconfig.Snapshot
	if err := json.Unmarshal(instance.Snapshot, &observed); err != nil {
		t.Fatal(err)
	}
	clock, err := bfd.ReadClock()
	if err != nil || observed.ExpiresNanos <= uint64(clock.BoottimeNanoseconds)+uint64(2*time.Second) {
		t.Fatal("BGP observation does not outlive the recursive prerequisite", err, observed)
	}
	var status supervisor.Status
	if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	paused := 0
	for _, module := range status.Modules {
		if module.Name == "control" || module.Name == "service.edge" {
			if err := syscall.Kill(module.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(module.PID, syscall.SIGCONT)
			paused++
		}
	}
	if paused != 2 {
		t.Fatal("control and BGP workers were not both paused", status)
	}
	if err := syscall.Kill(f.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(f.pid, syscall.SIGCONT)
	until(t, 1500*time.Millisecond, func() bool { return !fixtureRoute(t, "2001:db8:77::/64", "", 0) })
	clock, err = bfd.ReadClock()
	if err != nil || uint64(clock.BoottimeNanoseconds) >= observed.ExpiresNanos {
		t.Fatal("withdrawal waited for the longer BGP lease", err)
	}
	t.Log("writer withdrew the flattened learned IPv6 route at the original recursive-route deadline while supervisor, control and BGP were paused and the BGP lease remained current")
}

func fixtureRoute(t *testing.T, prefix, hop string, index int) bool {
	t.Helper()
	_, dst, err := net.ParseCIDR(prefix)
	if err != nil {
		t.Fatal(err)
	}
	family := netlink.FAMILY_V6
	if dst.IP.To4() != nil {
		family = netlink.FAMILY_V4
	}
	routes, err := netlink.RouteListFiltered(family, &netlink.Route{Dst: dst, Table: 254}, netlink.RT_FILTER_DST|netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		if int(route.Protocol) == 242 && route.Priority == 200 && (index == 0 || route.LinkIndex == index) && (hop == "" || route.Gw.String() == hop) {
			return true
		}
	}
	return false
}

func isolateFixture(t *testing.T, name string) bool {
	return isolateFixtureTimeout(t, name, 75*time.Second)
}

func isolateFixtureTimeout(t *testing.T, name string, timeout time.Duration) bool {
	t.Helper()
	const child = "ROUTERD2_PROTOCOL_FIXTURE"
	const parentKey = "ROUTERD2_PROTOCOL_PARENT"
	if os.Getenv(child) != name {
		if os.Getenv("ROUTERD2_TEST_BIN_DIR") == "" {
			if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
				t.Fatal("ROUTERD2_TEST_BIN_DIR must point to freshly built binaries")
			}
			t.Skip("set ROUTERD2_TEST_BIN_DIR to run protocol process integration")
		}
		parent, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^"+name+"$", "-test.v", "-test.timeout="+timeout.String())
		cmd.Env = append(os.Environ(), child+"="+name, parentKey+"="+parent)
		files, err := fixtureClockFiles(name, clocktest.ForwardFileFromEnv)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			defer file.Close()
		}
		cmd.ExtraFiles = files
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
				t.Skipf("namespace isolation unavailable: %v", err)
			}
			t.Fatalf("isolated protocol fixture: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return false
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || current == os.Getenv(parentKey) || os.Getenv(parentKey) == "" {
		t.Fatal("refusing protocol fixture outside verified isolated network namespace")
	}
	loop, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(loop); err != nil {
		t.Fatal(err)
	}
	return true
}

func fixtureClockFiles(name string, forward func() (*os.File, error)) ([]*os.File, error) {
	if name != "TestDaemonDHCPv4ClockCorrections" && name != "TestDaemonVRRPClockCorrections" {
		return nil, nil
	}
	file, err := forward()
	if err != nil {
		return nil, err
	}
	return []*os.File{file}, nil
}

type daemonFixture struct {
	pid                 int
	control, supervisor *localapi.Client
	network             *localapi.Client
	stop                func()
	restart             func() daemonFixture
}

func startFixtureDaemon(t *testing.T, doc config.Document) daemonFixture {
	t.Helper()
	dir, err := os.MkdirTemp("", "r2-proto-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return startFixtureDaemonAt(t, doc, dir)
}

func startFixtureDaemonAt(t *testing.T, doc config.Document, dir string) daemonFixture {
	return startFixtureDaemonSelected(t, doc, dir, nil)
}

func startFixtureDaemonSelected(t *testing.T, doc config.Document, dir string, startup *configref.Selection) daemonFixture {
	t.Helper()
	runDir, stateDir := filepath.Join(dir, "run"), filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "config.json")
	if err := config.AtomicJSON(configPath, doc); err != nil {
		t.Fatal(err)
	}
	boot := runtimeconfig.Boot{Platform: "host", Config: configPath, Workers: runtimeconfig.NativeWorkers(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd-modules")), RuntimeDir: runDir, StateDir: stateDir, SSH: runtimeconfig.SSH{User: "admin"}}
	boot.Startup = startup
	bootPath := filepath.Join(dir, "boot.json")
	if err := config.AtomicJSON(bootPath, boot); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "console.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "boot", "--file="+bootPath)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(12 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("daemon failed to drain workers")
		}
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("console: %s", b)
			files, _ := filepath.Glob(filepath.Join(stateDir, "logs", "*"))
			for _, file := range files {
				b, _ := os.ReadFile(file)
				if len(b) > 16384 {
					b = b[len(b)-16384:]
				}
				t.Logf("%s: %s", filepath.Base(file), b)
			}
		}
	})
	f := daemonFixture{pid: cmd.Process.Pid, control: localapi.NewClient(filepath.Join(runDir, control.SocketName)), supervisor: localapi.NewClient(filepath.Join(runDir, supervisor.SocketName)), network: localapi.NewClient(filepath.Join(runDir, control.NetworkSocketName)), stop: stop}
	f.restart = func() daemonFixture { stop(); return startFixtureDaemonSelected(t, doc, dir, startup) }
	until(t, 20*time.Second, func() bool {
		var s supervisor.Status
		return f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &s) == nil && s.Ready
	})
	return f
}

type fixtureBGPPeer struct {
	conn net.Conn
	mu   sync.Mutex
}

func (p *fixtureBGPPeer) send(kind byte, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	frame := bytes.Repeat([]byte{255}, 19)
	binary.BigEndian.PutUint16(frame[16:18], uint16(19+len(payload)))
	frame[18] = kind
	frame = append(frame, payload...)
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := p.conn.Write(frame)
	return err
}
func readFixtureMessage(c net.Conn) (byte, []byte, error) {
	h := make([]byte, 19)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(h[16:18]))
	if n < 19 || n > 4096 || !bytes.Equal(h[:16], bytes.Repeat([]byte{255}, 16)) {
		return 0, nil, errors.New("invalid BGP frame")
	}
	data := make([]byte, n-19)
	_, err := io.ReadFull(c, data)
	return h[18], data, err
}
func acceptFixturePeer(l net.Listener) (*fixtureBGPPeer, error) {
	c, err := l.Accept()
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*fixtureBGPPeer, error) { _ = c.Close(); return nil, err }
	_ = c.SetReadDeadline(time.Now().Add(12 * time.Second))
	kind, _, err := readFixtureMessage(c)
	if err != nil || kind != 1 {
		return fail(fmt.Errorf("expected BGP OPEN, got %d: %v", kind, err))
	}
	p := &fixtureBGPPeer{conn: c}
	caps := []byte{1, 4, 0, 1, 0, 1, 1, 4, 0, 2, 0, 1, 65, 4, 0, 0, 252, 1}
	open := []byte{4, 252, 1, 0, 30, 192, 0, 2, 2, byte(2 + len(caps)), 2, byte(len(caps))}
	if err := p.send(1, append(open, caps...)); err != nil {
		return fail(err)
	}
	if err := p.send(4, nil); err != nil {
		return fail(err)
	}
	for {
		kind, data, err := readFixtureMessage(c)
		if err != nil {
			return fail(err)
		}
		if kind == 3 {
			return fail(fmt.Errorf("BGP handshake notification: %x", data))
		}
		if kind == 4 {
			break
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	return p, nil
}
func fixtureUpdate(prefix, nextHop string, withdraw bool) []byte {
	p := netip.MustParsePrefix(prefix).Masked()
	nlri := append([]byte{byte(p.Bits())}, p.Addr().AsSlice()[:(p.Bits()+7)/8]...)
	var attrs []byte
	if !withdraw {
		attrs = []byte{0x40, 1, 1, 0, 0x40, 2, 6, 2, 1, 0, 0, 252, 1}
	}
	if p.Addr().Is4() {
		if withdraw {
			frame := append([]byte{0, byte(len(nlri))}, nlri...)
			return append(frame, 0, 0)
		}
		attrs = append(attrs, 0x40, 3, 4)
		attrs = append(attrs, netip.MustParseAddr(nextHop).AsSlice()...)
	} else {
		mp, kind := []byte{0, 2, 1}, byte(15)
		if !withdraw {
			kind = 14
			mp = append(mp, 16)
			mp = append(mp, netip.MustParseAddr(nextHop).AsSlice()...)
			mp = append(mp, 0)
		}
		mp = append(mp, nlri...)
		attrs = append(attrs, 0x80, kind, byte(len(mp)))
		attrs = append(attrs, mp...)
	}
	frame := append([]byte{0, 0, byte(len(attrs) >> 8), byte(len(attrs))}, attrs...)
	if p.Addr().Is4() {
		frame = append(frame, nlri...)
	}
	return frame
}
