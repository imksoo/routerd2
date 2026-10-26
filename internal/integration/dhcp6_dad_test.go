//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6DuplicateAddressDecline(t *testing.T) {
	if !isolateFixture(t, "TestDaemonDHCP6DuplicateAddressDecline") {
		return
	}
	provider := fixturePeerNamespace(t, "wire0", "server0")
	duplicate := fixturePeerNamespace(t, "dup0", "duplicate0")
	for _, endpoint := range []struct {
		ns      netns.NsHandle
		name    string
		address string
	}{{provider, "server0", "fe80::2/64"}, {provider, "server0", "2001:db8:42::1/64"}, {provider, "lo", "2001:db8:ffff::9/128"}, {duplicate, "duplicate0", "2001:db8:42::100/64"}} {
		if err := fixtureInsideNamespace(endpoint.ns, func() error {
			link, err := netlink.LinkByName(endpoint.name)
			if err != nil {
				return err
			}
			if err := netlink.LinkSetUp(link); err != nil {
				return err
			}
			a, err := netlink.ParseAddr(endpoint.address)
			if err != nil {
				return err
			}
			a.Flags = unix.IFA_F_NODAD
			return netlink.AddrAdd(link, a)
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := startFixtureDNSMasq(t, provider, []string{"--port=0", "--dhcp-range=2001:db8:42::100,2001:db8:42::100,64,120s"})
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	capture := healthPBRCapture(t, "wire0")
	bridge := "wan0"
	doc := config.Native{Network: network.Desired{
		Links:      []network.Link{{Name: bridge, Kind: "bridge"}},
		Interfaces: []network.Interface{{Name: "wire0", Master: &bridge}, {Name: "dup0", Master: &bridge}, {Name: bridge, Addresses: []string{"fe80::1/64"}, AddressOptions: map[string]network.AddressOptions{"fe80::1/64": {NoDAD: true}}}},
		Routes:     []network.Route{{Destination: "2001:db8:ffff::9/128", Gateway: "fe80::2", Interface: bridge}},
	}, Services: []serviceconfig.Service{{Name: "wan", DHCP6: &serviceconfig.DHCP6{Interface: bridge, IAID: 7, RequestAddress: true}}}}
	f := startFixtureDaemon(t, config.FromNative(doc))
	module := func() supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, m := range status.Modules {
			if m.Name == "service.wan" {
				return m
			}
		}
		t.Fatal("missing DHCPv6 worker")
		return supervisor.ModuleStatus{}
	}
	initial := module()
	// The duplicate is an independent Linux host. Observe both its DAD
	// response and the client's Decline carrying exactly that IAADDR.
	until(t, 12*time.Second, func() bool {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		var advertised, declined bool
		for _, p := range capture.packets {
			b := p.data
			if len(b) >= 78 && b[20] == unix.IPPROTO_ICMPV6 && b[54] == 136 && net.IP(b[62:78]).Equal(net.ParseIP("2001:db8:42::100")) {
				advertised = true
			}
			if len(b) < 66 || b[20] != unix.IPPROTO_UDP || binary.BigEndian.Uint16(b[54:56]) != 546 || binary.BigEndian.Uint16(b[56:58]) != 547 || b[62] != 9 {
				continue
			}
			opts, err := dhcp6FixtureOptions(b[66:])
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range opts {
				if o.code != 3 || len(o.value) < 12 || binary.BigEndian.Uint32(o.value) != 7 {
					continue
				}
				children, err := dhcp6FixtureOptions(o.value[12:])
				if err != nil {
					t.Fatal(err)
				}
				for _, child := range children {
					declined = declined || child.code == 5 && len(child.value) == 24 && net.IP(child.value[:16]).Equal(net.ParseIP("2001:db8:42::100"))
				}
			}
		}
		return advertised && declined
	})
	until(t, 3*time.Second, func() bool {
		link, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if address.IP.String() == "2001:db8:42::100" {
				return false
			}
		}
		return true
	})
	if err := server.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	until(t, time.Second, func() bool { return errors.Is(unix.Kill(server.Process.Pid, 0), unix.ESRCH) })
	startFixtureDNSMasq(t, provider, []string{"--port=0", "--dhcp-range=2001:db8:42::101,2001:db8:42::101,64,120s"})
	until(t, 12*time.Second, func() bool {
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/service.wan", nil, &status); err != nil {
			t.Fatal(err)
		}
		var s dhcp6config.Snapshot
		if err := json.Unmarshal(status.Snapshot, &s); err != nil {
			t.Fatal(err)
		}
		return len(s.Addresses) == 1 && s.Addresses[0].Address == "2001:db8:42::101" && s.State == "bound"
	})
	until(t, 3*time.Second, func() bool {
		link, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatal(err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if address.IP.String() == "2001:db8:42::101" && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				return true
			}
		}
		return false
	})
	fixtureDHCP6Traffic(t, "2001:db8:42::101")
	if final := module(); final.PID != initial.PID || final.Generation != initial.Generation {
		t.Fatal("DAD failure restarted the DHCPv6 worker", initial, final)
	}
	t.Log("independent Linux duplicate caused DAD failure and IA_NA Decline; failed address withdrawn; same worker acquired a usable dnsmasq lease and restored TCP/UDP")
}

type dhcp6FixtureOption struct {
	code  uint16
	value []byte
}

func dhcp6FixtureOptions(packet []byte) ([]dhcp6FixtureOption, error) {
	var out []dhcp6FixtureOption
	for len(packet) > 0 {
		if len(packet) < 4 {
			return nil, errors.New("short DHCPv6 option header")
		}
		code, n := binary.BigEndian.Uint16(packet), int(binary.BigEndian.Uint16(packet[2:]))
		packet = packet[4:]
		if n > len(packet) {
			return nil, errors.New("short DHCPv6 option body")
		}
		out = append(out, dhcp6FixtureOption{code: code, value: packet[:n]})
		packet = packet[n:]
	}
	return out, nil
}
