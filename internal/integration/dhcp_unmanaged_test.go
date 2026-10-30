//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/vishvananda/netlink"
)

// The external link owner, static address and DHCP server have separate intent.
const unmanagedDHCPFixture = `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: existing-lan}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: lan}
      spec: {ifname: lan0, managed: false, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: server}
      spec: {interface: lan, address: 192.0.2.1/24}
    - apiVersion: net.routerd.net/v1alpha1
      kind: DHCPv4Server
      metadata: {name: leases}
      spec:
        interface: lan
        addressPool: {start: 192.0.2.100, end: 192.0.2.102, leaseTime: 120s}
        gateway: 192.0.2.1
        dnsServers: [192.0.2.1]
`

func TestDaemonMigratedDHCPUnmanagedLAN(t *testing.T) {
	if !isolateFixture(t, "TestDaemonMigratedDHCPUnmanagedLAN") {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "client0")
	link, err := netlink.LinkByName("lan0")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMTU(link, 1472); err != nil {
		t.Fatal(err)
	}
	other, err := netlink.ParseAddr("198.51.100.1/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(link, other); err != nil {
		t.Fatal(err)
	}
	originalIndex := link.Attrs().Index
	checkLink := func(up, ownedAddress bool) {
		t.Helper()
		live, err := netlink.LinkByName("lan0")
		if err != nil {
			t.Fatal(err)
		}
		if live.Attrs().Index != originalIndex || live.Attrs().MTU != 1472 || (live.Attrs().Flags&net.FlagUp != 0) != up || live.Attrs().MasterIndex != 0 {
			t.Fatal("existing link identity, MTU, membership or administrative state changed", live.Attrs())
		}
		addresses, err := netlink.AddrList(live, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		var own, external bool
		for _, a := range addresses {
			own = own || a.IPNet.String() == "192.0.2.1/24"
			external = external || a.IPNet.String() == "198.51.100.1/24"
		}
		if own != ownedAddress || !external {
			t.Fatal("explicit address or unrelated address changed", addresses)
		}
	}
	checkLink(false, false)
	imported, err := legacyconfig.Compile([]byte(unmanagedDHCPFixture))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := config.Compile(imported.Document)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := network.New(network.Options{StatePath: filepath.Join(t.TempDir(), "network.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Apply(context.Background(), 1, plan.Native.Network); err != nil {
		t.Fatal(err)
	}
	checkLink(false, true)
	managed := plan.Native.Network
	managed.Interfaces = append([]network.Interface(nil), managed.Interfaces...)
	managed.Interfaces[0].AdminState = network.AdminUp
	if _, err := engine.Apply(context.Background(), 2, managed); err != nil {
		t.Fatal(err)
	}
	checkLink(true, true)
	if _, err := engine.Apply(context.Background(), 3, plan.Native.Network); err != nil {
		t.Fatal(err)
	}
	checkLink(false, true)
	if _, err := engine.Apply(context.Background(), 4, network.Desired{}); err != nil {
		t.Fatal(err)
	}
	checkLink(false, false)
	t.Log("DOWN existing LAN: explicit address applied and removed; link state, MTU, identity and unrelated address preserved")

	// The environment owner brings its link up; product configuration does not.
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	f := startFixtureDaemonAt(t, imported.Document, work)
	readyAt := time.Now()
	checkLink(true, true)
	report, hook := filepath.Join(work, "lease.txt"), filepath.Join(work, "lease.sh")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
bound|renew)
  busybox ifconfig "$interface" "$ip" netmask "$subnet"
  set -- $router
  gateway="$1"
  busybox route add default gw "$gateway" dev "$interface"
  printf '%%s %%s\n' "$ip" "$gateway" > %s
  ;;
esac
`, report)
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	client := exec.Command("busybox", "udhcpc", "-f", "-n", "-q", "-i", "client0", "-s", hook)
	var output bytes.Buffer
	client.Stdout, client.Stderr = &output, &output
	started := time.Now()
	if err := fixtureInsideNamespace(peer, client.Start); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("independent BusyBox acquisition: %v\n%s", err, output.String())
	}
	t.Logf("product ready observed %s; DHCP client started %s and completed in %s\n%s", readyAt.UTC().Format(time.RFC3339Nano), started.UTC().Format(time.RFC3339Nano), time.Since(started), output.String())
	logs := localapi.NewClient(filepath.Join(work, "run", logging.SocketName))
	var records logging.QueryResult
	until(t, 5*time.Second, func() bool {
		if err := logs.Do(context.Background(), "GET", "/v1/logs?limit=100", nil, &records); err != nil {
			t.Fatal(err)
		}
		for _, record := range records.Records {
			if strings.Contains(record.Message, "DHCPACK(lan0)") {
				return true
			}
		}
		return false
	})
	for _, record := range records.Records {
		if strings.Contains(record.Message, "dnsmasq") || record.Message == "ROUTERD_READY" {
			t.Logf("product event %s: %s", record.Timestamp, record.Message)
		}
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	lease := strings.Fields(string(data))
	if len(lease) != 2 {
		t.Fatal("client did not receive address and gateway", string(data))
	}
	address, gateway := lease[0], lease[1]
	if address != "192.0.2.100" && address != "192.0.2.101" && address != "192.0.2.102" {
		t.Fatal("client did not acquire a configured pool address", address)
	}
	ping := exec.Command("busybox", "ping", "-c", "1", "-W", "2", gateway)
	output.Reset()
	ping.Stdout, ping.Stderr = &output, &output
	if err := fixtureInsideNamespace(peer, ping.Start); err != nil {
		t.Fatal(err)
	}
	if err := ping.Wait(); err != nil {
		t.Fatalf("communication using the DHCP address and gateway: %v\n%s", err, output.String())
	}
	t.Logf("independent client applied DHCP address %s and reached its learned gateway %s", address, gateway)
	checkLink(true, true)
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-dhcp", Config: config.FromNative(config.Native{})}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return false
		}
		for _, a := range addresses {
			if a.IPNet.String() == "192.0.2.1/24" {
				return false
			}
		}
		return true
	})
	checkLink(true, false)
	f.stop()
	checkLink(true, false)
	t.Logf("UP existing LAN: independent BusyBox acquired %s; configuration removal and shutdown preserved external administrative state and unrelated address", address)
}
