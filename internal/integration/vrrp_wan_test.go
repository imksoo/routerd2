//go:build linux && integration

package integration

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	vrrpWANVIP = "198.51.100.254"
	vrrpWANLL  = "fe80::42:1"
	vrrpWANMAC = "02:00:5e:00:42:01"
)

type vrrpWANTest struct {
	client  netns.NsHandle
	peer    *netlink.Handle
	capture *healthPBRCaptureState
}

func setupVRRPWAN(t *testing.T, peerNS netns.NsHandle) *vrrpWANTest {
	t.Helper()
	bridge := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "wan-br"}}
	if err := netlink.LinkAdd(bridge); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(bridge); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"hub-rwan", "wan0"}, {"hub-pwan", "peerwan0"}} {
		if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: pair[0]}, PeerName: pair[1]}); err != nil {
			t.Fatal(err)
		}
		hub, err := netlink.LinkByName(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetMaster(hub, bridge); err != nil {
			t.Fatal(err)
		}
		if err := netlink.LinkSetUp(hub); err != nil {
			t.Fatal(err)
		}
		endpoint, err := netlink.LinkByName(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if pair[1] == "peerwan0" {
			if err := netlink.LinkSetNsFd(endpoint, int(peerNS)); err != nil {
				t.Fatal(err)
			}
		} else if err := netlink.LinkSetUp(endpoint); err != nil {
			t.Fatal(err)
		}
	}
	client := fixturePeerNamespace(t, "hub-cwan", "client0")
	hub, err := netlink.LinkByName("hub-cwan")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetMaster(hub, bridge); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(hub); err != nil {
		t.Fatal(err)
	}
	clientHandle, err := netlink.NewHandleAt(client, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer clientHandle.Close()
	clientLink, err := clientHandle.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("198.51.100.10/24")
	if err := clientHandle.AddrAdd(clientLink, address); err != nil {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(peerNS, func() error {
		parent, err := netlink.LinkByName("peerwan0")
		if err != nil {
			return err
		}
		if err := netlink.LinkSetUp(parent); err != nil {
			return err
		}
		mac, err := net.ParseMAC(vrrpWANMAC)
		if err != nil {
			return err
		}
		link := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "peer-ha", ParentIndex: parent.Attrs().Index, HardwareAddr: mac}, Mode: netlink.MACVLAN_MODE_BRIDGE}
		if err := netlink.LinkAdd(link); err != nil {
			return err
		}
		if err := netlink.LinkSetIP6AddrGenMode(link, 1 /* IN6_ADDR_GEN_MODE_NONE */); err != nil {
			return err
		}
		return netlink.LinkSetUp(link)
	}); err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Close)
	return &vrrpWANTest{client: client, peer: peer, capture: healthPBRCapture(t, "wan0")}
}

func (w *vrrpWANTest) peerOwns(t *testing.T) bool {
	t.Helper()
	link, err := w.peer.LinkByName("peer-ha")
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := w.peer.AddrList(link, unix.AF_UNSPEC)
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if address.IP.String() == vrrpWANVIP {
			return true
		}
	}
	return false
}

func (w *vrrpWANTest) waitNative(t *testing.T, owns bool) {
	t.Helper()
	until(t, 8*time.Second, func() bool {
		link, err := netlink.LinkByName("wan-ha")
		if !owns {
			return err != nil && w.peerOwns(t)
		}
		if err != nil || link.Attrs().Flags&net.FlagUp == 0 || link.Attrs().HardwareAddr.String() != vrrpWANMAC || w.peerOwns(t) {
			return false
		}
		addresses, err := netlink.AddrList(link, unix.AF_UNSPEC)
		if err != nil {
			t.Fatal(err)
		}
		ipv4, ipv6 := false, false
		for _, address := range addresses {
			if address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) != 0 {
				return false
			}
			ipv4 = ipv4 || address.IP.String() == vrrpWANVIP
			ipv6 = ipv6 || address.IP.String() == vrrpWANLL
		}
		return ipv4 && ipv6
	})
}

