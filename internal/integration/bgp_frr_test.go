//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// FRR is a separate BGP implementation. The product must advertise the LAN
// prefix and install FRR's route before the client can reach the peer.
func TestDaemonBGPWithFRR(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonBGPWithFRR", 90*time.Second) {
		return
	}
	peer := fixturePeerNamespace(t, "wan0", "peer0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	wanCapture, lanCapture := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "lan0")
	setup := func(ns netns.NsHandle, commands ...[]string) {
		t.Helper()
		if err := fixtureInsideNamespace(ns, func() error {
			for _, args := range commands {
				if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
					return fmt.Errorf("ip %v: %s: %w", args, out, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setup(peer, []string{"address", "add", "192.0.2.2/24", "dev", "peer0"}, []string{"address", "add", "203.0.113.50/32", "dev", "lo"}, []string{"route", "add", "198.18.0.0/24", "via", "192.0.2.1"})
	setup(client, []string{"address", "add", "198.18.0.2/24", "dev", "client0"}, []string{"route", "add", "default", "via", "198.18.0.1"})
	root := t.TempDir()
	configPath := filepath.Join(root, "frr.conf")
	frrConfig := "hostname independent\nlog stdout\nrouter bgp 64513\n bgp router-id 192.0.2.2\n no bgp ebgp-requires-policy\n no bgp network import-check\n neighbor 192.0.2.1 remote-as 64512\n address-family ipv4 unicast\n  network 203.0.113.0/24\n  neighbor 192.0.2.1 activate\n exit-address-family\n"
	if err := os.WriteFile(configPath, []byte(frrConfig), 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "frr.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	frrPath := os.Getenv("ROUTERD2_FRR_BGPD")
	if frrPath == "" {
		frrPath = "/usr/lib/frr/bgpd"
	}
	frr := exec.Command(frrPath, "-S", "-n", "-Z", "-p", "1179", "--vty_socket", root, "-i", filepath.Join(root, "bgpd.pid"), "-f", configPath, "--log", "stdout")
	frr.Stdout, frr.Stderr = log, log
	if err := fixtureInsideNamespace(peer, frr.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = frr.Process.Kill(); _ = frr.Wait() })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(root, "frr.log"))
			t.Logf("FRR: %s", data)
		}
	})
	server := exec.Command("python3", "-u", "-c", `import socket, threading
def udp():
 s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('203.0.113.50',18081))
 while True:
  data,addr=s.recvfrom(1024);s.sendto(data,addr)
threading.Thread(target=udp,daemon=True).start()
s=socket.socket();s.bind(('203.0.113.50',18080));s.listen()
while True:
 c,_=s.accept();c.sendall(b'FRR');c.close()`)
	if err := fixtureInsideNamespace(peer, server.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	yes := true
	bgp := serviceconfig.BGP{Runtime: bgpdaemon.AppliedConfig{
		Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1},
		Peers:  map[string]bgpdaemon.AppliedPeer{"192.0.2.2": {Address: "192.0.2.2", ASN: 64513, TimersProfile: "fast", ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24"}}}},
		Paths:  []bgpdaemon.AppliedPath{{Source: "StaticRoute/lan", Prefix: "198.18.0.0/24", Attrs: bgpdaemon.AppliedPathAttrs{NextHop: "192.0.2.1"}}},
	}, PeerTransports: map[string]bgpconfig.PeerTransport{"192.0.2.2": {LocalAddress: "192.0.2.1", RemotePort: 1179, BindInterface: "wan0"}}}
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}, {Name: "lan0", Addresses: []string{"198.18.0.1/24"}}}, Forwarding: network.Forwarding{IPv4: &yes}}, Services: []serviceconfig.Service{{Name: "edge", BGP: &bgp}}}
	fixture := startFixtureDaemon(t, config.FromNative(doc))
	show := func() (bool, bool) {
		out, err := exec.Command("vtysh", "--vty_socket", root, "-d", "bgpd", "-c", "show bgp ipv4 unicast 198.18.0.0/24 json").Output()
		var view struct {
			Paths []json.RawMessage `json:"paths"`
		}
		valid := err == nil && json.Unmarshal(out, &view) == nil
		return len(view.Paths) > 0, valid
	}
	until(t, 25*time.Second, func() bool { present, valid := show(); return valid && present })
	_, destination, _ := net.ParseCIDR("203.0.113.0/24")
	until(t, 15*time.Second, func() bool {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: destination}, netlink.RT_FILTER_DST)
		return err == nil && len(routes) > 0
	})
	forward := func() error {
		return fixtureInsideNamespace(client, func() error {
			out, err := exec.CommandContext(context.Background(), "python3", "-c", `import socket
for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
 s=socket.socket(socket.AF_INET,kind);s.settimeout(2);s.connect(('203.0.113.50',port))
 if kind==socket.SOCK_DGRAM:s.send(b'FRR')
 assert s.recv(10)==b'FRR'
 s.close()`).CombinedOutput()
			if err != nil {
				return fmt.Errorf("client forwarding: %s: %w", strings.TrimSpace(string(out)), err)
			}
			return nil
		})
	}
	if err := forward(); err != nil {
		t.Fatal(err)
	}
	fixture.stop()
	until(t, 15*time.Second, func() bool { present, valid := show(); return valid && !present })
	fixture = fixture.restart()
	until(t, 25*time.Second, func() bool { present, valid := show(); return valid && present })
	until(t, 15*time.Second, func() bool {
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: destination}, netlink.RT_FILTER_DST)
		return err == nil && len(routes) > 0
	})
	if err := forward(); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool {
		return wanCapture.count("192.0.2.1", "192.0.2.2", 6, 1179) > 0 &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 6, 18080) > 0 &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 17, 18081) > 0
	})
}
