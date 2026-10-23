//go:build linux && integration

package dhcp4

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd/pkg/pdclient"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// All link/address mutations occur after re-exec into a fresh user AND network
// namespace. The outer test never modifies the host or calls a protocol worker.
// Required CI sets ROUTERD2_INTEGRATION_REQUIRED=1 so missing isolation fails.
func TestIsolatedNativeDHCPAndRA(t *testing.T) {
	if os.Getenv("ROUTERD2_PROTOCOL_CHILD") == "1" {
		current, err := os.Readlink("/proc/self/ns/net")
		if err != nil || current == os.Getenv("ROUTERD2_PROTOCOL_PARENT") || os.Getenv("ROUTERD2_PROTOCOL_PARENT") == "" {
			t.Fatal("refusing protocol fixture outside a fresh network namespace")
		}
		t.Run("DHCPv4", nativeDHCP4)
		t.Run("DHCPv6PD", nativePD)
		t.Run("RouterAdvertisement", nativeRA)
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
	cmd := exec.Command(exe, "-test.run=^TestIsolatedNativeDHCPAndRA$", "-test.v", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), "ROUTERD2_PROTOCOL_CHILD=1", "ROUTERD2_PROTOCOL_PARENT="+parent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	out, err := cmd.CombinedOutput()
	if err != nil {
		unavailable := errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
		if errors.Is(err, syscall.ENOENT) {
			probe := exec.Command(exe, "-test.run=^$")
			unavailable = probe.Run() == nil
		}
		if unavailable && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
			t.Skipf("isolated user/network namespaces unavailable: %v", err)
		}
		t.Fatalf("native protocol fixture: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

func inside(ns netns.NsHandle, fn func() error) (err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	prior, err := netns.Get()
	if err != nil {
		return err
	}
	defer prior.Close()
	if err = netns.Set(ns); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, netns.Set(prior)) }()
	return fn()
}

func pair(t *testing.T, local, peer string) (netns.NsHandle, *netlink.Handle, netlink.Link) {
	t.Helper()
	runtime.LockOSThread()
	prior, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	ns, createErr := netns.New()
	restoreErr := netns.Set(prior)
	prior.Close()
	runtime.UnlockOSThread()
	if err = errors.Join(createErr, restoreErr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close() })
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: local}, PeerName: peer}); err != nil {
		t.Fatal(err)
	}
	client, err := netlink.LinkByName(local)
	if err != nil {
		t.Fatal(err)
	}
	server, err := netlink.LinkByName(peer)
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetNsFd(server, int(ns)); err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	server, err = h.LinkByName(peer)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit non-tentative link-local addresses remove DAD timing from the
	// fixture; the native clients still select an actual assigned address.
	a, _ := netlink.ParseAddr("fe80::1/64")
	a.Flags = unix.IFA_F_NODAD
	if err = netlink.AddrAdd(client, a); err != nil {
		t.Fatal(err)
	}
	a, _ = netlink.ParseAddr("fe80::2/64")
	a.Flags = unix.IFA_F_NODAD
	if err = h.AddrAdd(server, a); err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(client); err != nil {
		t.Fatal(err)
	}
	if err = h.LinkSetUp(server); err != nil {
		t.Fatal(err)
	}
	return ns, h, server
}