func (w *vrrpWANTest) advertisements() (positive, zero int) {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	mac, _ := net.ParseMAC(vrrpWANMAC)
	for _, packet := range w.capture.packets {
		b := packet.data
		if len(b) < 70 || binary.BigEndian.Uint16(b[12:14]) != 0x86dd || b[20] != 58 || b[54] != 134 || !bytes.Equal(b[6:12], mac) {
			continue
		}
		if binary.BigEndian.Uint16(b[60:62]) == 0 {
			zero++
		} else {
			positive++
		}
	}
	return positive, zero
}

func (w *vrrpWANTest) logPath(t *testing.T) {
	t.Helper()
	fdb, _ := netlink.NeighList(0, unix.AF_BRIDGE)
	for _, entry := range fdb {
		if entry.HardwareAddr.String() == vrrpWANMAC {
			link, _ := netlink.LinkByIndex(entry.LinkIndex)
			t.Log("WAN FDB", entry, "port", link.Attrs().Name)
		}
	}
	client, err := netlink.NewHandleAt(w.client, unix.NETLINK_ROUTE)
	if err != nil {
		t.Log(err)
		return
	}
	defer client.Close()
	neighbors, _ := client.NeighList(0, unix.AF_INET)
	t.Log("WAN client neighbors", neighbors)
	link, _ := w.peer.LinkByName("peer-ha")
	addresses, _ := w.peer.AddrList(link, unix.AF_UNSPEC)
	t.Log("WAN peer", link.Attrs(), addresses)
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	for _, packet := range w.capture.packets {
		b := packet.data
		if len(b) >= 42 && binary.BigEndian.Uint16(b[12:14]) == 0x0806 {
			t.Logf("WAN ARP %s srcmac=%x sender=%v target=%v", packet.at.Format(time.RFC3339Nano), b[6:12], net.IP(b[28:32]), net.IP(b[38:42]))
		}
		if len(b) >= 70 && binary.BigEndian.Uint16(b[12:14]) == 0x86dd && b[20] == 58 && b[54] == 134 {
			t.Logf("WAN RA %s lifetime=%d srcmac=%x", packet.at.Format(time.RFC3339Nano), binary.BigEndian.Uint16(b[60:62]), b[6:12])
		}
	}
}

func (w *vrrpWANTest) waitForwarder(t *testing.T, native bool) {
	t.Helper()
	want := "hub-pwan"
	if native {
		want = "hub-rwan"
	}
	until(t, 2*time.Second, func() bool {
		entries, err := netlink.NeighList(0, unix.AF_BRIDGE)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.HardwareAddr.String() != vrrpWANMAC || entry.State == unix.NUD_PERMANENT {
				continue
			}
			link, err := netlink.LinkByIndex(entry.LinkIndex)
			if err == nil && link.Attrs().Name == want {
				return true
			}
		}
		return false
	})
}

func (w *vrrpWANTest) announcements(t *testing.T) int {
	t.Helper()
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	mac, _ := net.ParseMAC(vrrpWANMAC)
	count := 0
	for _, packet := range w.capture.packets {
		b := packet.data
		if len(b) < 60 || binary.BigEndian.Uint16(b[12:14]) != 0x8035 {
			continue
		}
		if !bytes.Equal(b[:6], bytes.Repeat([]byte{255}, 6)) || !bytes.Equal(b[6:12], mac) || !bytes.Equal(b[22:28], mac) || !bytes.Equal(b[32:38], mac) || binary.BigEndian.Uint16(b[14:16]) != 1 || binary.BigEndian.Uint16(b[16:18]) != 0x0800 || b[18] != 6 || b[19] != 4 || binary.BigEndian.Uint16(b[20:22]) != 3 || !bytes.Equal(b[28:32], make([]byte, 4)) || !bytes.Equal(b[38:42], make([]byte, 4)) {
			t.Fatal("invalid WAN MAC announcement")
		}
		count++
	}
	return count
}
