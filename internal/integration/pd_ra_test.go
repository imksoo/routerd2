//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The independent server builds DHCPv6 options directly; it does not use the
// product's PD codec or state machine. LAN observations inspect actual RA PIOs.
func TestDaemonPDGeneratedRAWorkerLifetime(t *testing.T) {
	if !isolateFixture(t, "TestDaemonPDGeneratedRAWorkerLifetime") {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "provider0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	configure := func(namespace netns.NsHandle, name string, addresses ...string) {
		t.Helper()
		h, err := netlink.NewHandleAt(namespace, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		link, err := h.LinkByName(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.LinkSetUp(link); err != nil {
			t.Fatal(err)
		}
		for _, value := range addresses {
			address, _ := netlink.ParseAddr(value)
			address.Flags = unix.IFA_F_NODAD
			if err := h.AddrAdd(link, address); err != nil {
				t.Fatal(err)
			}
		}
	}
	configure(provider, "provider0", "fe80::2/64")
	configure(provider, "lo", "2001:db8:ffff::9/128")
	configure(client, "client0", "fe80::2/64")
	var server *net.UDPConn
	var providerIndex int
	err := fixtureInsideNamespace(provider, func() error {
		link, err := netlink.LinkByName("provider0")
		if err != nil {
			return err
		}
		providerIndex = link.Attrs().Index
		_, delegated, _ := net.ParseCIDR("2001:db8::/32")
		if err := netlink.RouteAdd(&netlink.Route{Dst: delegated, Gw: net.ParseIP("fe80::1"), LinkIndex: providerIndex}); err != nil {
			return err
		}
		listen := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "provider0")
				if inner != nil {
					return
				}
				group := &unix.IPv6Mreq{Interface: uint32(providerIndex), Multiaddr: netip.MustParseAddr("ff02::1:2").As16()}
				inner = unix.SetsockoptIPv6Mreq(int(fd), unix.IPPROTO_IPV6, unix.IPV6_JOIN_GROUP, group)
			})
			return errors.Join(err, inner)
		}}
		socket, err := listen.ListenPacket(context.Background(), "udp6", "[::]:547")
		if err == nil {
			server = socket.(*net.UDPConn)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var enabled atomic.Bool
	var selected, renewals atomic.Uint32
	enabled.Store(true)
	selected.Store(0x1200)
	failures, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, from, err := server.ReadFromUDP(buffer)
			if err != nil {
				if ctx.Err() == nil {
					failures <- err
				}
				return
			}
			if !enabled.Load() {
				continue
			}
			packet, err := delegatedPrefixReply(buffer[:n], selected.Load())
			if err != nil {
				failures <- err
				return
			}
			if buffer[0] == 5 {
				renewals.Add(1)
			}
			from.Zone = strconv.Itoa(providerIndex)
			if _, err := server.WriteToUDP(packet, from); err != nil {
				failures <- err
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = server.Close(); <-done })
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "P")
	wanCapture, lanCapture := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "lan0")
	forward := true
	doc := config.Native{Network: network.Desired{
		Interfaces: []network.Interface{
			{Name: "wan0", Addresses: []string{"fe80::1/64"}, AddressOptions: map[string]network.AddressOptions{"fe80::1/64": {NoDAD: true}}},
			{Name: "lan0", Addresses: []string{"fe80::1/64"}, AddressOptions: map[string]network.AddressOptions{"fe80::1/64": {NoDAD: true}}},
		},
		Forwarding: network.Forwarding{IPv6: &forward},
		Routes:     []network.Route{{Destination: "2001:db8:ffff::9/128", Gateway: "fe80::2", Interface: "wan0"}},
	}, Services: []serviceconfig.Service{{Name: "pd", DHCP6: &serviceconfig.DHCP6{RequestAddress: true, RequestPrefix: true, Interface: "wan0", IAID: 7, PrefixLength: 56, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", SubnetID: 3, Advertise: true, UseLearnedDNS: true}}}}}}
	started := time.Now()
	f := startFixtureDaemon(t, config.FromNative(doc))
	module := func(name string) (supervisor.ModuleStatus, bool) {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == name {
				return module, true
			}
		}
		return supervisor.ModuleStatus{}, false
	}
	checkPeer := func() {
		t.Helper()
		select {
		case err := <-failures:
			t.Fatal("independent DHCPv6 server", err)
		default:
		}
	}
	waitRA := func(prefix, deprecated string, after time.Time) supervisor.ModuleStatus {
		t.Helper()
		var worker supervisor.ModuleStatus
		until(t, 8*time.Second, func() bool {
			checkPeer()
			var found bool
			worker, found = module("service.pd-lan-ra")
			if !found || !worker.Ready {
				return false
			}
			lanCapture.mu.Lock()
			defer lanCapture.mu.Unlock()
			for _, packet := range lanCapture.packets {
				if packet.at.Before(after) {
					continue
				}
				prefixes := advertisedPDPrefixes(packet.data)
				if prefixes[prefix][0] > 0 && prefixes[prefix][1] > 0 && (deprecated == "" || prefixes[deprecated][0] > 0 && prefixes[deprecated][1] == 0) {
					return true
				}
			}
			return false
		})
		return worker
	}
	traffic := func(prefix string) {
		t.Helper()
		address := prefix + "2/64"
		configure(client, "client0", address)
		if err := fixtureInsideNamespace(client, func() error {
			link, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			_, target, _ := net.ParseCIDR("2001:db8:ffff::9/128")
			return netlink.RouteReplace(&netlink.Route{Dst: target, Gw: net.ParseIP("fe80::1"), LinkIndex: link.Attrs().Index})
		}); err != nil {
			t.Fatal(err)
		}
		for _, protocol := range []string{"tcp6", "udp6"} {
			port := "18093"
			dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(prefix + "2")}}
			if protocol == "udp6" {
				port, dialer.LocalAddr = "18094", &net.UDPAddr{IP: net.ParseIP(prefix + "2")}
			}
			var conn net.Conn
			err := fixtureInsideNamespace(client, func() error {
				var err error
				conn, err = dialer.DialContext(ctx, protocol, net.JoinHostPort("2001:db8:ffff::9", port))
				return err
			})
			if err != nil {
				t.Fatal("delegated client traffic", protocol, err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if protocol == "udp6" {
				_, err = conn.Write([]byte("pd"))
			}
			var reply [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, reply[:])
			}
			conn.Close()
			if err != nil || reply[0] != 'P' {
				t.Fatal("delegated TCP/UDP response", protocol, reply, err)
			}
		}
	}
	first := waitRA("2001:db8:1200:3::/64", "", started)
	traffic("2001:db8:1200:3::")
	until(t, 5*time.Second, func() bool {
		wan, err := netlink.LinkByName("wan0")
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := netlink.AddrList(wan, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range addresses {
			if a.IP.String() == "2001:db8:42::10" && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				return true
			}
		}
		return false
	})
	pdWorker, _ := module("service.pd")
	until(t, 6*time.Second, func() bool { checkPeer(); return renewals.Load() > 0 })
	renumbering := time.Now()
	selected.Store(0x1300)
	renumbered := waitRA("2001:db8:1300:3::/64", "2001:db8:1200:3::/64", renumbering)
	if renumbered.PID != first.PID || renumbered.Generation != first.Generation || renumbered.ConfigDigest != first.ConfigDigest {
		t.Fatal("PD renewal/renumbering replaced the immutable RA worker", first, renumbered)
	}
	traffic("2001:db8:1300:3::")
	enabled.Store(false)
	// RA retirement and kernel address removal are separate effects. Wait for
	// both within the existing expiry budget; process disappearance is not an
	// acknowledgement that the network writer has finished applying it.
	until(t, 14*time.Second, func() bool {
		checkPeer()
		if _, found := module("service.pd-lan-ra"); found {
			return false
		}
		link, err := netlink.LinkByName("lan0")
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if address.IP.IsGlobalUnicast() {
				return false
			}
		}
		return true
	})
	reacquiring := time.Now()
	enabled.Store(true)
	recovered := waitRA("2001:db8:1300:3::/64", "", reacquiring)
	if recovered.Generation <= first.Generation || recovered.ConfigDigest != first.ConfigDigest {
		t.Fatal("reacquired PD did not reuse the prepared RA identity", first, recovered)
	}
	if current, _ := module("service.pd"); current.PID != pdWorker.PID || current.Generation != pdWorker.Generation {
		t.Fatal("lease expiry replaced the DHCPv6 worker", pdWorker, current)
	}
	wanCapture.mu.Lock()
	packets := len(wanCapture.packets)
	wanCapture.mu.Unlock()
	if packets == 0 {
		t.Fatal("DHCPv6 wire capture is empty")
	}
	t.Log("independent DHCPv6 PD and captured LAN RA: renew/renumber on the same RA worker, routed client TCP/UDP, expiry/owned address removal, and reacquisition with the same immutable identity")
}

