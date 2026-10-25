//go:build linux && integration

package integration

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Captures opened before daemon startup must see the later interface-up traffic.
func TestPacketCaptureSurvivesLinkActivation(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestPacketCaptureSurvivesLinkActivation", 10*time.Second) {
		return
	}
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "capture0"}, PeerName: "send0"}); err != nil {
		t.Fatal(err)
	}
	local, err := netlink.LinkByName("capture0")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := netlink.LinkByName("send0")
	if err != nil {
		t.Fatal(err)
	}
	capture := healthPBRCapture(t, "capture0")
	if err := netlink.LinkSetUp(local); err != nil {
		t.Fatal(err)
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	frame := make([]byte, 60)
	copy(frame[:6], local.Attrs().HardwareAddr)
	copy(frame[6:12], peer.Attrs().HardwareAddr)
	binary.BigEndian.PutUint16(frame[12:14], 0x88b5)
	if err := unix.Sendto(fd, frame, 0, &unix.SockaddrLinklayer{Ifindex: peer.Attrs().Index, Protocol: htons(0x88b5), Halen: 6}); err != nil {
		t.Fatal(err)
	}
	until(t, time.Second, func() bool { capture.mu.Lock(); defer capture.mu.Unlock(); return len(capture.packets) > 0 })
}
