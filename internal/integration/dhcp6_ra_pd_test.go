//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6RAAddsAddressToPD(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6RAAddsAddressToPD", 140*time.Second) {
		return
	}
	provider := fixturePeerNamespace(t, "wire0", "server0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	duplicate := fixturePeerNamespace(t, "dup0", "duplicate0")
	if err := fixtureInsideNamespace(provider, func() error {
		for name, value := range map[string]string{"server0": "2001:db8:42::1/64", "lo": "2001:db8:ffff::9/128"} {
			link, err := netlink.LinkByName(name)
			if err != nil {
				return err
			}
			address, _ := netlink.ParseAddr(value)
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
		}
		return os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(duplicate, func() error {
		l, err := netlink.LinkByName("duplicate0")
		if err != nil {
			return err
		}
		for _, value := range []string{"2001:db8:42::100/128", "2001:db8:42::101/128"} {
			a, _ := netlink.ParseAddr(value)
			if err := netlink.AddrAdd(l, a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(client, func() error {
		for name, value := range map[string]string{"accept_ra": "2", "autoconf": "1", "forwarding": "0"} {
			if err := os.WriteFile("/proc/sys/net/ipv6/conf/client0/"+name, []byte(value), 0600); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	wan := healthPBRCapture(t, "wire0")
	lan := healthPBRCapture(t, "lan0")
	bridge := "wan0"
	doc := config.Native{Network: network.Desired{Links: []network.Link{{Name: bridge, Kind: "bridge"}}, Interfaces: []network.Interface{{Name: "wire0", Master: &bridge}, {Name: "dup0", Master: &bridge}, {Name: "wan0"}, {Name: "lan0"}}, Forwarding: network.Forwarding{IPv6: new(true)}}, Services: []serviceconfig.Service{
		{Name: "lan-info", DHCP6Server: &serviceconfig.DHCP6Server{Interface: "lan0", OptionsFrom: []string{"wan"}, DomainSearch: []string{"local.test"}}},
		{Name: "wan-ra", RAClient: &serviceconfig.RAClient{Interface: "wan0", SLAAC: new(false)}},
		{Name: "wan", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", RAFrom: "wan-ra", IAID: 7, RequestPrefix: true, RequestAddress: true, PrefixLength: 60, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", Advertise: true, OtherConfig: true}}}},
	}}
	f := startFixtureDaemon(t, config.FromNative(doc))
	until(t, 5*time.Second, func() bool {
		ready := false
		if err := fixtureInsideNamespace(provider, func() error {
			l, err := netlink.LinkByName("server0")
			if err != nil {
				return err
			}
			addresses, err := netlink.AddrList(l, netlink.FAMILY_V6)
			ready = len(addresses) >= 2
			for _, a := range addresses {
				ready = ready && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	})
	work := t.TempDir()
	start := func(name, executable string, args ...string) *exec.Cmd {
		t.Helper()
		log, err := os.Create(filepath.Join(work, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		ctx, cancel := context.WithCancel(context.Background())
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Stdout, cmd.Stderr = log, log
		if err := fixtureInsideNamespace(provider, cmd.Start); err != nil {
			cancel()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() { cancel(); <-done })
		t.Cleanup(func() {
			if t.Failed() {
				b, _ := os.ReadFile(log.Name())
				t.Log(name, string(b))
			}
		})
		return cmd
	}
	isc := os.Getenv("ROUTERD2_DHCP6_SERVER")
	if isc == "" {
		t.Fatal("independent ISC DHCP server required")
	}
	cf, leases := filepath.Join(work, "dhcp6.conf"), filepath.Join(work, "dhcp6.leases")
	if err := os.WriteFile(cf, []byte("default-lease-time 30; preferred-lifetime 20; option dhcp-renewal-time 10; option dhcp-rebinding-time 20; option dhcp6.name-servers 2001:db8:42::1; option dhcp6.domain-search \"wan.test\"; option dhcp6.sntp-servers 2001:db8:42::1; subnet6 2001:db8:42::/64 { range6 2001:db8:42::100 2001:db8:42::101; prefix6 2001:db8:80:: 2001:db8:80:f0:: /60; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leases, nil, 0600); err != nil {
		t.Fatal(err)
	}
	startFixtureDNSMasq(t, provider, []string{"--port=53", "--no-resolv", "--no-hosts", "--address=/wan.test/2001:db8:ffff::9"})
	server := start("isc", isc, "-6", "-f", "-d", "-cf", cf, "-lf", leases, "-pf", filepath.Join(work, "dhcp6.pid"), "server0")
	raPath := filepath.Join(work, "radvd.conf")
	advertise := func(managed bool) {
		flag := "off"
		if managed {
			flag = "on"
		}
		if err := os.WriteFile(raPath, []byte(fmt.Sprintf("interface server0 { AdvSendAdvert on; MinRtrAdvInterval 3; MaxRtrAdvInterval 4; AdvDefaultLifetime 12; AdvManagedFlag %s; prefix 2001:db8:42::/64 { AdvOnLink on; AdvAutonomous off; }; };\n", flag)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	advertise(false)
	router := start("radvd", "radvd", "-n", "-m", "stderr", "-d", "1", "-C", raPath, "-p", filepath.Join(work, "radvd.pid"))
	ctx := context.Background()
	var state runtimeplan.InstanceStatus
	read := func() dhcp6config.Snapshot {
		t.Helper()
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &state); err != nil {
			t.Fatal(err)
		}
		var s dhcp6config.Snapshot
		if len(state.Snapshot) > 0 {
			if err := json.Unmarshal(state.Snapshot, &s); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	var first dhcp6config.Snapshot
	until(t, 12*time.Second, func() bool { first = read(); return len(first.Prefixes) == 1 && first.State == "bound" })
	if len(first.Addresses) != 0 || first.RAStarted.Managed {
		t.Fatal("clear M unexpectedly requested IA_NA", first)
	}
	generation := state.Generation
	// ISC supplies the grant; the independent delegating router supplies its
	// return route, just as the PPP peer does through its point-to-point link.
	link, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}
	var gateway net.IP
	for _, a := range addresses {
		if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
			gateway = a.IP
		}
	}
	if err := fixtureInsideNamespace(provider, func() error {
		l, err := netlink.LinkByName("server0")
		if err != nil {
			return err
		}
		_, pool, _ := net.ParseCIDR("2001:db8:80::/56")
		return netlink.RouteAdd(&netlink.Route{Dst: pool, Gw: gateway, LinkIndex: l.Attrs().Index})
	}); err != nil {
		t.Fatal(err)
	}
	traffic := func() {
		t.Helper()
		var source string
		until(t, 12*time.Second, func() bool {
			ready := false
			if err := fixtureInsideNamespace(client, func() error {
				l, err := netlink.LinkByName("client0")
				if err != nil {
					return err
				}
				all, err := netlink.AddrList(l, netlink.FAMILY_V6)
				if err != nil {
					return err
				}
				for _, a := range all {
					if a.IP.IsGlobalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
						source = a.IP.String()
					}
				}
				routes, err := netlink.RouteList(l, netlink.FAMILY_V6)
				for _, r := range routes {
					ready = ready || (r.Dst == nil || r.Dst.String() == "::/0") && r.Gw.IsLinkLocalUnicast()
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return source != "" && ready
		})
		if err := fixtureInsideNamespace(client, func() error { fixtureDHCP6Traffic(t, source); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	traffic()
	information := func() {
		t.Helper()
		// WAN acquisition precedes the server's periodic input refresh. Wait for
		// its actual client-visible options, not just the producer's bound state.
		var got dhcp6FixtureInformation
		until(t, 4*time.Second, func() bool {
			got = fixtureDHCP6Information(t, client, "client0")
			return slices.Equal(got.dns, []netip.Addr{netip.MustParseAddr("2001:db8:42::1")}) && slices.Contains(got.search, "wan.test") && slices.Contains(got.search, "local.test")
		})
		if err := fixtureInsideNamespace(client, func() error {
			for _, transport := range []string{"+notcp", "+tcp"} {
				answer, err := exec.Command("dig", "-6", "@"+got.dns[0].String(), "host.wan.test", "AAAA", "+short", "+time=2", "+tries=1", transport).CombinedOutput()
				if err != nil || !strings.Contains(string(answer), "2001:db8:ffff::9") {
					return fmt.Errorf("client DNS %s: %v: %s", transport, err, answer)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	information()
	changed := time.Now()
	advertise(true)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	// The independent Linux peer already owns the two address candidates.
	// Exercise kernel DAD and a real ISC Decline while PD still forwards.
	until(t, 12*time.Second, func() bool {
		wan.mu.Lock()
		defer wan.mu.Unlock()
		for _, p := range wan.packets {
			b := p.data
			if !p.at.Before(changed) && len(b) >= 66 && b[20] == 17 && binary.BigEndian.Uint16(b[54:56]) == 546 && binary.BigEndian.Uint16(b[56:58]) == 547 && b[62] == 9 {
				return true
			}
		}
		return false
	})
	afterDecline := read()
	if len(afterDecline.Prefixes) != 1 || afterDecline.Prefixes[0].Prefix != first.Prefixes[0].Prefix || state.Generation != generation {
		t.Fatal("IA_NA DAD failure lost the existing delegation", first, afterDecline)
	}
	traffic()
	if err := fixtureInsideNamespace(duplicate, func() error {
		l, err := netlink.LinkByName("duplicate0")
		if err != nil {
			return err
		}
		for _, value := range []string{"2001:db8:42::100/128", "2001:db8:42::101/128"} {
			a, _ := netlink.ParseAddr(value)
			if err := netlink.AddrDel(l, a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	addressReady := func(address string) bool {
		t.Helper()
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
	var current dhcp6config.Snapshot
	until(t, 15*time.Second, func() bool {
		current = read()
		return len(current.Addresses) > 0 && len(current.Prefixes) == 1 && current.State == "bound" && addressReady(current.Addresses[0].Address)
	})
	if state.Generation != generation || current.ClientDUID != first.ClientDUID || current.IAID != first.IAID || current.Prefixes[0].Prefix != first.Prefixes[0].Prefix {
		t.Fatal("adding IA_NA replaced the client or existing delegation", first, current, generation, state.Generation)
	}
	traffic()
	fixtureDHCP6Traffic(t, current.Addresses[0].Address)
	if err := unix.Kill(server.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	retained := read()

	// A crash must recover both bindings using the original PD grant and the
	// same identity, even after the current RA clears its start indication.
	advertise(false)
	if err := router.Process.Signal(unix.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 8*time.Second, func() bool {
		var flags dhcp6config.RouterFlags
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan/dhcp6-input", nil, &flags); err != nil {
			t.Fatal(err)
		}
		return !flags.Managed
	})
	var modules supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	for _, m := range modules.Modules {
		if m.Name == "service.wan" {
			if err := unix.Kill(m.PID, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
	}
	until(t, 8*time.Second, func() bool {
		current = read()
		return state.Generation > generation && len(current.Prefixes) == 1 && len(current.Addresses) > 0 && addressReady(current.Addresses[0].Address)
	})
	if current.ClientDUID != retained.ClientDUID || current.Prefixes[0] != retained.Prefixes[0] || current.Addresses[0] != retained.Addresses[0] || !current.RAStarted.Managed {
		t.Fatal("combined IA recovery changed identity, grant or original deadlines", retained, current)
	}
	traffic()
	fixtureDHCP6Traffic(t, current.Addresses[0].Address)
	if err := unix.Kill(server.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, 12*time.Second, func() bool {
		current = read()
		return current.State == "bound" && len(current.Prefixes) == 1 && len(current.Addresses) > 0 && current.Prefixes[0].ValidUntil.Nanos > retained.Prefixes[0].ValidUntil.Nanos
	})
	traffic()
	information()
	wan.mu.Lock()
	combined := false
	for _, packet := range wan.packets {
		b := packet.data
		if packet.at.Before(changed) || len(b) < 66 || b[20] != 17 || binary.BigEndian.Uint16(b[54:56]) != 546 || binary.BigEndian.Uint16(b[56:58]) != 547 {
			continue
		}
		na, pd := false, false
		opts, err := dhcp6FixtureOptions(b[66:])
		if err != nil {
			wan.mu.Unlock()
			t.Fatal(err)
		}
		for _, o := range opts {
			if (o.code == 3 || o.code == 25) && len(o.value) >= 12 && binary.BigEndian.Uint32(o.value) == 7 {
				na, pd = na || o.code == 3, pd || o.code == 25
			}
		}

		combined = combined || na && pd
	}
	wan.mu.Unlock()
	if !combined {
		t.Fatal("no actual request carried both IA_NA and IA_PD with the shared IAID")
	}
	// Freeze the original authority and input owner. The information server
	// must expire its learned bytes itself while static search remains usable.
	if err := unix.Kill(server.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	deadline, live := current.InformationUntil(clock)
	if !live {
		t.Fatal("no live information grant", current)
	}
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	controlPID := 0
	for _, m := range modules.Modules {
		if m.Name == "control" {
			controlPID = m.PID
		}
	}
	if controlPID == 0 {
		t.Fatal("missing control")
	}
	// Leave the supervisor running. Pause control shortly before the finite
	// deadline so both client requests fit inside its real heartbeat budget.
	until(t, 35*time.Second, func() bool {
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		return deadline.Nanos != 0 && uint64(clock.BoottimeNanoseconds)+uint64(2*time.Second) >= deadline.Nanos
	})
	if err := unix.Kill(controlPID, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(controlPID, unix.SIGCONT) })
	beforeExpiry := fixtureDHCP6Information(t, client, "client0")
	if !slices.Contains(beforeExpiry.search, "wan.test") || len(beforeExpiry.dns) != 1 {
		t.Fatal("learned authority was absent before paused-control expiry", beforeExpiry)
	}
	until(t, 4*time.Second, func() bool {
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		return !deadline.Live(clock)
	})
	got := fixtureDHCP6Information(t, client, "client0")
	if len(got.dns) != 0 || !slices.Equal(got.search, []string{"local.test"}) {
		t.Fatal("expired learned options survived or static search disappeared", got)
	}
	if err := unix.Kill(controlPID, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(server.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, 12*time.Second, func() bool {
		current = read()
		return len(current.Prefixes) > 0 && current.Prefixes[0].ValidUntil.Nanos > deadline.Nanos
	})
	traffic()
	information()
	lan.mu.Lock()
	learnedSNTP, otherConfig := false, false
	for _, p := range lan.packets {
		b := p.data
		otherConfig = otherConfig || len(b) >= 70 && b[20] == 58 && b[54] == 134 && b[59]&0x40 != 0
		if len(b) < 66 || b[20] != 17 || binary.BigEndian.Uint16(b[54:]) != 547 || binary.BigEndian.Uint16(b[56:]) != 546 || b[62] != 7 {
			continue
		}
		opts, err := dhcp6FixtureOptions(b[66:])
		if err != nil {
			lan.mu.Unlock()
			t.Fatal(err)
		}
		for _, o := range opts {
			learnedSNTP = learnedSNTP || o.code == 31 && bytes.Equal(o.value, netip.MustParseAddr("2001:db8:42::1").AsSlice())
		}
	}
	lan.mu.Unlock()
	if !learnedSNTP || !otherConfig {
		t.Fatal("missing learned SNTP or configured RA O flag", learnedSNTP, otherConfig)
	}
	// An explicit client identity is trial intent; reverting it must not
	// adopt that trial's checkpoint identity as the new automatic default.
	trial := doc
	trial.Services = slices.Clone(doc.Services)
	const trialDUID = "00030001020000000199"
	for i, s := range trial.Services {
		if s.DHCP6 == nil {
			continue
		}
		cfg := *s.DHCP6
		cfg.ClientDUID = trialDUID
		trial.Services[i].DHCP6 = &cfg
	}
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "wan-identity-trial", Config: config.FromNative(trial), ConfirmTimeoutSeconds: 5}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 4*time.Second, func() bool {
		current = read()
		return current.ClientDUID == trialDUID && len(current.Prefixes) == 1 && current.State == "bound"
	})
	var operator control.State
	until(t, 12*time.Second, func() bool {
		operator = control.State{}
		return f.control.Do(ctx, "GET", "/v1/config", nil, &operator) == nil && operator.Revision == 3 && operator.Confirmation == nil && operator.Healthy
	})
	until(t, 8*time.Second, func() bool { current = read(); return len(current.Prefixes) == 1 && current.State == "bound" })
	if current.ClientDUID != first.ClientDUID {
		t.Fatal("WAN rollback replaced automatic DUID with trial identity", first.ClientDUID, current.ClientDUID)
	}
	traffic()
	information()
	f.stop()
	t.Log("ISC/radvd: explicit PD before M, same-worker IA_NA addition, Linux duplicate/DAD/Decline without losing delegated LAN SLAAC/TCP/UDP, original combined grants after SIGKILL and Rebind recovery; BusyBox PD-derived DNS/search with actual UDP/TCP DNS and original expiry during stopped control, static option retention and recovery")
}
