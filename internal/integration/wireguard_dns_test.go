//go:build linux && integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"golang.org/x/sys/unix"
)

func TestDaemonWireGuardDNS(t *testing.T) {
	if !isolateFixtureTimeout(t, t.Name(), 285*time.Second) {
		return
	}
	work := t.TempDir()
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	resolver := filepath.Join(work, "resolv.conf")
	if err := os.WriteFile(resolver, []byte("nameserver 192.0.2.2\noptions timeout:1 attempts:1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(resolver, "/etc/resolv.conf", "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	peer := fixturePeerNamespace(t, "wan0", "peer0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	setupBGPAddresses(t, peer, []string{"address", "add", "192.0.2.2/24", "dev", "peer0"}, []string{"address", "add", "192.0.2.3/24", "dev", "peer0"})
	setupBGPAddresses(t, client, []string{"address", "add", "198.18.0.2/24", "dev", "client0"}, []string{"route", "add", "default", "via", "198.18.0.1"})
	hosts := filepath.Join(work, "hosts")
	writeDNS := func(address string) {
		t.Helper()
		body := ""
		if address != "" {
			body = address + " wg-peer.test\n"
		}
		if err := os.WriteFile(hosts, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeDNS("192.0.2.2")
	dns := startFixtureDNSMasq(t, peer, "peer0", []string{"--no-resolv", "--no-hosts", "--addn-hosts=" + hosts, "--local=/wg-peer.test/"})
	private, public := samFixtureWireGuardKey(t)
	peerPrivate, peerPublic := samFixtureWireGuardKey(t)
	keyFile, peerKey := filepath.Join(work, "router.key"), filepath.Join(work, "peer.key")
	for path, key := range map[string]string{keyFile: private, peerKey: peerPrivate} {
		if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setupBGPAddresses(t, peer, []string{"link", "add", "wg-peer", "type", "wireguard"}, []string{"address", "add", "198.51.100.20/24", "dev", "wg-peer"}, []string{"link", "set", "wg-peer", "up"}, []string{"route", "add", "198.18.0.0/24", "dev", "wg-peer"})
	peerWG := func(args ...string) {
		t.Helper()
		if err := fixtureInsideNamespace(peer, func() error { return exec.Command("wg", append([]string{"set", "wg-peer"}, args...)...).Run() }); err != nil {
			t.Fatal(err)
		}
	}
	peerWG("private-key", peerKey, "listen-port", "51821", "peer", public, "endpoint", "192.0.2.1:51820", "allowed-ips", "198.51.100.10/32,198.18.0.0/24")
	healthPBRListener(t, peer, "198.51.100.20:18080", "W")
	healthPBRUDP(t, peer, "W")
	_ = healthPBRCapture(t, "wan0")
	_ = healthPBRCapture(t, "lan0")
	forward := true
	desired := network.Desired{
		Links:      []network.Link{{Name: "wg0", Kind: "wireguard"}},
		Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"192.0.2.1/24"}}, {Name: "lan0", Addresses: []string{"198.18.0.1/24"}}, {Name: "wg0", Addresses: []string{"198.51.100.10/24"}}},
		Forwarding: network.Forwarding{IPv4: &forward},
		WireGuard:  []network.WireGuard{{Interface: "wg0", PrivateKeyFile: keyFile, ListenPort: 51820, Peers: []network.WireGuardPeer{{PublicKey: peerPublic, Endpoint: "WG-Peer.TEST:51821", AllowedIPs: []string{"198.51.100.20/32"}, PersistentKeepalive: 5}}}},
	}
	doc := config.Native{Network: desired}
	dir := t.TempDir()
	f := startFixtureDaemonAt(t, config.FromNative(doc), dir)
	observed := func() network.WireGuardPeerState {
		var snapshot network.Snapshot
		if err := f.network.Do(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
			return network.WireGuardPeerState{}
		}
		for _, device := range snapshot.WireGuard {
			if device.Interface == "wg0" && len(device.Peers) == 1 {
				return device.Peers[0]
			}
		}
		return network.WireGuardPeerState{}
	}
	traffic := func() {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			port := "18080"
			if protocol == "udp4" {
				port = "18081"
			}
			if err := fixtureInsideNamespace(client, func() error {
				c, err := net.DialTimeout(protocol, "198.51.100.20:"+port, 2*time.Second)
				if err != nil {
					return err
				}
				defer c.Close()
				if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
					return err
				}
				if protocol == "udp4" {
					if _, err := c.Write([]byte("probe")); err != nil {
						return err
					}
				}
				var b [1]byte
				if _, err := io.ReadFull(c, b[:]); err != nil {
					return err
				}
				if b[0] != 'W' {
					return fmt.Errorf("unexpected WireGuard peer response %q", b)
				}
				return nil
			}); err != nil {
				t.Logf("product WireGuard peer: %+v", observed())
				_ = fixtureInsideNamespace(peer, func() error {
					output, err := exec.Command("ip", "route", "get", "198.18.0.2", "from", "198.51.100.20").CombinedOutput()
					t.Logf("peer return route: %s", output)
					return err
				})
				t.Fatal(protocol, err)
			}
		}
	}
	until(t, 35*time.Second, func() bool { return observed().Endpoint == "192.0.2.2:51821" })
	until(t, 10*time.Second, func() bool { return !observed().LastHandshake.IsZero() })
	traffic()
	peerWG("listen-port", "51822")
	if err := fixtureInsideNamespace(peer, func() error {
		return exec.Command("ping", "-I", "wg-peer", "-c", "1", "-W", "2", "198.51.100.10").Run()
	}); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool { return observed().Endpoint == "192.0.2.2:51822" })
	writeDNS("192.0.2.3")
	if err := dns.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(32 * time.Second)
	if observed().Endpoint != "192.0.2.2:51822" {
		t.Fatal("DNS refresh overwrote authenticated roaming")
	}
	traffic()
	t.Log("hostname bootstrap and actual LAN TCP/UDP passed; authenticated port roaming survived DNS rotation")
	beforeOutage := observed().LastHandshake
	setupBGPAddresses(t, peer, []string{"link", "set", "wg-peer", "down"})
	peerWG("listen-port", "51821")
	writeDNS("")
	if err := dns.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 175*time.Second, func() bool {
		paths, _ := filepath.Glob(filepath.Join(dir, "state/logs/*.ndjson"))
		for _, path := range paths {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), "WireGuard endpoint resolution failed") {
				return true
			}
		}
		return false
	})
	if observed().Endpoint != "192.0.2.2:51822" {
		t.Fatal("DNS failure withdrew the retained endpoint")
	}
	writeDNS("192.0.2.3")
	if err := dns.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 35*time.Second, func() bool { return observed().Endpoint == "192.0.2.3:51821" })
	setupBGPAddresses(t, peer, []string{"link", "set", "wg-peer", "up"}, []string{"route", "replace", "198.18.0.0/24", "dev", "wg-peer"})
	until(t, 10*time.Second, func() bool { return observed().LastHandshake.After(beforeOutage) })
	traffic()
	f = f.restart()
	until(t, 35*time.Second, func() bool { return observed().Endpoint == "192.0.2.3:51821" })
	until(t, 10*time.Second, func() bool { return !observed().LastHandshake.IsZero() })
	traffic()
	doc.Network.WireGuard[0].Peers = nil
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-wg-peer", Config: config.FromNative(doc)}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		var snapshot network.Snapshot
		if err := f.network.Do(context.Background(), "GET", "/v1/snapshot", nil, &snapshot); err != nil {
			return false
		}
		for _, device := range snapshot.WireGuard {
			if device.Interface == "wg0" {
				return device.Owned && len(device.Peers) == 0
			}
		}
		return false
	})
	t.Log("natural stale-handshake interval, DNS failure/recovery, re-resolution to the moved peer, daemon restart and peer removal passed")
}