func nativeDHCP4(t *testing.T) {
	ns, h, serverLink := pair(t, "d4wan", "d4server")
	a, _ := netlink.ParseAddr("192.0.2.1/24")
	if err := h.AddrAdd(serverLink, a); err != nil {
		t.Fatal(err)
	}
	fd := -1
	var replySocket *net.UDPConn
	err := inside(ns, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(etherTypeIPv4)))
		if err != nil {
			return err
		}
		if err = unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: serverLink.Attrs().Index, Protocol: htons(etherTypeIPv4)}); err != nil {
			return err
		}
		lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "d4server")
				if inner == nil {
					inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
				}
			})
			return errors.Join(err, inner)
		}}
		p, err := lc.ListenPacket(context.Background(), "udp4", "192.0.2.1:67")
		if err == nil {
			replySocket = p.(*net.UDPConn)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	defer replySocket.Close()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 65535)
		for serverCtx.Err() == nil {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, 100); err != nil {
				serverDone <- err
				return
			}
			if poll[0].Revents&unix.POLLIN == 0 {
				continue
			}
			n, _, err := unix.Recvfrom(fd, buf, 0)
			if err != nil {
				serverDone <- err
				return
			}
			if n < 42 || buf[14] != 0x45 || buf[23] != 17 || binary.BigEndian.Uint16(buf[34:]) != 68 {
				continue
			}
			m, err := wire.Decode(buf[42:n])
			if err != nil || m.Op != 1 {
				continue
			}
			kind := byte(wire.MessageACK)
			if m.MessageType() == wire.MessageDiscover {
				kind = wire.MessageOffer
			} else if m.MessageType() != wire.MessageRequest {
				continue
			}
			opts := map[byte][]byte{wire.OptionServerID: {192, 0, 2, 1}, wire.OptionSubnetMask: {255, 255, 255, 0}, wire.OptionRouter: {192, 0, 2, 1}, wire.OptionLeaseTime: {0, 0, 0, 4}, wire.OptionT1: {0, 0, 0, 1}, wire.OptionT2: {0, 0, 0, 3}}
			packet := wire.EncodeRequest(kind, m.XID, m.CHAddr, opts)
			packet[0] = 2
			copy(packet[16:20], []byte{192, 0, 2, 9})
			// A normal kernel UDP sender on veth exercises checksum offload
			// metadata on AF_PACKET reception, unlike hand-built zero-checksum
			// test frames.
			if _, err = replySocket.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4bcast, Port: 68}); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	updates := make(chan Snapshot, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		installed := false
		done <- Run(ctx, Config{Resource: "wan-test", Interface: "d4wan", StatePath: t.TempDir() + "/dhcp4.json"}, EffectsFunc(func(_ context.Context, s Snapshot) error {
			if s.Active && !installed {
				l, err := netlink.LinkByName("d4wan")
				if err != nil {
					return err
				}
				a, _ := netlink.ParseAddr(s.CurrentAddress + "/" + strconv.Itoa(s.PrefixLength))
				if err = netlink.AddrAdd(l, a); err != nil {
					return err
				}
				installed = true
			}
			updates <- s
			return nil
		}), nil)
	}()
	var acquired time.Time
	renewed := false
	for {
		select {
		case s := <-updates:
			if s.Active && s.State == wire.StateBound {
				if acquired.IsZero() {
					acquired = s.AcquiredAt
				} else if !s.AcquiredAt.Equal(acquired) && !renewed {
					renewed = true
					stopServer()
					if err := <-serverDone; err != nil {
						t.Fatal(err)
					}
				}
			}
			if renewed && !s.Active && s.State == wire.StateExpired {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("stop DHCP4: %v", err)
				}
				return
			}
		case err := <-done:
			t.Fatalf("DHCP4 exited before renewal/expiry: %v", err)
		case <-ctx.Done():
			t.Fatal("DHCP4 exchange/renew/expiry timed out")
		}
	}
}

