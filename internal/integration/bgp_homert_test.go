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
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The homert operation uses two same-AS peers, peer-address next-hop rewriting,
// four import prefixes, connected LAN export, and 120/360-second GR intent.
// Addresses and ASNs here are independent lab values, not the private setup.
func TestDaemonBGPTwoFRRPeers(t *testing.T) {
	if !isolateFixtureTimeout(t, t.Name(), 120*time.Second) {
		return
	}
	client := fixturePeerNamespace(t, "lan0", "client0")
	setupBGPAddresses(t, client, []string{"address", "add", "198.18.0.2/24", "dev", "client0"}, []string{"route", "add", "default", "via", "198.18.0.1"})
	lanCapture := healthPBRCapture(t, "lan0")
	restart := &bgpdaemon.AppliedGracefulRestart{Enabled: true, RestartTime: 120, StaleRoutesTime: 360}
	imports := []string{"203.0.113.0/24", "198.51.100.0/24", "10.64.0.0/12", "10.128.0.0/16"}
	learned := []string{"10.64.1.0/24", "10.128.1.0/24"}
	forwardIPv4 := true
	intent := serviceconfig.BGP{Runtime: bgpdaemon.AppliedConfig{
		Global: bgpdaemon.AppliedGlobal{ASN: 64520, RouterID: "192.0.2.1", ListenPort: 179, Families: []string{"ipv4-unicast"}, UseMultiplePaths: true, GracefulRestart: restart},
		Peers:  map[string]bgpdaemon.AppliedPeer{},
		Paths:  []bgpdaemon.AppliedPath{{Source: bgpdaemon.AppliedPathSourceStatic, Prefix: "198.18.0.0/24"}},
	}}
	document := config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "forwarding", Forwarding: &network.Forwarding{IPv4: &forwardIPv4}},
		{ID: "lan", Interface: &network.Interface{Name: "lan0"}},
		{ID: "lan-address", Address: &resourceprogram.Address{Interface: "lan0", CIDR: "198.18.0.1/24"}},
	}}
	type peer struct {
		local, remote string
		prefixes      []string
		addresses     []string
		vty           func(...string) ([]byte, error)
		stop, start   func()
		capture       *healthPBRCaptureState
	}
	var peers []peer
	for i := range 2 {
		iface := fmt.Sprintf("wan%d", i)
		local, remote := fmt.Sprintf("192.0.%d.1", i+2), fmt.Sprintf("192.0.%d.2", i+2)
		namespace := fixturePeerNamespace(t, iface, "peer0")
		prefixes := []string{imports[i], learned[i]}
		addresses := []string{strings.TrimSuffix(prefixes[0], "0/24") + "50", strings.TrimSuffix(prefixes[1], "0/24") + "50"}
		setupBGPAddresses(t, namespace, []string{"address", "add", remote + "/24", "dev", "peer0"}, []string{"address", "add", addresses[0] + "/32", "dev", "lo"}, []string{"address", "add", addresses[1] + "/32", "dev", "lo"}, []string{"route", "add", "198.18.0.0/24", "via", local})
		vty, stop, start := startIndependentFRR(t, namespace, local, remote, prefixes)
		peers = append(peers, peer{local, remote, prefixes, addresses, vty, stop, start, healthPBRCapture(t, iface)})
		document.Resources = append(document.Resources,
			resourceprogram.Resource{ID: iface, Interface: &network.Interface{Name: iface}},
			resourceprogram.Resource{ID: iface + "-address", Address: &resourceprogram.Address{Interface: iface, CIDR: local + "/24"}})
		intent.Runtime.Peers[remote] = bgpdaemon.AppliedPeer{Address: remote, ASN: 64521, ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: imports, NextHopRewrite: "peer-address"}, ExportPolicyName: iface, ExportPolicy: bgpdaemon.AppliedExportPolicy{AllowedPrefixes: []string{"198.18.0.0/24"}}, GracefulRestart: restart}
		server := exec.Command("python3", "-u", "-c", `import socket,threading,sys
def serve(addr,kind,port):
 s=socket.socket(socket.AF_INET,kind);s.bind((addr,port))
 if kind==socket.SOCK_STREAM:
  s.listen()
  while True:
   c,_=s.accept();c.sendall(b'FRR');c.close()
 else:
  while True:
   data,peer=s.recvfrom(1024);s.sendto(data,peer)
for addr in sys.argv[1:]:
 for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
  threading.Thread(target=serve,args=(addr,kind,port),daemon=True).start()
threading.Event().wait()`, addresses[0], addresses[1])
		if err := fixtureInsideNamespace(namespace, server.Start); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Process.Kill(); _ = server.Wait() })
	}
	document.Resources = append(document.Resources, resourceprogram.Resource{ID: "bgp", Service: &serviceconfig.Service{Name: "edge", BGP: &intent}})
	fixture := startFixtureDaemon(t, document)
	advertised := func(p peer, present bool) bool {
		var view struct {
			Paths []json.RawMessage `json:"paths"`
		}
		out, err := p.vty("show bgp ipv4 unicast 198.18.0.0/24 json")
		return err == nil && json.Unmarshal(out, &view) == nil && (len(view.Paths) > 0) == present
	}
	installed := func(p peer, present bool) bool {
		for _, prefix := range p.prefixes {
			_, dst, _ := net.ParseCIDR(prefix)
			routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Dst: dst}, netlink.RT_FILTER_DST)
			if err != nil || (len(routes) > 0) != present {
				return false
			}
			if present && !routes[0].Gw.Equal(net.ParseIP(p.remote)) {
				return false
			}
		}
		return true
	}
	forward := func(p peer) error {
		return fixtureInsideNamespace(client, func() error {
			args := append([]string{"-c", `import socket,sys
for addr in sys.argv[1:]:
 for kind,port in ((socket.SOCK_STREAM,18080),(socket.SOCK_DGRAM,18081)):
  s=socket.socket(socket.AF_INET,kind);s.settimeout(1);s.connect((addr,port))
  if kind==socket.SOCK_DGRAM:s.send(b'FRR')
  assert s.recv(10)==b'FRR';s.close()`}, p.addresses...)
			out, err := exec.CommandContext(context.Background(), "python3", args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("client to %s: %s: %w", p.remote, out, err)
			}
			return nil
		})
	}
	for _, p := range peers {
		until(t, 25*time.Second, func() bool { return advertised(p, true) && installed(p, true) && forward(p) == nil })
		out, err := p.vty("show bgp neighbors " + p.local)
		neighbor := string(out)
		if err != nil || !strings.Contains(neighbor, "Graceful Restart Capability: advertised and received") {
			t.Fatalf("GR was not negotiated with %s: %s", p.remote, neighbor)
		}
	}
	t.Log("both peers established and forwarding; GR negotiated")
	first := peers[0]
	for _, prefix := range first.prefixes {
		if out, err := first.vty("configure terminal", "router bgp 64521", "address-family ipv4 unicast", "no network "+prefix); err != nil {
			t.Fatal(string(out), err)
		}
	}
	until(t, 10*time.Second, func() bool { return installed(first, false) })
	if err := forward(first); err == nil {
		t.Fatal("withdrawn peer routes still forward")
	}
	if err := forward(peers[1]); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range first.prefixes {
		if out, err := first.vty("configure terminal", "router bgp 64521", "address-family ipv4 unicast", "network "+prefix); err != nil {
			t.Fatal(string(out), err)
		}
	}
	until(t, 10*time.Second, func() bool { return installed(first, true) && forward(first) == nil })
	t.Log("one peer withdrawal and rejoin preserved the other peer")
	first.stop()
	until(t, 5*time.Second, func() bool {
		var instance runtimeplan.InstanceStatus
		if err := fixture.control.Do(t.Context(), "GET", "/v1/services/service.edge", nil, &instance); err != nil {
			return false
		}
		var snapshot bgpconfig.Snapshot
		if json.Unmarshal(instance.Snapshot, &snapshot) != nil {
			return false
		}
		stale := 0
		for _, route := range snapshot.Routes {
			if route.PeerAddress == first.remote && route.Stale {
				stale++
			}
		}
		return stale == len(first.prefixes)
	})
	if !installed(first, true) {
		t.Fatal("GR discarded peer routes before its restart interval")
	}
	if err := forward(first); err != nil {
		t.Fatal("GR retained routes do not forward: ", err)
	}
	if err := forward(peers[1]); err != nil {
		t.Fatal(err)
	}
	t.Log("stopped FRR routes observed stale and still forwarding while the other peer remained usable")
	first.start()
	until(t, 25*time.Second, func() bool { return advertised(first, true) && installed(first, true) && forward(first) == nil })
	if err := forward(peers[1]); err != nil {
		t.Fatal(err)
	}
	fixture = fixture.restart()
	for _, p := range peers {
		until(t, 25*time.Second, func() bool { return advertised(p, true) && installed(p, true) && forward(p) == nil })
		until(t, 3*time.Second, func() bool {
			return p.capture.count(p.local, p.remote, 6, 179) > 0 || p.capture.count(p.remote, p.local, 6, 179) > 0
		})
		for _, address := range p.addresses {
			if lanCapture.count("198.18.0.2", address, 6, 18080) == 0 || lanCapture.count("198.18.0.2", address, 17, 18081) == 0 {
				t.Fatalf("missing LAN TCP/UDP capture to %s", address)
			}
		}
	}
	if err := fixture.control.Do(t.Context(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-bgp", Config: config.Document{SchemaVersion: config.SchemaVersion}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		until(t, 15*time.Second, func() bool { return !advertised(p, true) && installed(p, false) })
		if err := forward(p); err == nil {
			t.Fatal("removed BGP configuration still forwards")
		}
	}
	t.Log("two independent FRR peers: four imported prefixes, next-hop rewrite, LAN export, negotiated GR, TCP/UDP, isolated withdrawal/rejoin, peer and product restart, removal passed")
}

