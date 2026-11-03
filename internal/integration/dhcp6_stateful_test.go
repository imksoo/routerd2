//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"syscall"
)

func TestDaemonDHCP6StatefulIndependentClient(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6StatefulIndependentClient", 95*time.Second) {
		return
	}
	client := fixturePeerNamespace(t, "lan0", "client0")
	capture := healthPBRCapture(t, "lan0")
	resolver := dnsconfig.RuntimeConfig{
		Spec: api.DNSResolverSpec{
			Listen:  []api.DNSResolverListenSpec{{Name: "lan", Addresses: []string{"198.18.0.1", "2001:db8:80::1"}, Port: 53}},
			Sources: []api.DNSResolverSourceSpec{{Name: "local", Kind: "zone", Match: []string{"lan.test", "ip6.arpa", "0.18.198.in-addr.arpa"}, ZoneRef: []string{"lan"}}},
		},
		Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "lan.test", DHCPDerived: api.DNSZoneDHCPDerivedSpec{Sources: []string{"DHCPv4Server/addresses4", "DHCPv6Server/addresses"}}}}},
	}
	document := config.FromNative(config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"2001:db8:80::1/64", "198.18.0.1/24"}}, {Name: "lo", Addresses: []string{"2001:db8:ffff::9/128"}}}},
		Services: []serviceconfig.Service{
			{Name: "addresses4", DHCPServer: &serviceconfig.DHCPServer{Interface: "lan0", ServerAddress: "198.18.0.1", Authoritative: true, Primary: dhcpserverconfig.Scope{Prefix: "198.18.0.0/24", Start: "198.18.0.100", End: "198.18.0.199", LeaseSeconds: 120, Router: "198.18.0.1", DNSServers: []string{"198.18.0.1"}, Domain: "lan.test"}}},
			{Name: "addresses", DHCP6Server: &serviceconfig.DHCP6Server{Interface: "lan0", LogDHCP: true, Pool: &dhcp6serverconfig.Pool{Start: netip.MustParseAddr("::100"), End: netip.MustParseAddr("::1ff"), LeaseSeconds: 120}, DNSServers: []netip.Addr{netip.MustParseAddr("2001:db8:80::1")}, DomainSearch: []string{"lan.test"}}},
			{Name: "lan-ra", RA: &serviceconfig.RouterAdvertisement{Config: ra.Config{Interface: "lan0", Managed: true, OtherConfig: true, RouterLifetimeSeconds: 1800, DNSSLifetimeSeconds: 1800, Prefixes: []ra.Prefix{{Prefix: "2001:db8:80::/64", OnLink: true, Autonomous: false, ValidLifetimeSeconds: 7200, PreferredLifetimeSeconds: 3600}}}}},
			{Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: resolver}},
		},
	})
	f := startFixtureDaemon(t, document)
	self, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	vrrpVIPResponders(t, self, "6", []string{"2001:db8:ffff::9"}, "A")
	work := t.TempDir()
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
				if err == nil && netip.MustParsePrefix("2001:db8:80::/64").Contains(ip) && value.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
					address = ip
				}
			}
			return err
		})
		return err == nil && address.IsValid() && address4.IsValid()
	})
	if address.Compare(netip.MustParseAddr("2001:db8:80::100")) < 0 || address.Compare(netip.MustParseAddr("2001:db8:80::1ff")) > 0 {
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
	if err != nil || server6 != netip.MustParseAddr("2001:db8:80::1") {
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
		until(t, 8*time.Second, func() bool {
			for _, server := range servers {
				for _, transport := range []string{"+notcp", "+tcp"} {
					for _, record := range []struct {
						kind    string
						address netip.Addr
					}{{"A", address4}, {"AAAA", address}} {
						answer, err := query(server, dnsName+".lan.test", record.kind, transport)
						if err != nil {
							return false
						}
						got, err := netip.ParseAddr(answer)
						if err != nil || got != record.address {
							return false
						}
						cmd := exec.Command("dig", "@"+server, "-x", record.address.String(), "+short", "+time=2", "+tries=1", transport)
						var result []byte
						if err := fixtureInsideNamespace(client, func() error { var err error; result, err = cmd.CombinedOutput(); return err }); err != nil {
							return false
						}
						if strings.TrimSpace(string(result)) != dnsName+".lan.test." {
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