func delegatedPrefixReply(packet []byte, selected uint32) ([]byte, error) {
	if len(packet) < 4 {
		return nil, errors.New("short DHCPv6 request")
	}
	options := make(map[uint16][]byte)
	for at := 4; at < len(packet); {
		if len(packet)-at < 4 {
			return nil, errors.New("short DHCPv6 option header")
		}
		code, length := binary.BigEndian.Uint16(packet[at:]), int(binary.BigEndian.Uint16(packet[at+2:]))
		at += 4
		if length > len(packet)-at {
			return nil, errors.New("short DHCPv6 option body")
		}
		options[code] = packet[at : at+length]
		at += length
	}
	if len(options[1]) < 4 {
		return nil, errors.New("missing DHCPv6 client identity")
	}
	response := append([]byte{7}, packet[1:4]...)
	if packet[0] == 1 {
		response[0] = 2
	}
	option := func(code uint16, body []byte) {
		response = binary.BigEndian.AppendUint16(response, code)
		response = binary.BigEndian.AppendUint16(response, uint16(len(body)))
		response = append(response, body...)
	}
	option(1, options[1])
	option(2, []byte{0, 3, 0, 1, 2, 0, 0, 0, 0, 9})
	if packet[0] == 11 {
		option(32, []byte{0, 0, 2, 88})
		server := netip.MustParseAddr("2001:db8::53").As16()
		option(23, server[:])
		return response, nil
	}
	if len(options[25]) < 12 {
		return nil, errors.New("missing IA_PD")
	}
	ia := append([]byte(nil), options[25][:4]...)
	ia = binary.BigEndian.AppendUint32(ia, 2)
	ia = binary.BigEndian.AppendUint32(ia, 4)
	ia = append(ia, 0, 26, 0, 25)
	ia = binary.BigEndian.AppendUint32(ia, 6)
	ia = binary.BigEndian.AppendUint32(ia, 10)
	ia = append(ia, 56)
	prefix := netip.MustParseAddr(fmt.Sprintf("2001:db8:%x::", selected)).As16()
	ia = append(ia, prefix[:]...)
	option(25, ia)
	if len(options[3]) < 12 || !bytes.Equal(options[3][:4], options[25][:4]) {
		return nil, errors.New("address and prefix did not share an IAID/transaction")
	}
	if len(options[3]) >= 12 {
		addressIA := append([]byte(nil), options[3][:4]...)
		addressIA = binary.BigEndian.AppendUint32(addressIA, 2)
		addressIA = binary.BigEndian.AppendUint32(addressIA, 4)
		addressIA = append(addressIA, 0, 5, 0, 24)
		raw := netip.MustParseAddr("2001:db8:42::10").As16()
		addressIA = append(addressIA, raw[:]...)
		addressIA = binary.BigEndian.AppendUint32(addressIA, 6)
		addressIA = binary.BigEndian.AppendUint32(addressIA, 10)
		option(3, addressIA)
	}
	return response, nil
}

func advertisedPDPrefixes(frame []byte) map[string][2]uint32 {
	result := make(map[string][2]uint32)
	if len(frame) < 70 || binary.BigEndian.Uint16(frame[12:]) != 0x86dd || frame[20] != 58 || frame[21] != 255 || frame[54] != 134 || binary.BigEndian.Uint16(frame[60:]) == 0 {
		return result
	}
	for at := 70; at+2 <= len(frame); {
		length := int(frame[at+1]) * 8
		if length == 0 || length > len(frame)-at {
			return result
		}
		if frame[at] == 3 && length == 32 {
			prefix := netip.PrefixFrom(netip.AddrFrom16([16]byte(frame[at+16:at+32])), int(frame[at+2]))
			result[prefix.String()] = [2]uint32{binary.BigEndian.Uint32(frame[at+4:]), binary.BigEndian.Uint32(frame[at+8:])}
		}
		at += length
	}
	return result
}
