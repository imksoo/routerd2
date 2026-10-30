//go:build linux && integration

package integration

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonBlackholeRoute(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonBlackholeRoute", 40*time.Second) {
		return
	}
	wan := healthPBRPeer(t, "wan0", "provider0", "192.0.2.1/24", "192.0.2.2")
	lan := fixturePeerNamespace(t, "lan0", "client0")
	for _, peer := range []struct {
		ns                  netns.NsHandle
		name, cidr, gateway string
	}{
		{wan, "provider0", "10.1.1.1/32", ""},
		{lan, "client0", "198.18.0.2/24", "198.18.0.1"},
	} {
		h, err := netlink.NewHandleAt(peer.ns, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		link, err := h.LinkByName(peer.name)
		if err != nil {
			t.Fatal(err)
		}
		address, _ := netlink.ParseAddr(peer.cidr)
		if err := h.AddrAdd(link, address); err != nil {
			t.Fatal(err)
		}
		if peer.gateway != "" {
			if err := h.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(peer.gateway)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	wanCapture := healthPBRCapture(t, "wan0")
	lanCapture := healthPBRCapture(t, "lan0")
	vrrpVIPResponders(t, wan, "4", []string{"198.51.100.20", "10.1.1.1"}, "R")
	result, err := legacyconfig.Compile([]byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: blackhole}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4Route
      metadata: {name: private-10-blackhole}
      spec: {type: blackhole, destination: 10.0.0.0/8, metric: 20}
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(result.Document)
	if err != nil {
		t.Fatal(err)
	}
	doc := plan.Native
	doc.Network.Interfaces = []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.2/24"}}, {Name: "lan0", Addresses: []string{"198.18.0.1/24"}}}
	forward := true
	doc.Network.Forwarding.IPv4 = &forward
	doc.Network.Routes = append(doc.Network.Routes, network.Route{Destination: "0.0.0.0/0", Gateway: "192.0.2.1", Interface: "wan0"})
	f := startFixtureDaemon(t, config.FromNative(doc))
	_, foreignPrefix, _ := net.ParseCIDR("10.9.0.0/16")
	if err := netlink.RouteAdd(&netlink.Route{Dst: foreignPrefix, Type: unix.RTN_BLACKHOLE, Protocol: 99, Priority: 30}); err != nil {
		t.Fatal(err)
	}
	probe := func(address string, allowed bool) {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			err := fixtureInsideNamespace(lan, func() error {
				port := "18093"
				if protocol == "udp4" {
					port = "18094"
				}
				c, err := net.DialTimeout(protocol, net.JoinHostPort(address, port), 500*time.Millisecond)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
					return err
				}
				if protocol == "udp4" {
					if _, err := c.Write([]byte("route")); err != nil {
						return err
					}
				}
				var b [1]byte
				_, err = io.ReadFull(c, b[:])
				if err == nil && b[0] != 'R' {
					return errors.New("unexpected peer reply")
				}
				return err
			})
			if allowed {
				if err != nil {
					t.Fatal(address, err)
				}
			} else {
				var failure net.Error
				if !errors.As(err, &failure) || !failure.Timeout() {
					t.Fatalf("blackhole must silently discard forwarded traffic: %v", err)
				}
			}
		}
	}
	probe("198.51.100.20", true)
	probe("10.1.1.1", false)
	f = f.restart()
	probe("198.51.100.20", true)
	probe("10.1.1.1", false)
	for _, transport := range []struct {
		protocol byte
		port     uint16
	}{{6, 18093}, {17, 18094}} {
		until(t, time.Second, func() bool { return lanCapture.count("198.18.0.2", "10.1.1.1", transport.protocol, transport.port) > 0 })
		if wanCapture.count("198.18.0.2", "10.1.1.1", transport.protocol, transport.port) != 0 {
			t.Fatal("blackholed traffic escaped onto WAN", transport.protocol)
		}
	}
	doc.Network.Routes = append(doc.Network.Routes, network.Route{Destination: "10.1.1.1/32", Gateway: "192.0.2.1", Interface: "wan0"})
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "specific-route", Config: config.FromNative(doc)}, nil); err != nil {
		t.Fatal(err)
	}
	probe("10.1.1.1", true)
	doc.Network.Routes = doc.Network.Routes[1:2]
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 2, RequestID: "remove-blackhole", Config: config.FromNative(doc)}, nil); err != nil {
		t.Fatal(err)
	}
	probe("10.1.1.1", true)
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	foreign := false
	for _, route := range routes {
		if route.Dst != nil && route.Dst.String() == "10.0.0.0/8" {
			t.Fatal("removed blackhole remains", route)
		}
		if route.Dst != nil && route.Dst.String() == foreignPrefix.String() && route.Type == unix.RTN_BLACKHOLE && route.Protocol == 99 {
			foreign = true
		}
	}
	if !foreign {
		t.Fatal("unmanaged blackhole was removed")
	}
}