func nativePD(t *testing.T) {
	ns, _, link := pair(t, "pdwan", "pdserver")
	var conn *net.UDPConn
	err := inside(ns, func() error {
		lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "pdserver")
				if inner != nil {
					return
				}
				group := &unix.IPv6Mreq{Interface: uint32(link.Attrs().Index)}
				copy(group.Multiaddr[:], net.ParseIP("ff02::1:2").To16())
				inner = unix.SetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, group)
			})
			return errors.Join(err, inner)
		}}
		p, err := lc.ListenPacket(context.Background(), "udp6", "[::]:547")
		if err == nil {
			conn = p.(*net.UDPConn)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for serverCtx.Err() == nil {
			conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				serverDone <- err
				return
			}
			m, err := pdclient.DecodeMessage(buf[:n])
			if err != nil {
				continue
			}
			response := pdclient.Message{Type: pdclient.MessageReply, TransactionID: m.TransactionID, ClientDUID: m.ClientDUID, ServerDUID: []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 9}, IAID: m.IAID, Prefix: netip.MustParsePrefix("2001:db8:1200::/56"), T1: 2, T2: 6, Preferred: 8, Valid: 10}
			if m.Type == pdclient.MessageSolicit {
				response.Type = pdclient.MessageAdvertise
			}
			data, err := pdclient.EncodeMessage(response)
			if err != nil {
				serverDone <- err
				return
			}
			if m.Type == pdclient.MessageInfoRequest {
				data = append(data, 0, 32, 0, 4, 0, 0, 2, 88) // 600 seconds.
				data = append(data, 0, 23, 0, 16)
				data = append(data, net.ParseIP("2001:db8::53").To16()...)
			}
			from.Zone = strconv.Itoa(link.Attrs().Index)
			if _, err = conn.WriteToUDP(data, from); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	updates := make(chan dhcp6pd.Snapshot, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- dhcp6pd.Run(ctx, dhcp6pd.Config{Resource: "pd-test", Interface: "pdwan", StatePath: t.TempDir() + "/pd.json", IAID: 7}, dhcp6pd.EffectsFunc(func(_ context.Context, s dhcp6pd.Snapshot) error { updates <- s; return nil }), nil)
	}()
	var acquired time.Time
	for {
		select {
		case s := <-updates:
			if !s.Active {
				continue
			}
			if s.CurrentPrefix != "2001:db8:1200::/56" {
				t.Fatalf("wrong PD prefix %q", s.CurrentPrefix)
			}
			if acquired.IsZero() {
				acquired = s.AcquiredAt
			}
			if s.AcquiredAt.After(acquired) && len(s.DNSServers) == 1 && s.InformationRefreshSeconds == 600 {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("stop PD: %v", err)
				}
				stopServer()
				if err := <-serverDone; err != nil {
					t.Fatal(err)
				}
				return
			}
		case err := <-done:
			t.Fatalf("PD exited before renewal/information: %v", err)
		case <-ctx.Done():
			t.Fatal("PD/information/renewal timed out")
		}
	}
}

