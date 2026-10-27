//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type routedBFDPeer struct {
	peer                    *daemonBFDPeer
	listener                *bfdFixtureListener
	desired                 network.Desired
	source, remote, gateway string
	index                   int
	remoteNS                netns.NsHandle
}

// Each endpoint owns a /32 or /128 on lo. A third Linux namespace forwards
// between different subnets, so direct-link transport cannot pass this fixture.
func newRoutedBFDPeer(t *testing.T, family, interfaceName string) routedBFDPeer {
	t.Helper()
	middle := fixturePeerNamespace(t, interfaceName, "from-router")
	var remoteNS netns.NsHandle
	if err := fixtureInsideNamespace(middle, func() error {
		remoteNS = fixturePeerNamespace(t, "to-peer", "peer0")
		key := "/proc/sys/net/ipv4/ip_forward"
		if family == "ipv6" {
			key = "/proc/sys/net/ipv6/conf/all/forwarding"
		}
		return os.WriteFile(key, []byte("1\n"), 0644)
	}); err != nil {
		t.Fatal(err)
	}
	source, remote := "192.0.2.1", "192.0.2.2"
	localCIDR, gatewayCIDR, outboundCIDR, peerCIDR := "198.18.0.1/30", "198.18.0.2/30", "198.18.0.5/30", "198.18.0.6/30"
	hostBits := 32
	if family == "ipv6" {
		source, remote, hostBits = "2001:db8:100::1", "2001:db8:200::2", 128
		localCIDR, gatewayCIDR, outboundCIDR, peerCIDR = "2001:db8:10::1/64", "2001:db8:10::2/64", "2001:db8:20::1/64", "2001:db8:20::2/64"
	}
	local := netip.MustParsePrefix(localCIDR).Addr().String()
	gateway := netip.MustParsePrefix(gatewayCIDR).Addr().String()
	outbound := netip.MustParsePrefix(outboundCIDR).Addr().String()
	peerAddress := netip.MustParsePrefix(peerCIDR).Addr().String()
	sourceCIDR := netip.PrefixFrom(netip.MustParseAddr(source), hostBits).String()
	remoteCIDR := netip.PrefixFrom(netip.MustParseAddr(remote), hostBits).String()
	configure := func(namespace netns.NsHandle, addresses map[string]string, routes []network.Route) {
		t.Helper()
		h, err := netlink.NewHandleAt(namespace, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		for name, cidr := range addresses {
			link, err := h.LinkByName(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.LinkSetUp(link); err != nil {
				t.Fatal(err)
			}
			addr, _ := netlink.ParseAddr(cidr)
			if family == "ipv6" {
				addr.Flags = unix.IFA_F_NODAD
			}
			if err := h.AddrAdd(link, addr); err != nil {
				t.Fatal(err)
			}
		}
		for _, route := range routes {
			_, prefix, _ := net.ParseCIDR(route.Destination)
			link, err := h.LinkByName(route.Interface)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.RouteAdd(&netlink.Route{Dst: prefix, Gw: net.ParseIP(route.Gateway), LinkIndex: link.Attrs().Index}); err != nil {
				t.Fatal(err)
			}
		}
	}
	configure(middle, map[string]string{"from-router": gatewayCIDR, "to-peer": outboundCIDR}, []network.Route{
		{Destination: sourceCIDR, Interface: "from-router", Gateway: local},
		{Destination: remoteCIDR, Interface: "to-peer", Gateway: peerAddress},
	})
	configure(remoteNS, map[string]string{"lo": remoteCIDR, "peer0": peerCIDR}, []network.Route{{Destination: sourceCIDR, Interface: "peer0", Gateway: outbound}})
	if family == "ipv6" {
		// Loopback-sourced packets need usable link-local ND sources on every
		// forwarding link. This isolated static topology coordinates them
		// explicitly instead of relying on automatic-address DAD timing.
		configure(middle, map[string]string{"from-router": "fe80::2/64", "to-peer": "fe80::1/64"}, nil)
		configure(remoteNS, map[string]string{"peer0": "fe80::2/64"}, nil)
	}
	if family == "ipv4" {
		configure(middle, nil, []network.Route{{Destination: "203.0.113.0/24", Interface: "to-peer", Gateway: peerAddress}})
		configure(remoteNS, map[string]string{"lo": "203.0.113.9/32"}, nil)
	} else {
		configure(middle, nil, []network.Route{{Destination: "2001:db8:77::/64", Interface: "to-peer", Gateway: peerAddress}})
		configure(remoteNS, map[string]string{"lo": "2001:db8:77::9/128"}, nil)
	}
	peer, listener := openDaemonBFDPeer(t, remoteNS, netip.MustParseAddr(remote), netip.AddrPortFrom(netip.MustParseAddr(source), 4784), 254)
	link, err := netlink.LinkByName(interfaceName)
	if err != nil {
		t.Fatal(err)
	}
	desired := network.Desired{Interfaces: []network.Interface{
		{Name: interfaceName, Addresses: []string{localCIDR}},
		{Name: "lo", Addresses: []string{sourceCIDR}},
	}, Routes: []network.Route{{Destination: remoteCIDR, Interface: interfaceName, Gateway: gateway}}}
	if family == "ipv6" {
		desired.Interfaces[0].Addresses = append(desired.Interfaces[0].Addresses, "fe80::1/64")
		for i := range desired.Interfaces {
			desired.Interfaces[i].AddressOptions = map[string]network.AddressOptions{}
			for _, address := range desired.Interfaces[i].Addresses {
				desired.Interfaces[i].AddressOptions[address] = network.AddressOptions{NoDAD: true}
			}
		}
	}
	return routedBFDPeer{peer: peer, listener: listener, index: link.Attrs().Index, source: source, remote: remote, gateway: gateway, desired: desired, remoteNS: remoteNS}
}

func TestDaemonMultihopBFDIPv6(t *testing.T) {
	if !isolateFixture(t, "TestDaemonMultihopBFDIPv6") {
		return
	}
	path := newRoutedBFDPeer(t, "ipv6", "wan0")
	capture := healthPBRCapture(t, "wan0")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	failures := make(chan error, 4)
	path.peer.enabled.Store(true)
	go path.peer.run(ctx, failures)
	cfg := bfd.Config{Multihop: true, SourceAddress: "2001:0DB8:0100:0:0:0:0:1", Peers: []bfd.Peer{{Address: "2001:0DB8:0200::2", MinTxMilliseconds: 200, MinRxMilliseconds: 200, DetectMultiplier: 5}}}
	f := startFixtureDaemon(t, config.FromNative(config.Native{Network: path.desired, Services: []serviceconfig.Service{{Name: "routed", BFD: &cfg}}}))
	module := func() supervisor.ModuleStatus {
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == "service.routed" {
				return module
			}
		}
		t.Fatal("missing multihop BFD worker")
		return supervisor.ModuleStatus{}
	}
	wait := func(state string) runtimeplan.InstanceStatus {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		until(t, 6*time.Second, func() bool {
			select {
			case err := <-failures:
				t.Fatal("independent routed peer", err)
			default:
			}
			if err := f.control.Do(ctx, "GET", "/v1/services/service.routed", nil, &instance); err != nil {
				t.Fatal(err)
			}
			var snapshot bfd.Snapshot
			if err := json.Unmarshal(instance.Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
			return len(snapshot.Peers) == 1 && snapshot.Peers[0].State == state
		})
		return instance
	}
	first := wait(bfd.StateUp)
	worker := module()
	path.peer.enabled.Store(false)
	wait(bfd.StateDown)
	path.peer.enabled.Store(true)
	wait(bfd.StateUp)
	if current := module(); current.PID != worker.PID || current.Generation != worker.Generation {
		t.Fatal("routed peer loss replaced BFD worker", worker, current)
	}
	if err := syscall.Kill(worker.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 6*time.Second, func() bool { return module().Generation > worker.Generation && module().Ready })
	fresh := wait(bfd.StateUp)
	if fresh.ProducerEpoch == first.ProducerEpoch || fresh.Generation <= first.Generation {
		t.Fatal("replacement retained the previous BFD producer", first, fresh)
	}
	verifyMultihopBFDCapture(t, capture, path.source, path.remote)
	t.Log("actual IPv6 loopback-to-loopback BFD crossed a Linux router: UDP 4784, received hop limit 254, loss/recovery and fresh worker replacement")
}

func verifyMultihopBFDCapture(t *testing.T, capture *healthPBRCaptureState, local, remote string) {
	t.Helper()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	transmitted, received := 0, 0
	for _, frame := range capture.packets {
		b := frame.data
		var source, destination netip.Addr
		var ttl byte
		var udp []byte
		if len(b) >= 66 && binary.BigEndian.Uint16(b[12:14]) == 0x0800 && b[23] == 17 {
			at := 14 + int(b[14]&15)*4
			if at+32 > len(b) {
				continue
			}
			source, destination, ttl, udp = netip.AddrFrom4([4]byte(b[26:30])), netip.AddrFrom4([4]byte(b[30:34])), b[22], b[at:]
		} else if len(b) >= 86 && binary.BigEndian.Uint16(b[12:14]) == 0x86dd && b[20] == 17 {
			source, destination, ttl, udp = netip.AddrFrom16([16]byte(b[22:38])), netip.AddrFrom16([16]byte(b[38:54])), b[21], b[54:]
		} else {
			continue
		}
		if binary.BigEndian.Uint16(udp[2:4]) != 4784 {
			continue
		}
		if binary.BigEndian.Uint16(udp[:2]) < 49152 {
			t.Fatal("multihop BFD used a source port outside the protocol range")
		}
		if source.String() == local && destination.String() == remote {
			if ttl != 255 {
				t.Fatal("multihop BFD did not originate with TTL/Hop Limit 255", ttl)
			}
			transmitted++
		} else if source.String() == remote && destination.String() == local {
			if ttl != 254 {
				t.Fatal("multihop BFD did not cross the forwarding hop", ttl)
			}
			received++
		}
	}
	if transmitted < 3 || received < 3 {
		t.Fatal("multihop BFD capture incomplete", transmitted, received)
	}
	t.Log("captured routed BFD UDP 4784", local, remote, "transmitted", transmitted, "received", received)
}
