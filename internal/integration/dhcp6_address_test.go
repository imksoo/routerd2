//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6AddressIndependentServer(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6AddressIndependentServer", 285*time.Second) {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "server0")
	if err := fixtureInsideNamespace(provider, func() error {
		link, err := netlink.LinkByName("server0")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}
		for _, value := range []string{"fe80::2/64", "2001:db8:42::1/64"} {
			a, _ := netlink.ParseAddr(value)
			a.Flags = unix.IFA_F_NODAD
			if err := netlink.AddrAdd(link, a); err != nil {
				return err
			}
		}
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(lo); err != nil {
			return err
		}
		a, _ := netlink.ParseAddr("2001:db8:ffff::9/128")
		return netlink.AddrAdd(lo, a)
	}); err != nil {
		t.Fatal(err)
	}
	server := startFixtureDNSMasq(t, provider, []string{"--dhcp-range=2001:db8:42::100,2001:db8:42::110,64,120s", "--dhcp-option=option6:dns-server,[fe80::2]", "--dhcp-option=option6:domain-search,wan.test", "--port=53", "--no-resolv", "--no-hosts", "--address=/wan.test/2001:db8:ffff::9"})
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	capture := healthPBRCapture(t, "wan0")
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0"}}, Routes: []network.Route{{Destination: "2001:db8:ffff::9/128", Gateway: "fe80::2", Interface: "wan0"}}}, Services: []serviceconfig.Service{{Name: "wan", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 7, RequestAddress: true}}}}
	resolver := dnsconfig.Runtime{Spec: api.DNSResolverSpec{
		Listen:  []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: 1053}},
		Sources: []api.DNSResolverSourceSpec{{Name: "wan", Kind: "forward", Match: []string{"."}, QueryTimeout: "1s", UpstreamFrom: []api.StatusValueSourceSpec{{Resource: "DHCPv6Address/wan", Field: "dnsServers"}}}},
	}}
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "dns", DNS: &resolver})
	f := startFixtureDaemon(t, config.FromNative(doc))
	ctx := context.Background()
	snapshot := func() (dhcp6config.Snapshot, uint64) {
		t.Helper()
		var out runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &out); err != nil {
			t.Fatal(err)
		}
		var s dhcp6config.Snapshot
		if len(out.Snapshot) > 0 {
			if err := json.Unmarshal(out.Snapshot, &s); err != nil {
				t.Fatal(err)
			}
		}
		return s, out.Generation
	}
	module := func() supervisor.ModuleStatus {
		t.Helper()
		var out supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &out); err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Modules {
			if m.Name == "service.wan" {
				return m
			}
		}
		return supervisor.ModuleStatus{}
	}
	installed := func(address string) bool {
		t.Helper()
		link, err := netlink.LinkByName("wan0")
		if err != nil {
			t.Fatal(err)
		}
		all, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range all {
			if a.IP.String() == address && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				return true
			}
		}
		return false
	}
	var first dhcp6config.Snapshot
	until(t, 12*time.Second, func() bool {
		first, _ = snapshot()
		return len(first.Addresses) > 0 && len(first.DNSServers) == 1 && len(first.DomainSearch) == 1 && installed(first.Addresses[0].Address)
	})
	if first.DNSServers[0] != "fe80::2" || first.DomainSearch[0] != "wan.test" {
		t.Fatal("server information was lost", first)
	}
	answer, err := exec.Command("dig", "-6", "@fe80::2%wan0", "host.wan.test", "AAAA", "+short", "+time=2", "+tries=1").CombinedOutput()
	if err != nil || !bytes.Contains(answer, []byte("2001:db8:ffff::9")) {
		t.Fatalf("learned link-local DNS failed on the WAN scope: %s %v", answer, err)
	}
	resolverTraffic := func() {
		t.Helper()
		for _, transport := range [][]string{nil, {"+tcp"}} {
			until(t, 5*time.Second, func() bool {
				args := append([]string{"@127.0.0.1", "-p", "1053", "host.wan.test", "AAAA", "+short", "+time=2", "+tries=1"}, transport...)
				answer, err := exec.Command("dig", args...).CombinedOutput()
				return err == nil && bytes.Equal(bytes.TrimSpace(answer), []byte("2001:db8:ffff::9"))
			})
		}
	}
	address := first.Addresses[0].Address
	fixtureDHCP6Traffic(t, address)
	resolverTraffic()
	initial := module()
	if initial.PID == 0 || !initial.Ready {
		t.Fatal("missing DHCPv6 worker", initial)
	}
	if err := unix.Kill(initial.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var replacement supervisor.ModuleStatus
	var current dhcp6config.Snapshot
	until(t, 12*time.Second, func() bool {
		replacement = module()
		s, generation := snapshot()
		current = s
		return replacement.Ready && replacement.PID != initial.PID && replacement.Generation > initial.Generation && generation == replacement.Generation && len(s.Addresses) > 0 && s.Addresses[0].ValidUntil.Nanos > first.Addresses[0].ValidUntil.Nanos && installed(s.Addresses[0].Address)
	})
	if current.ClientDUID != first.ClientDUID || current.IAID != first.IAID || replacement.ConfigDigest != initial.ConfigDigest {
		t.Fatal("worker restart changed client or prepared identity", first, current, initial, replacement)
	}
	address = current.Addresses[0].Address
	fixtureDHCP6Traffic(t, address)
	resolverTraffic()
	// Wait for an actual Renew/Reply from the independent server, rather than
	// treating Request after restart as renewal coverage.
	acquired := current.Addresses[0].ValidUntil
	until(t, 80*time.Second, func() bool {
		current, _ = snapshot()
		return len(current.Addresses) > 0 && current.Addresses[0].ValidUntil.Nanos > acquired.Nanos+uint64(10*time.Second)
	})
	fixtureDHCP6Traffic(t, address)
	resolverTraffic()
	paused := time.Now()
	if err := unix.Kill(server.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(server.Process.Pid, unix.SIGCONT) })
	expires := current.Addresses[0].ValidUntil
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	if expires.Remaining(clock) > 130*time.Second {
		t.Fatal("fixture server returned an unexpected lease duration", current)
	}
	until(t, expires.Remaining(clock)+8*time.Second, func() bool { current, _ = snapshot(); return len(current.Addresses) == 0 && !installed(address) })
	kinds := map[byte]int{}
	capture.mu.Lock()
	for _, p := range capture.packets {
		b := p.data
		if p.at.Before(paused) || len(b) < 66 || b[20] != 17 || b[54] != 2 || b[55] != 34 || b[56] != 2 || b[57] != 35 {
			continue
		}
		kinds[b[62]]++
	}
	capture.mu.Unlock()
	if kinds[5] == 0 || kinds[6] == 0 {
		t.Fatal("paused server did not exercise Renew and Rebind on the wire", kinds)
	}
	if err := unix.Kill(server.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, 12*time.Second, func() bool {
		current, _ = snapshot()
		return len(current.Addresses) > 0 && installed(current.Addresses[0].Address)
	})
	fixtureDHCP6Traffic(t, current.Addresses[0].Address)
	resolverTraffic()
	if final := module(); final.PID != replacement.PID || final.Generation != replacement.Generation {
		t.Fatal("lease expiry/recovery restarted the client", replacement, final)
	}
	address = current.Addresses[0].Address
	shutdown := time.Now()
	f.stop()
	if installed(address) {
		t.Fatal("graceful shutdown retained its IA_NA address", address)
	}
	until(t, time.Second, func() bool {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		var transaction []byte
		for _, packet := range capture.packets {
			b := packet.data
			if packet.at.Before(shutdown) || len(b) < 66 || b[20] != 17 {
				continue
			}
			if b[54] == 2 && b[55] == 34 && b[56] == 2 && b[57] == 35 && b[62] == 8 {
				if source := netip.AddrFrom16([16]byte(b[22:38])); !source.IsLinkLocalUnicast() {
					t.Fatal("Release used a released address", source)
				}
				transaction = b[63:66]
			}
			if transaction != nil && b[54] == 2 && b[55] == 35 && b[56] == 2 && b[57] == 34 && b[62] == 7 && bytes.Equal(transaction, b[63:66]) {
				return true
			}
		}
		return false
	})
	// The independent server keeps the new lease for 120 seconds but cannot
	// answer Release. Exercise the real retry/shutdown budget before expiry.
	f = f.restart()
	until(t, 12*time.Second, func() bool {
		current, _ = snapshot()
		return len(current.Addresses) > 0 && installed(current.Addresses[0].Address)
	})
	if current.ClientDUID != first.ClientDUID {
		t.Fatal("daemon restart changed DHCPv6 identity")
	}
	address = current.Addresses[0].Address
	if err := unix.Kill(server.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	shutdown = time.Now()
	f.stop()
	if elapsed := time.Since(shutdown); elapsed < 13*time.Second || elapsed > 22*time.Second {
		t.Fatal("unanswered Release did not use its bounded four-transmission exchange", elapsed)
	}
	if installed(address) {
		t.Fatal("unanswered Release retained its IA_NA address", address)
	}
	var transaction []byte
	transmissions, replies := 0, 0
	capture.mu.Lock()
	for _, packet := range capture.packets {
		b := packet.data
		if packet.at.Before(shutdown) || len(b) < 66 || b[20] != 17 {
			continue
		}
		if b[54] == 2 && b[55] == 34 && b[56] == 2 && b[57] == 35 && b[62] == 8 {
			if transaction != nil && !bytes.Equal(transaction, b[63:66]) {
				t.Error("Release retransmission changed transaction")
			}
			transaction = b[63:66]
			transmissions++
		}
		if transaction != nil && b[54] == 2 && b[55] == 35 && b[56] == 2 && b[57] == 34 && b[62] == 7 && bytes.Equal(transaction, b[63:66]) {
			replies++
		}
	}
	capture.mu.Unlock()
	if transmissions != 4 || replies != 0 {
		t.Fatal("unanswered Release wire exchange", transmissions, replies)
	}
	if err := unix.Kill(server.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	t.Log("paused dnsmasq: four Release transmissions with one transaction, no matching Reply, bounded shutdown and no retained address")
	t.Log("dnsmasq IA_NA: source-bound TCP/UDP, stable restart identity, Renew/Rebind/expiry/recovery and graceful kernel withdrawal with link-local Release/Reply")
}

func fixtureDHCP6Traffic(t *testing.T, source string) {
	t.Helper()
	for _, protocol := range []string{"tcp6", "udp6"} {
		port := "18093"
		dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(source)}}
		if protocol == "udp6" {
			port = "18094"
			dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(source)}
		}
		conn, err := dialer.Dial(protocol, net.JoinHostPort("2001:db8:ffff::9", port))
		if err != nil {
			t.Fatal("DHCPv6 source traffic", source, protocol, err)
		}
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if protocol == "udp6" {
			_, err = conn.Write([]byte("lease"))
		}
		var answer [1]byte
		if err == nil {
			_, err = io.ReadFull(conn, answer[:])
		}
		conn.Close()
		if err != nil || answer[0] != 'A' {
			t.Fatal("DHCPv6 TCP/UDP result", source, protocol, answer, err)
		}
	}
}
