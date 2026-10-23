//go:build linux && integration

package dhcpserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The outer process only forks. All native protocol sockets and link/address
// mutations occur after verifying a fresh user AND network namespace.
func TestIsolatedNativeDHCPServer(t *testing.T) {
	if os.Getenv("ROUTERD2_DHCPSERVER_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		if err != nil || current == os.Getenv("ROUTERD2_DHCPSERVER_PARENT_NS") || os.Getenv("ROUTERD2_DHCPSERVER_PARENT_NS") == "" {
			t.Fatal("refusing DHCP fixture without network isolation")
		}
		nativeServerFixture(t)
		return
	}
	parent, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestIsolatedNativeDHCPServer$", "-test.v", "-test.timeout=70s")
	cmd.Env = append(os.Environ(), "ROUTERD2_DHCPSERVER_CHILD=1", "ROUTERD2_DHCPSERVER_PARENT_NS="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	out, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
		if errors.Is(err, unix.ENOENT) {
			unavailable = exec.Command(exe, "-test.run=^$").Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated DHCP server namespaces unavailable: %v", err)
		}
		t.Fatalf("native DHCP server fixture: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func nativeServerFixture(t *testing.T) {
	dir, err := os.MkdirTemp("", "routerd2-dhcp-server-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	clientNS, serverLink := serverPair(t, "srv0", "cli0")
	serverAddr, _ := netlink.ParseAddr("192.0.2.1/24")
	if err := netlink.AddrAdd(serverLink, serverAddr); err != nil {
		t.Fatal(err)
	}
	c := Normalize(Config{Resource: "DHCPv4Server/native", Interface: "srv0", ServerAddress: "192.0.2.1", StatePath: filepath.Join(dir, "server.json"), SocketPath: filepath.Join(dir, "server.sock"), Primary: Scope{Prefix: "192.0.2.0/24", Start: "192.0.2.100", End: "192.0.2.102", Router: "192.0.2.1", DNSServers: []string{"192.0.2.1"}, LeaseSeconds: 60}, Authoritative: true, ProbeTimeoutMilliseconds: 50})
	// Exact VRF binding is exercised when the host kernel provides the driver.
	vrf := &netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: "dhcpblue"}, Table: 1001}
	if err := netlink.LinkAdd(vrf); err == nil {
		if err := netlink.LinkSetUp(vrf); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetMaster(serverLink, vrf); err != nil {
			t.Fatal(err)
		}
		c.VRF = "dhcpblue"
		t.Log("native UDP67 serving interface is in VRF dhcpblue")
	} else if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOENT) {
		t.Logf("VRF driver unavailable; ordinary namespace path still tested: %v", err)
	} else {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var latest Snapshot
	effects := EffectsFunc(func(_ context.Context, s Snapshot) error {
		if err := dhcpserverconfig.ValidateSnapshot(c, s); err != nil {
			return err
		}
		mu.Lock()
		latest = s
		mu.Unlock()
		return nil
	})
	start := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		ready := make(chan struct{})
		go func() { done <- Run(ctx, c, effects, func() { close(ready) }) }()
		select {
		case <-ready:
		case err := <-done:
			cancel()
			t.Fatalf("start native server: %v", err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("native server not ready")
		}
		return cancel, done
	}
	stop := func(cancel context.CancelFunc, done <-chan error) {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("native server did not terminate")
		}
	}
	cancel, done := start()
	defer func() {
		if cancel != nil {
			stop(cancel, done)
		}
	}()
	if duplicate, err := openTransport(c); err == nil {
		duplicate.Close()
		t.Fatal("duplicate native interface ownership accepted")
	}
	wrong := c
	if c.VRF != "" {
		wrong.VRF = ""
	} else {
		wrong.VRF = "missing"
	}
	if wrongTransport, err := openTransport(wrong); err == nil {
		wrongTransport.Close()
		t.Fatal("wrong VRF intent accepted")
	}
	reportPath := filepath.Join(dir, "client-report.json")
	runClient := func(mode string) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		serverNS, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		clientNamespace, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", int(clientNS)))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestDHCPServerClientChild$", "-test.v", "-test.timeout=45s")
		cmd.Env = append(os.Environ(), "ROUTERD2_DHCPSERVER_CLIENT="+mode, "ROUTERD2_DHCPSERVER_SERVER_NS="+serverNS, "ROUTERD2_DHCPSERVER_CLIENT_NS="+clientNamespace, "ROUTERD2_DHCPSERVER_CLIENT_STATE="+filepath.Join(dir, "client.json"), "ROUTERD2_DHCPSERVER_CLIENT_REPORT="+reportPath, "ROUTERD2_DHCPSERVER_SOCKET="+c.SocketPath)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := inServerNamespace(clientNS, cmd.Start); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("native client %s: %v\n%s", mode, err, output.String())
		}
		t.Logf("native client %s:\n%s", mode, output.String())
	}
	runClient("dora-renew")
	var first dhcp4.Snapshot
	if err := json.Unmarshal(mustRead(t, reportPath), &first); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	before := latest
	mu.Unlock()
	if len(before.Leases) != 1 || before.Stats.Acknowledged < 2 || before.Stats.Offered != 1 || first.CurrentAddress != before.Leases[0].IPAddress {
		t.Fatalf("DORA/renew not observed: %+v", before)
	}
	stop(cancel, done)
	cancel = nil
	cancel, done = start()
	runClient("reboot")
	var reboot dhcp4.Snapshot
	if err := json.Unmarshal(mustRead(t, reportPath), &reboot); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	after := latest
	mu.Unlock()
	if reboot.CurrentAddress != first.CurrentAddress || after.Stats.Offered != 0 || after.Stats.Acknowledged != 1 {
		t.Fatalf("restart did not reuse journal with INIT-REBOOT: %+v", after)
	}
	runClient("wire-lifecycle")
	mu.Lock()
	final := latest
	mu.Unlock()
	if final.Stats.NAK < 1 || final.Stats.Released != 1 || final.Stats.Declined != 1 || final.Stats.Quarantined != 1 || len(final.Leases) != 0 {
		t.Fatalf("native NAK/RELEASE/DECLINE: %+v", final)
	}
	stop(cancel, done)
	cancel = nil
}

