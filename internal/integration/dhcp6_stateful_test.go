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

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6StatefulIndependentClient(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6StatefulIndependentClient", 95*time.Second) {
		return
	}
	client := fixturePeerNamespace(t, "lan0", "client0")
	capture := healthPBRCapture(t, "lan0")
	document := config.FromNative(config.Native{
		Network:  network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"2001:db8:80::1/64"}}, {Name: "lo", Addresses: []string{"2001:db8:ffff::9/128"}}}},
		Services: []serviceconfig.Service{{Name: "addresses", DHCP6Server: &serviceconfig.DHCP6Server{Interface: "lan0", LogDHCP: true, Pool: &dhcp6serverconfig.Pool{Start: netip.MustParseAddr("::100"), End: netip.MustParseAddr("::1ff"), LeaseSeconds: 120}, DNSServers: []netip.Addr{netip.MustParseAddr("2001:db8:80::1")}, DomainSearch: []string{"lan.test"}}}},
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
	hook := filepath.Join(work, "client.sh")
	body := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in bound|renew) printf '%%s %%s\\n' \"$ipv6\" \"$dns\" > '%s';; esac\n", lease)
	if err := os.WriteFile(hook, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(work, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	child := exec.Command("busybox", "udhcpc6", "-f", "-n", "-i", "client0", "-t", "5", "-T", "1", "-O", "23", "-O", "24", "-s", hook)
	child.Stdout, child.Stderr = log, log
	if err := fixtureInsideNamespace(client, child.Start); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	until(t, 12*time.Second, func() bool { _, err := os.Stat(lease); return err == nil })
	data, err := os.ReadFile(lease)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		b, _ := os.ReadFile(filepath.Join(work, "client.log"))
		t.Fatal("missing client address/DNS", string(data), string(b))
	}
	address, err := netip.ParseAddr(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	if !netip.MustParsePrefix("2001:db8:80::/64").Contains(address) || address.Compare(netip.MustParseAddr("2001:db8:80::100")) < 0 || address.Compare(netip.MustParseAddr("2001:db8:80::1ff")) > 0 {
		t.Fatal("client outside configured range", address)
	}
	if netip.MustParseAddr(fields[1]) != netip.MustParseAddr("2001:db8:80::1") {
		t.Fatal("wrong DHCP DNS", fields)
	}
	if err := fixtureInsideNamespace(client, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		a, _ := netlink.ParseAddr(address.String() + "/64")
		if err := netlink.AddrReplace(link, a); err != nil {
			return err
		}
		return exec.Command("ip", "-6", "route", "replace", "2001:db8:ffff::9/128", "via", "2001:db8:80::1", "dev", "client0").Run()
	}); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		usable := false
		_ = fixtureInsideNamespace(client, func() error {
			l, _ := netlink.LinkByName("client0")
			all, err := netlink.AddrList(l, netlink.FAMILY_V6)
			for _, a := range all {
				if a.IP.String() == address.String() && a.Flags&unix.IFA_F_TENTATIVE == 0 {
					usable = true
				}
			}
			return err
		})
		return usable
	})
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
		if err := child.Process.Signal(unix.SIGUSR1); err != nil {
			t.Fatal(err)
		}
		until(t, 12*time.Second, func() bool {
			after, err := os.Stat(lease)
			return err == nil && after.ModTime().After(before.ModTime())
		})
		after, _ := os.ReadFile(lease)
		renewed, parseErr := netip.ParseAddr(strings.Fields(string(after))[0])
		if parseErr != nil || renewed != address {
			t.Fatal("renew changed the address", string(after))
		}
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
	t.Log("Independent BusyBox acquired and renewed an actual address; configured DNS, TCP/UDP, persisted DUID/lease after worker and daemon restart", address)
}
