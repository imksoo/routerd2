//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

//go:embed testdata/bgp-frr.yaml
var legacyBGPFRR []byte

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
	root, err := os.MkdirTemp("", "routerd-frr-peer.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if output, err := exec.Command("chown", "frr:frr", root).CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
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
	// Standard-port FRR needs Zebra's actual interface/NHT observations.
	// bgpd -n still keeps route installation out of this independent peer.
	zebraConfig := filepath.Join(root, "zebra.conf")
	if err := os.WriteFile(zebraConfig, []byte("hostname independent\nlog stdout\n"), 0644); err != nil {
		t.Fatal(err)
	}
	zebraLog, err := os.Create(filepath.Join(root, "zebra.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zebraLog.Close() })
	zebraSocket := filepath.Join(root, "zserv.api")
	zebra := exec.Command(filepath.Join(filepath.Dir(frrPath), "zebra"), "-u", "frr", "-g", "frr", "-z", zebraSocket, "--vty_socket", root, "-i", filepath.Join(root, "zebra.pid"), "-f", zebraConfig, "--log", "stdout")
	zebra.Stdout, zebra.Stderr = zebraLog, zebraLog
	if err := fixtureInsideNamespace(peer, zebra.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = zebra.Process.Kill(); _ = zebra.Wait() })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(root, "zebra.log"))
			t.Logf("Zebra: %s", data)
		}
	})
	until(t, 5*time.Second, func() bool { info, err := os.Stat(zebraSocket); return err == nil && info.Mode()&os.ModeSocket != 0 })
	frr := exec.Command(frrPath, "-S", "-n", "-z", zebraSocket, "-p", "179", "--vty_socket", root, "-i", filepath.Join(root, "bgpd.pid"), "-f", configPath, "--log", "stdout")
	frr.Stdout, frr.Stderr = log, log
	if err := fixtureInsideNamespace(peer, frr.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = frr.Process.Kill(); _ = frr.Wait() })
	t.Cleanup(func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(root, "frr.log"))
			t.Logf("FRR: %s", data)
			for _, command := range []string{"show interface", "show bgp neighbors"} {
				output, err := exec.Command("vtysh", "--vty_socket", root, "-d", "bgpd", "-c", command).CombinedOutput()
				t.Logf("FRR %s: %s %v", command, output, err)
			}
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
	source := filepath.Join(root, "legacy.yaml")
	if err := os.WriteFile(source, legacyBGPFRR, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", source)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	body, err := command.Output()
	if err != nil {
		t.Fatal(err, diagnostics.String())
	}
	document, err := config.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	fixture := startFixtureDaemon(t, document)
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
		return wanCapture.count("192.0.2.1", "192.0.2.2", 6, 179) > 0 &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 6, 18080) > 0 &&
			lanCapture.count("198.18.0.2", "203.0.113.50", 17, 18081) > 0
	})
}
