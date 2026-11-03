//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6StatefulIndependentClient(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6StatefulIndependentClient", 400*time.Second) {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "server0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	healthPBRCapture(t, "wan0")
	capture := healthPBRCapture(t, "lan0")
	resolver := dnsconfig.RuntimeConfig{
		Spec: api.DNSResolverSpec{
			Listen:  []api.DNSResolverListenSpec{{Name: "lan", Addresses: []string{"198.18.0.1", "::"}, Port: 53}},
			Sources: []api.DNSResolverSourceSpec{{Name: "local", Kind: "zone", Match: []string{"lan.test", "ip6.arpa", "0.18.198.in-addr.arpa"}, ZoneRef: []string{"lan"}}},
		},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lan.test", DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/addresses4", "DHCPv6Server/addresses"}}}}},
	}
	native := config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"198.18.0.1/24"}}, {Name: "wan0", Addresses: []string{"2001:db8:42::2/64"}}}, Routes: []network.Route{{Destination: "::/0", Gateway: "2001:db8:42::1", Interface: "wan0"}}, Forwarding: network.Forwarding{IPv6: new(true)}},
		Services: []serviceconfig.Service{
			{Name: "addresses4", DHCPServer: &serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "198.18.0.1", Authoritative: true, Primary: dhcpserverconfig.Scope{Prefix: "198.18.0.0/24", Start: "198.18.0.100", End: "198.18.0.199", LeaseSeconds: 600, Router: "198.18.0.1", DNSServers: []string{"198.18.0.1"}, Domain: "lan.test"}}},
			{Name: "addresses", DHCP6Server: &serviceconfig.DHCP6Server{Interface: "lan0", LogDHCP: true, Pool: &dhcp6serverconfig.Pool{Start: netip.MustParseAddr("::100"), End: netip.MustParseAddr("::1ff"), LeaseSeconds: 120}, DNSDelegation: &serviceconfig.DNSDelegation{Service: "wan-pd", Delegation: "lan"}, DomainSearch: []string{"lan.test"}}},
			{Name: "lan-ra", RA: &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: "lan0", Managed: true, OtherConfig: true, RouterLifetimeSeconds: 1800, DNSSLifetimeSeconds: 1800}, From: &serviceconfig.RADelegation{Service: "wan-pd", Delegation: "lan", RDNSSRouter: true}}},
			{Name: "dns", DNS: &serviceconfig.DNS{ListenInterface: "lan0", RuntimeConfig: resolver}},
			{Name: "wan-pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 1, RequestPrefix: true, PrefixLength: 60, Delegations: []serviceconfig.Delegation{{Name: "lan", Interface: "lan0", HostID: 1}}}},
		},
	}
	f := startFixtureDaemon(t, config.FromNative(native))
	work := t.TempDir()
	if err := fixtureInsideNamespace(provider, func() error {
		for name, cidr := range map[string]string{"server0": "2001:db8:42::1/64", "lo": "2001:db8:ffff::9/128"} {
			link, err := netlink.LinkByName(name)
			if err != nil {
				return err
			}
			address, err := netlink.ParseAddr(cidr)
			if err != nil {
				return err
			}
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
		}
		return os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	iscConfig, iscLeases := filepath.Join(work, "isc.conf"), filepath.Join(work, "isc.leases")
	if err := os.WriteFile(iscLeases, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var isc *exec.Cmd
	var iscLog *os.File
	stopISC := func() {
		if isc == nil {
			return
		}
		_ = isc.Process.Signal(unix.SIGTERM)
		_ = isc.Wait()
		iscLog.Close()
		if t.Failed() {
			data, _ := os.ReadFile(iscLog.Name())
			if len(data) > 6000 {
				data = data[len(data)-6000:]
			}
			t.Logf("independent ISC: %s", data)
		}
		isc = nil
	}
	t.Cleanup(stopISC)
	startISC := func(prefix string) {
		t.Helper()
		stopISC()
		body := fmt.Sprintf("default-lease-time 60; preferred-lifetime 45; option dhcp-renewal-time 20; option dhcp-rebinding-time 40; subnet6 2001:db8:42::/64 { prefix6 %s %s /60; }\n", prefix, prefix)
		if err := os.WriteFile(iscConfig, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		var err error
		iscLog, err = os.Create(filepath.Join(work, "isc.log"))
		if err != nil {
			t.Fatal(err)
		}
		isc = exec.Command(os.Getenv("ROUTERD2_DHCP6_SERVER"), "-6", "-f", "-d", "-cf", iscConfig, "-lf", iscLeases, "-pf", filepath.Join(work, "isc.pid"), "server0")
		isc.Stdout, isc.Stderr = iscLog, iscLog
		if err := fixtureInsideNamespace(provider, isc.Start); err != nil {
			t.Fatal(err)
		}
	}
	startISC("2001:db8:80::")
	var lanPrefix netip.Prefix
	readPD := func() bool {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(t.Context(), "GET", "/v1/services/service.wan-pd", nil, &status); err != nil {
			return false
		}
		var observed dhcp6config.Snapshot
		if err := json.Unmarshal(status.Snapshot, &observed); err != nil || len(observed.Prefixes) == 0 {
			return false
		}
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if !observed.Active(clock) {
			return false
		}
		prefix, err := netip.ParsePrefix(observed.Prefixes[0].Prefix)
		if err != nil {
			t.Fatal(err)
		}
		next := netip.PrefixFrom(prefix.Addr(), 64)
		if next == lanPrefix {
			return true
		}
		// The delegating peer routes its actual grant back to this router.
		if err := fixtureInsideNamespace(provider, func() error {
			link, err := netlink.LinkByName("server0")
			if err != nil {
				return err
			}
			_, destination, err := net.ParseCIDR(prefix.String())
			if err != nil {
				return err
			}
			return netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: destination, Gw: net.ParseIP("2001:db8:42::2")})
		}); err != nil {
			t.Fatal(err)
		}
		lanPrefix = next
		return true
	}
	until(t, 30*time.Second, readPD)
	poolAddress := func(ip netip.Addr) bool {
		if !lanPrefix.Contains(ip) {
			return false
		}
		raw := ip.As16()
		suffix := binary.BigEndian.Uint64(raw[8:])
		return suffix >= 0x100 && suffix <= 0x1ff
	}
	lease := filepath.Join(work, "client.lease")
	dns4 := filepath.Join(work, "dns4")
	hook := filepath.Join(work, "client.sh")
	body := fmt.Sprintf("#!/bin/sh\ncase \"$reason\" in\nBOUND6|RENEW6|REBIND6|REBOOT6) printf '%%s|%%s|%%s\\n' \"$reason\" \"$new_dhcp6_name_servers\" \"$new_dhcp6_domain_search\" > '%s';;\nBOUND|RENEW|REBIND|REBOOT) printf '%%s\\n' \"$new_domain_name_servers\" > '%s';; esac\n", lease, dns4)
	if err := os.WriteFile(hook, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	clientConfig := filepath.Join(work, "dhcpcd.conf")
	configureName := func(name string) {
		t.Helper()
		if err := os.WriteFile(clientConfig, []byte("duid\noption domain_name_servers, domain_name, domain_search\nhostname "+name+".lan.test\nfqdn both\ninterface client0\n iaid 1\n ia_na 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	configureName("v6client")
	log, err := os.Create(filepath.Join(work, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	defer func() {
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(work, "client.log"))
			if len(data) > 6000 {
				data = data[len(data)-6000:]
			}
			t.Logf("independent dhcpcd: %s", data)
		}
	}()
	child := exec.Command("unshare", "--mount", "--propagation", "private", "python3", "-c", `import os,subprocess,sys
subprocess.run(['mount','-t','tmpfs','-o','mode=755','tmpfs','/run'],check=True)
subprocess.run(['mount','-t','tmpfs','-o','mode=755','tmpfs','/var/lib/dhcpcd'],check=True)
os.execvp('dhcpcd',sys.argv[1:])`, "dhcpcd", "-B", "-d", "-f", clientConfig, "-c", hook, "client0")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Stdout, child.Stderr = log, log
	if err := fixtureInsideNamespace(client, child.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-child.Process.Pid, unix.SIGTERM); _ = child.Wait() })
	until(t, 12*time.Second, func() bool { _, err := os.Stat(lease); return err == nil })
	var address, address4 netip.Addr
	until(t, 8*time.Second, func() bool {
		if child.ProcessState != nil {
			t.Fatal("independent dhcpcd exited")
		}
		err := fixtureInsideNamespace(client, func() error {
			link, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			all, err := netlink.AddrList(link, netlink.FAMILY_ALL)
			for _, value := range all {
				ip, err := netip.ParseAddr(value.IP.String())
				if err == nil && netip.MustParsePrefix("198.18.0.0/24").Contains(ip) {
					address4 = ip
				}
				if err == nil && poolAddress(ip) && value.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
					address = ip
				}
			}
			return err
		})
		return err == nil && address.IsValid() && address4.IsValid()
	})
	if !poolAddress(address) {
		t.Fatal("client outside configured DHCP range", address)
	}
	until(t, 5*time.Second, func() bool { _, err := os.Stat(dns4); return err == nil })
	learned4, err := os.ReadFile(dns4)
	if err != nil {
		t.Fatal(err)
	}
	learned6, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	options6 := strings.Split(strings.TrimSpace(string(learned6)), "|")
	if len(options6) != 3 || options6[2] != "lan.test" {
		t.Fatal("missing actual DHCPv6 DNS/search options", string(learned6))
	}
	server4, err := netip.ParseAddr(strings.TrimSpace(string(learned4)))
	if err != nil || server4 != netip.MustParseAddr("198.18.0.1") {
		t.Fatal("wrong actual DHCPv4 DNS", string(learned4))
	}
	server6, err := netip.ParseAddr(options6[1])
	if err != nil || !lanPrefix.Contains(server6) {
		t.Fatal("wrong actual DHCPv6 DNS", options6[1])
	}
	servers := []string{server4.String(), server6.String()}
	clientAction := func(action string) {
		t.Helper()
		cmd := exec.Command("nsenter", fmt.Sprintf("--mount=/proc/%d/ns/mnt", child.Process.Pid), "dhcpcd", action, "client0")
		if err := fixtureInsideNamespace(client, func() error {
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("dhcpcd action %s: %v %s", action, err, data)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	query := func(server, name, kind, transport string) (string, error) {
		t.Helper()
		cmd := exec.Command("dig", "@"+server, name, kind, "+short", "+time=2", "+tries=1", transport)
		var data []byte
		err := fixtureInsideNamespace(client, func() error { var err error; data, err = cmd.CombinedOutput(); return err })
		return strings.TrimSpace(string(data)), err
	}
	dnsName := "v6client"
	checkDNS := func() {
		t.Helper()
		var observed string
		defer func() {
			if t.Failed() {
				t.Log(observed)
				var serving runtimeplan.InstanceStatus
				if err := f.control.Do(t.Context(), "GET", "/v1/services/service.addresses", nil, &serving); err == nil {
					t.Log("DHCPv6 observation:", string(serving.Snapshot))
				}
				var input dnsconfig.Input
				if err := f.control.Do(t.Context(), "GET", "/v1/services/service.dns/dns-input", nil, &input); err == nil {
					data, _ := json.Marshal(input)
					t.Log("DNS input:", string(data))
				}
			}
		}()
		until(t, 8*time.Second, func() bool {
			for _, server := range servers {
				for _, transport := range []string{"+notcp", "+tcp"} {
					for _, record := range []struct {
						kind    string
						address netip.Addr
					}{{"A", address4}, {"AAAA", address}} {
						answer, err := query(server, dnsName+".lan.test", record.kind, transport)
						if err != nil {
							observed = fmt.Sprintf("DNS %s %s %s: %q %v", server, transport, record.kind, answer, err)
							return false
						}
						got, err := netip.ParseAddr(answer)
						if err != nil || got != record.address {
							observed = fmt.Sprintf("DNS %s %s %s: got %q, want %s", server, transport, record.kind, answer, record.address)
							return false
						}
						cmd := exec.Command("dig", "@"+server, "-x", record.address.String(), "+short", "+time=2", "+tries=1", transport)
						var result []byte
						if err := fixtureInsideNamespace(client, func() error { var err error; result, err = cmd.CombinedOutput(); return err }); err != nil {
							return false
						}
						if strings.TrimSpace(string(result)) != dnsName+".lan.test." {
							observed = fmt.Sprintf("PTR %s %s %s: %q", server, transport, record.address, result)
							return false
						}
					}
				}
			}
			return true
		})
	}
	checkDNS()
	configureName("v6renamed")
	beforeRename, err := os.Stat(lease)
	if err != nil {
		t.Fatal(err)
	}
	clientAction("-n")
	until(t, 12*time.Second, func() bool {
		current, err := os.Stat(lease)
		return err == nil && current.ModTime().After(beforeRename.ModTime())
	})
	clientAction("-N")
	dnsName = "v6renamed"
	checkDNS()
	for _, transport := range []string{"+notcp", "+tcp"} {
		old, err := query(server6.String(), "v6client.lan.test", "AAAA", transport)
		if err != nil || old != "" {
			t.Fatal("renamed DHCP client retained its old AAAA", old, err)
		}
	}
	traffic := func() {
		t.Helper()
		if err := fixtureInsideNamespace(client, func() error { fixtureDHCP6Traffic(t, address.String()); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	traffic()
	ctx := context.Background()
	identity := func() string {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.addresses", nil, &status); err != nil {
			t.Fatal(err)
		}
		var s dhcp6serverconfig.Snapshot
		if err := json.Unmarshal(status.Snapshot, &s); err != nil {
			t.Fatal(err)
		}
		return s.ServerDUID
	}
	original := identity()
	renew := func() {
		t.Helper()
		before, err := os.Stat(lease)
		if err != nil {
			t.Fatal(err)
		}
		clientAction("-N")
		until(t, 12*time.Second, func() bool {
			after, err := os.Stat(lease)
			return err == nil && after.ModTime().After(before.ModTime())
		})
		if err := fixtureInsideNamespace(client, func() error {
			link, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			all, err := netlink.AddrList(link, netlink.FAMILY_V6)
			found := false
			for _, value := range all {
				found = found || value.IP.String() == address.String()
			}
			if !found {
				t.Fatal("client lost its DHCPv6 address on renewal")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		checkDNS()
		traffic()
	}
	renew()
	var modules supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	var generation uint64
	for _, m := range modules.Modules {
		if m.Name == "service.addresses" {
			generation = m.Generation
			if err := unix.Kill(m.PID, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
	}
	until(t, 12*time.Second, func() bool {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.addresses", nil, &status); err != nil {
			return false
		}
		return status.Generation > generation && len(status.Snapshot) > 0
	})
	if identity() != original {
		t.Fatal("worker restart changed server DUID")
	}
	renew()
	f.stop()
	f = f.restart()
	if identity() != original {
		t.Fatal("daemon restart changed server DUID")
	}
	renew()

	// Change the real delegating peer, retaining its lease database. Product
	// configuration stays unchanged; natural PD/client renewal drives the LAN.
	oldAddress, oldPrefix := address, lanPrefix
	startISC("2001:db8:81::")
	until(t, 90*time.Second, func() bool { return readPD() && lanPrefix != oldPrefix })
	readRenumbered := func() bool {
		found := false
		err := fixtureInsideNamespace(client, func() error {
			link, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			all, err := netlink.AddrList(link, netlink.FAMILY_V6)
			for _, value := range all {
				ip, parseErr := netip.ParseAddr(value.IP.String())
				if parseErr == nil && poolAddress(ip) && value.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
					address = ip
					found = true
				}
			}
			return err
		})
		return err == nil && found && address != oldAddress
	}
	until(t, 135*time.Second, readRenumbered)
	options, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(options)), "|")
	if len(fields) != 3 {
		t.Fatal("missing DHCPv6 DNS after PD change", string(options))
	}
	server6, err = netip.ParseAddr(fields[1])
	if err != nil || !lanPrefix.Contains(server6) {
		t.Fatal("DNS did not follow the real PD", fields[1], err)
	}
	servers[1] = server6.String()
	checkDNS()
	traffic()
	if identity() != original {
		t.Fatal("prefix change changed server DUID")
	}

	// Suspend the independent client without Release. The real lease expires
	// on the server's ordinary clock; IPv4 has its own longer lease.
	if err := unix.Kill(-child.Process.Pid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-child.Process.Pid, unix.SIGCONT) })
	var observed runtimeplan.InstanceStatus
	if err := f.control.Do(ctx, "GET", "/v1/services/service.addresses", nil, &observed); err != nil {
		t.Fatal(err)
	}
	var allocated dhcp6serverconfig.Snapshot
	if err := json.Unmarshal(observed.Snapshot, &allocated); err != nil {
		t.Fatal(err)
	}
	var expires time.Time
	for _, lease := range allocated.Leases {
		if lease.Address == address {
			expires = lease.ExpiresAt
			break
		}
	}
	if expires.IsZero() {
		t.Fatal("no actual lease expiry for", address)
	}
	// Wait for the recorded expiry rather than flooding the LAN with DNS
	// queries throughout the client's lease lifetime.
	if remaining := time.Until(expires); remaining > 0 {
		time.Sleep(remaining)
	}
	until(t, 8*time.Second, func() bool {
		answer, err := query(servers[0], dnsName+".lan.test", "AAAA", "+notcp")
		return err == nil && answer == ""
	})
	for _, server := range servers {
		for _, transport := range []string{"+notcp", "+tcp"} {
			answer, err := query(server, dnsName+".lan.test", "A", transport)
			got, parseErr := netip.ParseAddr(answer)
			if err != nil || parseErr != nil || got != address4 {
				t.Fatal("IPv6 expiry removed the live IPv4 record", answer, err)
			}
			answer, err = query(server, dnsName+".lan.test", "AAAA", transport)
			if err != nil || answer != "" {
				t.Fatal("expired DHCPv6 lease remains in DNS", answer, err)
			}
		}
	}
	beforeResume, err := os.Stat(lease)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(-child.Process.Pid, unix.SIGCONT); err != nil {
		t.Fatal(err)
	}
	clientAction("-N")
	until(t, 15*time.Second, func() bool {
		current, err := os.Stat(lease)
		return err == nil && current.ModTime().After(beforeResume.ModTime()) && readRenumbered()
	})
	// The expired address was free to be reassigned; read the client's current
	// allocation instead of requiring the old one to survive expiry.
	checkDNS()
	traffic()

	// Losing the upstream grant withdraws the product's delegated kernel
	// address and DNS publication, even while clients cache their own lease.
	stopISC()
	until(t, 75*time.Second, func() bool {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(t.Context(), "GET", "/v1/services/service.wan-pd", nil, &status); err != nil {
			return false
		}
		var pd dhcp6config.Snapshot
		if err := json.Unmarshal(status.Snapshot, &pd); err != nil {
			return false
		}
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if pd.Active(clock) {
			return false
		}
		link, err := netlink.LinkByName("lan0")
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range addresses {
			ip, err := netip.ParseAddr(entry.IP.String())
			if err == nil && lanPrefix.Contains(ip) {
				return false
			}
		}
		return true
	})
	until(t, 8*time.Second, func() bool {
		answer, err := query(servers[0], dnsName+".lan.test", "AAAA", "+notcp")
		return err == nil && answer == ""
	})
	for _, transport := range []string{"+notcp", "+tcp"} {
		answer, err := query(servers[0], dnsName+".lan.test", "A", transport)
		got, parseErr := netip.ParseAddr(answer)
		if err != nil || parseErr != nil || got != address4 {
			t.Fatal("upstream PD expiry removed working IPv4 DNS", answer, err)
		}
	}
	startISC("2001:db8:81::")
	until(t, 30*time.Second, readPD)
	until(t, 135*time.Second, readRenumbered)
	checkDNS()
	traffic()
	if identity() != original {
		t.Fatal("upstream PD recovery changed LAN DUID")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	wanted, err := hex.DecodeString(original)
	if err != nil {
		t.Fatal(err)
	}
	replies, renewals := 0, 0
	for _, packet := range capture.packets {
		b := packet.data
		if len(b) < 66 || b[20] != 17 {
			continue
		}
		if binary.BigEndian.Uint16(b[54:]) == 546 && (b[62] == 5 || b[62] == 6) {
			renewals++
		}
		if binary.BigEndian.Uint16(b[54:]) != 547 || (b[62] != 2 && b[62] != 7) {
			continue
		}
		options, err := dhcp6FixtureOptions(b[66:])
		if err != nil {
			t.Fatal(err)
		}
		for _, option := range options {
			if option.code == 2 {
				if !bytes.Equal(option.value, wanted) {
					t.Fatal("wire server DUID differs from persisted identity")
				}
				replies++
			}
		}
	}
	if replies < 4 || renewals < 3 {
		t.Fatal("missing actual server replies/client renewals", replies, renewals)
	}
	t.Log("Independent dhcpcd acquired RA/DHCPv6 and used real AAAA/PTR, hostname replacement, TCP/UDP, DUID/lease recovery", address)
}