func setupBGPAddresses(t *testing.T, namespace netns.NsHandle, commands ...[]string) {
	t.Helper()
	if err := fixtureInsideNamespace(namespace, func() error {
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

func startIndependentFRR(t *testing.T, namespace netns.NsHandle, local, remote string, prefixes []string) (func(...string) ([]byte, error), func(), func()) {
	t.Helper()
	root, err := os.MkdirTemp("", "routerd-two-frr.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if out, err := exec.Command("chown", "frr:frr", root).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	frrPath := os.Getenv("ROUTERD2_FRR_BGPD")
	if frrPath == "" {
		frrPath = "/usr/lib/frr/bgpd"
	}
	config := "hostname independent\nlog stdout\nrouter bgp 64521\n bgp router-id " + remote + "\n no bgp ebgp-requires-policy\n no bgp network import-check\n bgp graceful-restart\n neighbor " + local + " remote-as 64520\n address-family ipv4 unicast\n"
	for _, prefix := range prefixes {
		config += "  network " + prefix + "\n"
	}
	config += "  neighbor " + local + " activate\n  neighbor " + local + " route-map FOREIGN out\n exit-address-family\nroute-map FOREIGN permit 10\n set ip next-hop 192.0.2.254\n"
	for name, body := range map[string]string{"frr.conf": config, "zebra.conf": "hostname independent\nlog stdout\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(root, "zserv.api")
	start := func(name string, args ...string) *exec.Cmd {
		t.Helper()
		log, err := os.OpenFile(filepath.Join(root, name+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = log.Close() })
		command := exec.Command(filepath.Join(filepath.Dir(frrPath), name), args...)
		command.Stdout, command.Stderr = log, log
		if err := fixtureInsideNamespace(namespace, command.Start); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if command.ProcessState == nil {
				_ = command.Process.Kill()
				_ = command.Wait()
			}
		})
		return command
	}
	start("zebra", "-u", "frr", "-g", "frr", "-z", socket, "--vty_socket", root, "-i", filepath.Join(root, "zebra.pid"), "-f", filepath.Join(root, "zebra.conf"), "--log", "stdout")
	until(t, 5*time.Second, func() bool { info, err := os.Stat(socket); return err == nil && info.Mode()&os.ModeSocket != 0 })
	startBGP := func() *exec.Cmd {
		return start("bgpd", "-S", "-n", "-z", socket, "-p", "179", "--vty_socket", root, "-i", filepath.Join(root, "bgpd.pid"), "-f", filepath.Join(root, "frr.conf"), "--log", "stdout")
	}
	bgp := startBGP()
	t.Cleanup(func() {
		if t.Failed() {
			for _, name := range []string{"zebra", "bgpd"} {
				body, _ := os.ReadFile(filepath.Join(root, name+".log"))
				t.Logf("FRR %s %s: %s", remote, name, body)
			}
		}
	})
	vty := func(commands ...string) ([]byte, error) {
		t.Helper()
		args := []string{"--vty_socket", root, "-d", "bgpd"}
		for _, command := range commands {
			args = append(args, "-c", command)
		}
		return exec.Command("vtysh", args...).CombinedOutput()
	}
	return vty, func() { _ = bgp.Process.Kill(); _ = bgp.Wait() }, func() { bgp = startBGP() }
}