func inServerNamespace(ns netns.NsHandle, fn func() error) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, err := netns.Get()
	if err != nil {
		return err
	}
	defer previous.Close()
	if err = netns.Set(ns); err != nil {
		return err
	}
	defer func() { result = errors.Join(result, netns.Set(previous)) }()
	return fn()
}
func serverPair(t *testing.T, serverName, clientName string) (netns.NsHandle, netlink.Link) {
	t.Helper()
	runtime.LockOSThread()
	prior, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	ns, createErr := netns.New()
	restoreErr := netns.Set(prior)
	prior.Close()
	runtime.UnlockOSThread()
	if err := errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: serverName}, PeerName: clientName}); err != nil {
		t.Fatal(err)
	}
	server, err := netlink.LinkByName(serverName)
	if err != nil {
		t.Fatal(err)
	}
	client, err := netlink.LinkByName(clientName)
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetNsFd(client, int(ns)); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	client, err = h.LinkByName(clientName)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetHardwareAddr(client, testMAC); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(client); err != nil {
		t.Fatal(err)
	}
	loop, err := h.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.LinkSetUp(loop); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(server); err != nil {
		t.Fatal(err)
	}
	return ns, server
}

func TestDHCPServerClientChild(t *testing.T) {
	mode := os.Getenv("ROUTERD2_DHCPSERVER_CLIENT")
	if mode == "" {
		t.Skip("only runs inside the isolated server fixture")
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || current != os.Getenv("ROUTERD2_DHCPSERVER_CLIENT_NS") || current == os.Getenv("ROUTERD2_DHCPSERVER_SERVER_NS") || current == os.Getenv("ROUTERD2_DHCPSERVER_PARENT_NS") || os.Getenv("ROUTERD2_DHCPSERVER_CHILD") != "1" {
		t.Fatal("refusing client fixture outside its dedicated namespace")
	}
	if mode == "wire-lifecycle" {
		nativeClientWireLifecycle(t)
		return
	}
	if mode != "dora-renew" && mode != "reboot" {
		t.Fatal("unknown fixture client mode")
	}
	link, err := netlink.LinkByName("cli0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	wanted := 1
	if mode == "dora-renew" {
		wanted = 2
	}
	bindings := 0
	var latest dhcp4.Snapshot
	var installed *netlink.Addr
	effects := dhcp4.EffectsFunc(func(_ context.Context, s dhcp4.Snapshot) error {
		if s.Active {
			a, err := netlink.ParseAddr(fmt.Sprintf("%s/%d", s.CurrentAddress, s.PrefixLength))
			if err != nil {
				return err
			}
			if err := netlink.AddrReplace(link, a); err != nil {
				return err
			}
			installed = a
			if s.State == wire.StateBound {
				bindings++
				latest = s
				if s.ServerID != "192.0.2.1" || s.LeaseTimeSeconds != 60 || s.DefaultGateway != "192.0.2.1" {
					return errors.New("unexpected native server lease options")
				}
				if bindings >= wanted {
					cancel()
				}
			}
		} else if installed != nil {
			if err := netlink.AddrDel(link, installed); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
				return err
			}
			installed = nil
		}
		return nil
	})
	err = dhcp4.Run(ctx, dhcp4.Config{Resource: "DHCPv4Client/native", Interface: "cli0", StatePath: os.Getenv("ROUTERD2_DHCPSERVER_CLIENT_STATE"), Hostname: "native-client"}, effects, nil)
	if !errors.Is(err, context.Canceled) || bindings != wanted {
		t.Fatalf("native DHCP client failed: bound=%d wanted=%d err=%v", bindings, wanted, err)
	}
	b, err := json.Marshal(latest)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(os.Getenv("ROUTERD2_DHCPSERVER_CLIENT_REPORT"), b); err != nil {
		t.Fatal(err)
	}
	t.Logf("native DHCP client reached %d BOUND acknowledgments at %s", bindings, latest.CurrentAddress)
}

func nativeClientWireLifecycle(t *testing.T) {
	var prior dhcp4.Snapshot
	if err := json.Unmarshal(mustRead(t, os.Getenv("ROUTERD2_DHCPSERVER_CLIENT_REPORT")), &prior); err != nil {
		t.Fatal(err)
	}
	iface, err := net.InterfaceByName("cli0")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_IP)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: htons(unix.ETH_P_IP)}); err != nil {
		t.Fatal(err)
	}
	server := netip.MustParseAddr("192.0.2.1")
	id := append([]byte{1}, iface.HardwareAddr...)
	send := func(p packet) {
		frame, err := udpFrame(iface.HardwareAddr, broadcastMAC, p.Source, p.Destination, 68, 67, 1, p.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: iface.Index, Protocol: htons(unix.ETH_P_IP)}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func(xid uint32, typ byte, unicast bool) wire.Message {
		deadline := time.Now().Add(3 * time.Second)
		buf := make([]byte, 2048)
		for time.Now().Before(deadline) {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, 50); err != nil && err != unix.EINTR {
				t.Fatal(err)
			}
			if poll[0].Revents&unix.POLLIN == 0 {
				continue
			}
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if n < 42 || buf[14] != 0x45 || buf[23] != 17 || binary.BigEndian.Uint16(buf[34:36]) != 67 || binary.BigEndian.Uint16(buf[36:38]) != 68 {
				continue
			}
			m, err := wire.Decode(buf[42:n])
			if err != nil || m.XID != xid || m.MessageType() != typ {
				continue
			}
			if unicast && (string(buf[:6]) != string(iface.HardwareAddr) || addr4(buf[30:34]) != m.YIAddr) {
				t.Fatal("pre-address OFFER was not delivered by Ethernet/IP unicast")
			}
			return m
		}
		t.Fatalf("wire reply timed out xid=%d type=%d", xid, typ)
		return wire.Message{}
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", os.Getenv("ROUTERD2_DHCPSERVER_SOCKET"))
	}}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	waitStatus := func(accept func(Snapshot) bool) Snapshot {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			r, err := client.Get("http://unix/v1/status")
			if err != nil {
				t.Fatal(err)
			}
			var s Snapshot
			err = json.NewDecoder(r.Body).Decode(&s)
			r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if accept(s) {
				return s
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("native lease event not published")
		return Snapshot{}
	}
	send(requestPacket(msgRequest, 100, iface.HardwareAddr, map[byte][]byte{61: id, 50: {192, 0, 2, 254}, 54: server.AsSlice()}, ""))
	receive(100, msgNAK, false)
	send(requestPacket(msgRelease, 101, iface.HardwareAddr, map[byte][]byte{61: id, 54: server.AsSlice()}, prior.CurrentAddress))
	waitStatus(func(s Snapshot) bool { return s.Stats.Released == 1 && len(s.Leases) == 0 })
	p := requestPacket(msgDiscover, 102, iface.HardwareAddr, map[byte][]byte{61: id, 50: netip.MustParseAddr(prior.CurrentAddress).AsSlice()}, "")
	binary.BigEndian.PutUint16(p.Data[10:12], 0)
	send(p)
	offer := receive(102, msgOffer, true)
	send(requestPacket(msgDecline, 102, iface.HardwareAddr, map[byte][]byte{61: id, 50: offer.YIAddr.AsSlice(), 54: server.AsSlice()}, ""))
	waitStatus(func(s Snapshot) bool { return s.Stats.Declined == 1 && s.Stats.Quarantined == 1 })
	p = requestPacket(msgDiscover, 103, iface.HardwareAddr, map[byte][]byte{61: id, 50: offer.YIAddr.AsSlice()}, "")
	binary.BigEndian.PutUint16(p.Data[10:12], 0)
	send(p)
	next := receive(103, msgOffer, true)
	if next.YIAddr == offer.YIAddr {
		t.Fatal("DECLINE address was reused on the wire")
	}
	t.Log("native NAK, RELEASE, DECLINE, quarantine and pre-address Ethernet unicast passed")
}