func nativeRA(t *testing.T) {
	ns, _, link := pair(t, "ralan", "rahost")
	fd := -1
	err := inside(ns, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
		if err != nil {
			return err
		}
		if err = unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "rahost"); err != nil {
			return err
		}
		for _, item := range [][2]int{{unix.IPV6_RECVHOPLIMIT, 1}, {unix.IPV6_MULTICAST_HOPS, 255}} {
			if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, item[0], item[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	statuses := make(chan ra.Snapshot, 64)
	configs := make(chan ra.Config, 4)
	done := make(chan error, 1)
	initial := ra.Config{Resource: "ra-test", Interface: "ralan", RouterLifetimeSeconds: 1800, Prefixes: []ra.Prefix{
		{Prefix: "2001:db8:5::/64", ValidLifetimeSeconds: 100, PreferredLifetimeSeconds: 50, Autonomous: true, OnLink: true},
		{Prefix: "2001:db8:7::/64", ValidLifetimeSeconds: 100, PreferredLifetimeSeconds: 50, Autonomous: true, OnLink: true},
	}}
	go func() {
		done <- ra.RunWithUpdates(ctx, initial, configs, ra.EffectsFunc(func(_ context.Context, s ra.Snapshot) error { statuses <- s; return nil }), nil)
	}()
	read := func(wantWithdrawal bool) []byte {
		t.Helper()
		payload, oob := make([]byte, 2048), make([]byte, 256)
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, 100); err != nil {
				t.Fatal(err)
			}
			if poll[0].Revents&unix.POLLIN == 0 {
				continue
			}
			n, count, _, _, err := unix.Recvmsg(fd, payload, oob, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n < 16 || payload[0] != 134 {
				continue
			}
			cms, err := unix.ParseSocketControlMessage(oob[:count])
			if err != nil {
				t.Fatal(err)
			}
			hop := uint32(0)
			for _, cm := range cms {
				if cm.Header.Level == unix.IPPROTO_IPV6 && cm.Header.Type == unix.IPV6_HOPLIMIT {
					hop = binary.NativeEndian.Uint32(cm.Data)
				}
			}
			if hop != 255 {
				t.Fatalf("RA hop limit %d", hop)
			}
			withdrawn := binary.BigEndian.Uint16(payload[6:]) == 0
			if !wantWithdrawal && withdrawn {
				t.Fatal("hot update unexpectedly withdrew the active router")
			}
			if withdrawn == wantWithdrawal {
				return append([]byte(nil), payload[:n]...)
			}
		}
		t.Fatal("expected RA not received")
		return nil
	}
	prefixes := func(packet []byte) map[string][2]uint32 {
		t.Helper()
		out := map[string][2]uint32{}
		for pos := 16; pos < len(packet); {
			if pos+2 > len(packet) {
				t.Fatal("short RA option")
			}
			size := int(packet[pos+1]) * 8
			if size == 0 || pos+size > len(packet) {
				t.Fatal("invalid RA option size")
			}
			if packet[pos] == 3 {
				if size != 32 {
					t.Fatal("invalid prefix option size")
				}
				var addr [16]byte
				copy(addr[:], packet[pos+16:pos+32])
				name := netip.PrefixFrom(netip.AddrFrom16(addr), int(packet[pos+2])).String()
				out[name] = [2]uint32{binary.BigEndian.Uint32(packet[pos+4:]), binary.BigEndian.Uint32(packet[pos+8:])}
			}
			pos += size
		}
		return out
	}
	if got := prefixes(read(false)); got["2001:db8:5::/64"][0] != 100 {
		t.Fatalf("initial prefixes %v", got)
	}
	renewed := initial
	renewed.Prefixes = append([]ra.Prefix(nil), initial.Prefixes...)
	renewed.Prefixes[0].ValidLifetimeSeconds = 400
	renewed.Prefixes[0].PreferredLifetimeSeconds = 200
	renewed.Prefixes[0].ValidUntil = time.Now().Add(400 * time.Second)
	renewed.Prefixes[0].PreferredUntil = time.Now().Add(200 * time.Second)
	configs <- renewed
	if got := prefixes(read(false)); got["2001:db8:5::/64"][0] < 390 || got["2001:db8:7::/64"][0] != 100 {
		t.Fatalf("renewal did not preserve prefixes: %v", got)
	}
	replaced := renewed
	replaced.Prefixes = append([]ra.Prefix(nil), renewed.Prefixes...)
	replaced.Prefixes[1].Prefix = "2001:db8:6::/64"
	configs <- replaced
	if got := prefixes(read(false)); len(got) != 3 || got["2001:db8:5::/64"][0] < 380 || got["2001:db8:6::/64"][0] != 100 || got["2001:db8:7::/64"] != [2]uint32{} {
		t.Fatalf("selective prefix withdrawal failed: %v", got)
	}
	invalid := replaced
	invalid.Interface = "wrong0"
	configs <- invalid
	rejected := false
	for !rejected {
		select {
		case s := <-statuses:
			if s.RejectedUpdates == 1 {
				if s.Generation != 3 || s.LastUpdateError == "" || s.Withdrawing {
					t.Fatalf("invalid update status: %+v", s)
				}
				rejected = true
			}
		case err := <-done:
			t.Fatalf("RA stopped on invalid hot update: %v", err)
		case <-ctx.Done():
			t.Fatal("invalid update was not reported")
		}
	}
	close(configs) // The last valid configuration must continue serving.
	to := &unix.SockaddrInet6{ZoneId: uint32(link.Attrs().Index)}
	copy(to.Addr[:], net.ParseIP("ff02::2").To16())
	info := &unix.Inet6Pktinfo{Ifindex: uint32(link.Attrs().Index)}
	copy(info.Addr[:], net.ParseIP("fe80::2").To16())
	if _, err = unix.SendmsgN(fd, []byte{133, 0, 0, 0, 0, 0, 0, 0}, unix.PktInfo6(info), to, 0); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case s := <-statuses:
			if s.LastSolicitation.IsZero() {
				continue
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("stop RA: %v", err)
			}
			stopped := prefixes(read(true))
			if stopped["2001:db8:5::/64"] != [2]uint32{} || stopped["2001:db8:6::/64"] != [2]uint32{} {
				t.Fatalf("shutdown did not withdraw final intent: %v", stopped)
			}
			return
		case err := <-done:
			t.Fatalf("RA exited before solicitation: %v", err)
		case <-ctx.Done():
			t.Fatal("RA solicitation not serviced after closing updates")
		}
	}
}
