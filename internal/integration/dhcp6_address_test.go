//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6RAIndependentPeers(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6RAIndependentPeers", 285*time.Second) {
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

	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	capture := healthPBRCapture(t, "wan0")
	doc := config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0"}}}, Services: []serviceconfig.Service{{Name: "wan", DHCP6: &serviceconfig.DHCP6{RAFrom: "wan-ra", Interface: "wan0", IAID: 7, RequestAddress: true}}, {Name: "wan-ra", RAClient: &serviceconfig.RAClient{Interface: "wan0", SLAAC: new(false), UseMTU: true, UseHopLimit: true}}}}
	resolver := dnsconfig.Runtime{Spec: api.DNSResolverSpec{
		Listen:  []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: 1053}},
		Sources: []api.DNSResolverSourceSpec{{Name: "wan", Kind: "forward", Match: []string{"."}, QueryTimeout: "1s", UpstreamFrom: []api.StatusValueSourceSpec{{Resource: "DHCPv6Address/wan", Field: "dnsServers"}}}},
	}}
	doc.Services = append(doc.Services, serviceconfig.Service{Name: "dns", DNS: &resolver})
	f := startFixtureDaemon(t, config.FromNative(doc))

	until(t, 5*time.Second, func() bool {
		ready := false
		err := fixtureInsideNamespace(provider, func() error {
			link, err := netlink.LinkByName("server0")
			if err != nil {
				return err
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
			if err != nil {
				return err
			}
			ready = len(addresses) > 0
			for _, a := range addresses {
				ready = ready && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return ready
	})
	server := startFixtureDNSMasq(t, provider, []string{"--dhcp-range=2001:db8:42::100,2001:db8:42::110,64,120s", "--dhcp-option=option6:dns-server,[fe80::2]", "--dhcp-option=option6:domain-search,wan.test", "--port=53", "--no-resolv", "--no-hosts", "--address=/wan.test/2001:db8:ffff::9"})
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
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var out supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &out); err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Modules {
			if m.Name == name {
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
	// DHCP is already reachable, but the configured RA-controlled client
	// must wait for the peer's M/O indication before its first exchange.
	until(t, 8*time.Second, func() bool { s, _ := snapshot(); return s.State == "waiting_ra" && module("service.wan").Ready })
	time.Sleep(time.Second)
	capture.mu.Lock()
	for _, p := range capture.packets {
		if len(p.data) >= 63 && p.data[20] == 17 && p.data[54] == 2 && p.data[55] == 34 && p.data[56] == 2 && p.data[57] == 35 {
			t.Error("DHCPv6 started before any RA indication")
		}
	}
	capture.mu.Unlock()
	radvd, err := exec.LookPath("radvd")
	if err != nil {
		t.Fatal("independent radvd required", err)
	}
	work := t.TempDir()
	path := filepath.Join(work, "radvd.conf")
	advertisedPrefix := "2001:db8:42::/64"
	advertise := func(managed, other bool) {
		flag := func(on bool) string {
			if on {
				return "on"
			}
			return "off"
		}
		body := fmt.Sprintf("interface server0 { AdvSendAdvert on; MinRtrAdvInterval 3; MaxRtrAdvInterval 4; AdvDefaultLifetime 12; AdvManagedFlag %s; AdvOtherConfigFlag %s; AdvCurHopLimit 42; AdvLinkMTU 1400; prefix %s { AdvOnLink on; AdvAutonomous off; }; };\n", flag(managed), flag(other), advertisedPrefix)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ipv6Parameter := func(name string) int {
		b, err := os.ReadFile("/proc/sys/net/ipv6/conf/wan0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	baselineMTU, baselineHop := ipv6Parameter("mtu"), ipv6Parameter("hop_limit")
	advertise(false, false)
	log, err := os.Create(filepath.Join(work, "radvd.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	router := exec.CommandContext(peerCtx, radvd, "-n", "-m", "stderr", "-d", "1", "-C", path, "-p", filepath.Join(work, "radvd.pid"))
	router.Stdout, router.Stderr = log, log
	if err := fixtureInsideNamespace(provider, func() error {
		if err := os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0600); err != nil {
			return err
		}
		return router.Start()
	}); err != nil {
		cancelPeer()
		t.Fatal(err)
	}
	peerDone := make(chan error, 1)
	go func() { peerDone <- router.Wait() }()
	t.Cleanup(func() {
		cancelPeer()
		<-peerDone
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Logf("independent radvd: %s", data)
		}
	})
	flags := func() dhcp6config.RouterFlags {
		var out dhcp6config.RouterFlags
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan/dhcp6-input", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	until(t, 8*time.Second, func() bool {
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range routes {
			if (r.Dst == nil || r.Dst.String() == "::/0") && r.Gw.IsLinkLocalUnicast() {
				return true
			}
		}
		return false
	})
	until(t, 5*time.Second, func() bool { return ipv6Parameter("mtu") == 1400 && ipv6Parameter("hop_limit") == 42 })
	link, err := netlink.LinkByName("wan0")
	if err != nil || link.Attrs().MTU != 1500 {
		t.Fatal("RA MTU changed physical/IPv4 MTU", link, err)
	}
	s, _ := snapshot()
	if s.State != "waiting_ra" {
		t.Fatal("clear M/O started DHCP", s)
	}
	advertise(false, true)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 8*time.Second, func() bool {
		s, _ := snapshot()
		return flags().OtherConfig && s.State == "information" && len(s.DNSServers) > 0 && len(s.Addresses) == 0
	})
	generation := module("service.wan").Generation
	advertise(true, true)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
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
	if module("service.wan").Generation != generation || !first.RAStarted.Managed || !first.RAStarted.OtherConfig {
		t.Fatal("RA modes replaced the client or lost its start decision", first, module("service.wan"))
	}
	advertise(false, false)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 8*time.Second, func() bool { return flags() == (dhcp6config.RouterFlags{}) })
	if s, _ := snapshot(); len(s.Addresses) == 0 || !s.RAStarted.Managed || module("service.wan").Generation != generation {
		t.Fatal("cleared RA flags revoked a live DHCP grant", s)
	}
	t.Log("independent radvd M/O: clear waits, O starts information, M acquires IA_NA on one worker; later clear retains the grant")
	address := first.Addresses[0].Address
	fixtureDHCP6Traffic(t, address)
	resolverTraffic()
	// The writer must retain the same original IPv6 baselines after SIGKILL.
	networkBefore := module("network")
	if err := unix.Kill(networkBefore.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 8*time.Second, func() bool {
		m := module("network")
		return m.Ready && m.Generation > networkBefore.Generation && ipv6Parameter("mtu") == 1400 && ipv6Parameter("hop_limit") == 42
	})
	fixtureDHCP6Traffic(t, address)
	lower := doc
	lower.Network.Interfaces = slices.Clone(doc.Network.Interfaces)
	lower.Network.Interfaces[0].MTU = 1300
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "ra-physical-mtu", ConfirmTimeoutSeconds: 5, Config: config.FromNative(lower)}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 4*time.Second, func() bool {
		l, err := netlink.LinkByName("wan0")
		return err == nil && l.Attrs().MTU == 1300 && ipv6Parameter("mtu") == 1300
	})
	fixtureDHCP6Traffic(t, address)
	until(t, 10*time.Second, func() bool {
		l, err := netlink.LinkByName("wan0")
		return err == nil && l.Attrs().MTU == 1500 && ipv6Parameter("mtu") == 1400 && ipv6Parameter("hop_limit") == 42
	})
	fixtureDHCP6Traffic(t, address)
	capture.mu.Lock()
	hopSeen := false
	for _, packet := range capture.packets {
		b := packet.data
		if len(b) >= 54 && b[20] == 6 && b[21] == 42 && netip.AddrFrom16([16]byte(b[22:38])).String() == address {
			hopSeen = true
		}
	}
	capture.mu.Unlock()
	if !hopSeen {
		t.Fatal("no actual TCP packet used the advertised hop limit")
	}
	t.Log("RA IPv6 MTU/hop limit applied without changing physical MTU, survived writer crash and physical MTU1300/confirmed rollback")
	initial := module("service.wan")
	if initial.PID == 0 || !initial.Ready {
		t.Fatal("missing DHCPv6 worker", initial)
	}
	beforeRestart, _ := snapshot()
	restartAt := time.Now()
	if err := unix.Kill(initial.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var replacement supervisor.ModuleStatus
	var current dhcp6config.Snapshot
	until(t, 12*time.Second, func() bool {
		replacement = module("service.wan")
		s, generation := snapshot()
		current = s
		return replacement.Ready && replacement.PID != initial.PID && replacement.Generation > initial.Generation && generation == replacement.Generation && s.State == "bound" && len(s.Addresses) > 0 && installed(s.Addresses[0].Address)
	})
	if current.ClientDUID != first.ClientDUID || current.IAID != first.IAID || replacement.ConfigDigest != initial.ConfigDigest || !slices.Equal(current.Addresses, beforeRestart.Addresses) || current.RenewAt != beforeRestart.RenewAt || current.RebindAt != beforeRestart.RebindAt {
		t.Fatal("worker restart changed client or prepared identity", first, current, initial, replacement)
	}
	capture.mu.Lock()
	confirmations := map[string]bool{}
	confirmed := false
	for _, p := range capture.packets {
		b := p.data
		if p.at.Before(restartAt) || len(b) < 66 || b[20] != 17 {
			continue
		}
		xid := string(b[63:66])
		if binary.BigEndian.Uint16(b[54:56]) == 546 && binary.BigEndian.Uint16(b[56:58]) == 547 && b[62] == 4 {
			confirmations[xid] = true
		}
		if binary.BigEndian.Uint16(b[54:56]) == 547 && binary.BigEndian.Uint16(b[56:58]) == 546 && b[62] == 7 && confirmations[xid] {
			options, err := dhcp6FixtureOptions(b[66:])
			if err != nil {
				capture.mu.Unlock()
				t.Fatal(err)
			}
			status := uint16(0)
			for _, o := range options {
				if o.code == 13 && len(o.value) >= 2 {
					status = binary.BigEndian.Uint16(o.value)
				}
			}
			confirmed = confirmed || status == 0
		}
	}
	capture.mu.Unlock()
	if !confirmed {
		t.Fatal("no successful real Confirm/Reply after IA_NA worker replacement")
	}
	t.Log("independent dnsmasq Confirm accepted the same saved IA_NA without changing T1/T2 or assignment deadlines")
	address = current.Addresses[0].Address
	fixtureDHCP6Traffic(t, address)
	resolverTraffic()
	// A silent server cannot turn a Confirm into a renewed grant. After
	// the bounded exchange, the same original lease can be used again.
	quiet := current
	priorWorker := replacement
	if err := unix.Kill(server.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(server.Process.Pid, unix.SIGCONT) })
	quietAt := time.Now()
	if err := unix.Kill(priorWorker.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		replacement = module("service.wan")
		current, generation := snapshot()
		return replacement.Generation > priorWorker.Generation && generation == replacement.Generation && current.State == "confirming"
	})
	until(t, 15*time.Second, func() bool {
		current, _ = snapshot()
		return (current.State == "bound" || current.State == "renewing" || current.State == "rebinding") && len(current.Addresses) > 0 && installed(current.Addresses[0].Address)
	})
	if time.Since(quietAt) < 10*time.Second || !slices.Equal(current.Addresses, quiet.Addresses) || current.RenewAt != quiet.RenewAt || current.RebindAt != quiet.RebindAt || current.ClientDUID != quiet.ClientDUID {
		t.Fatal("silent Confirm changed the original grant or skipped its reply window", quiet, current, time.Since(quietAt))
	}
	fixtureDHCP6Traffic(t, address)
	if err := unix.Kill(server.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	resolverTraffic()
	t.Log("silent Confirm completed its ten-second window and retained the same original deadlines and usable TCP/UDP")
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
	if final := module("service.wan"); final.PID != replacement.PID || final.Generation != replacement.Generation {
		t.Fatal("lease expiry/recovery restarted the client", replacement, final)
	}
	// Move the independent server/router to another on-link prefix. Its
	// real NotOnLink must discard the old IA_NA and discover on the new link.
	oldAddress := current.Addresses[0].Address
	if err := fixtureInsideNamespace(provider, func() error {
		l, err := netlink.LinkByName("server0")
		if err != nil {
			return err
		}
		a, _ := netlink.ParseAddr("2001:db8:43::1/64")
		if err := netlink.AddrAdd(l, a); err != nil {
			return err
		}
		old, _ := netlink.ParseAddr("2001:db8:42::1/64")
		return netlink.AddrDel(l, old)
	}); err != nil {
		t.Fatal(err)
	}
	if err := server.Process.Signal(unix.SIGTERM); err != nil {
		t.Fatal(err)
	}
	until(t, 2*time.Second, func() bool { return unix.Kill(server.Process.Pid, 0) == unix.ESRCH })
	until(t, 5*time.Second, func() bool {
		ready := false
		if err := fixtureInsideNamespace(provider, func() error {
			l, err := netlink.LinkByName("server0")
			if err != nil {
				return err
			}
			all, err := netlink.AddrList(l, netlink.FAMILY_V6)
			for _, a := range all {
				ready = ready || a.IP.String() == "2001:db8:43::1" && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	})
	server = startFixtureDNSMasq(t, provider, []string{"--dhcp-range=2001:db8:43::100,2001:db8:43::110,64,120s", "--dhcp-option=option6:dns-server,[fe80::2]", "--dhcp-option=option6:domain-search,wan.test", "--port=53", "--no-resolv", "--no-hosts", "--address=/wan.test/2001:db8:ffff::9"})
	advertisedPrefix = "2001:db8:43::/64"
	advertise(false, false)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	movedAt := time.Now()
	if err := unix.Kill(replacement.PID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 20*time.Second, func() bool {
		current, _ = snapshot()
		return current.State == "bound" && len(current.Addresses) > 0 && strings.HasPrefix(current.Addresses[0].Address, "2001:db8:43:") && installed(current.Addresses[0].Address)
	})
	if installed(oldAddress) || current.ClientDUID != first.ClientDUID {
		t.Fatal("NotOnLink retained the old address or changed DUID", oldAddress, current)
	}
	fixtureDHCP6Traffic(t, current.Addresses[0].Address)
	resolverTraffic()
	capture.mu.Lock()
	rejected := false
	for _, p := range capture.packets {
		b := p.data
		if p.at.Before(movedAt) || len(b) < 66 || b[20] != 17 || binary.BigEndian.Uint16(b[54:56]) != 547 || binary.BigEndian.Uint16(b[56:58]) != 546 || b[62] != 7 {
			continue
		}
		opts, err := dhcp6FixtureOptions(b[66:])
		if err != nil {
			capture.mu.Unlock()
			t.Fatal(err)
		}
		for _, o := range opts {
			rejected = rejected || o.code == 13 && len(o.value) >= 2 && binary.BigEndian.Uint16(o.value) == 4
		}
	}
	capture.mu.Unlock()
	if !rejected {
		t.Fatal("independent moved-link server did not send NotOnLink")
	}
	t.Log("independent dnsmasq NotOnLink withdrew the old IA_NA and reacquired on the new prefix with the same DUID and TCP/UDP")
	address = current.Addresses[0].Address
	cancelPeer()
	until(t, 2*time.Second, func() bool { return unix.Kill(router.Process.Pid, 0) == unix.ESRCH })
	shutdown := time.Now()
	f.stop()
	if ipv6Parameter("mtu") != baselineMTU || ipv6Parameter("hop_limit") != baselineHop {
		t.Fatal("RA parameter withdrawal lost the original kernel baselines", baselineMTU, baselineHop, ipv6Parameter("mtu"), ipv6Parameter("hop_limit"))
	}
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
