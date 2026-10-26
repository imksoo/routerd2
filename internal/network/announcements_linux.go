//go:build linux

package network

import (
	"encoding/binary"

	"github.com/imksoo/routerd2/internal/boottime"
	"golang.org/x/sys/unix"
)

// A reverse-ARP request advertises the Ethernet location without asserting an
// IPv4/IPv6 neighbor or default route. Reasserting a current finite MAC repairs
// switch learning after the departing owner's final RA or other queued packet.
func (*linuxKernel) AnnounceLink(a linkAnnouncement) error {
	var frame [60]byte
	for i := range 6 {
		frame[i] = 255
	}
	copy(frame[6:12], a.MAC)
	binary.BigEndian.PutUint16(frame[12:], unix.ETH_P_RARP)
	binary.BigEndian.PutUint16(frame[14:], 1) // Ethernet hardware
	binary.BigEndian.PutUint16(frame[16:], unix.ETH_P_IP)
	frame[18], frame[19] = 6, 4
	binary.BigEndian.PutUint16(frame[20:], 3) // RFC 903 request-reverse
	copy(frame[22:28], a.MAC)
	copy(frame[32:38], a.MAC)
	protocol := binary.NativeEndian.Uint16(frame[12:14])
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, int(protocol))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if !a.Authority.Live(clock) {
		return nil
	}
	destination := &unix.SockaddrLinklayer{Ifindex: a.Index, Protocol: protocol, Halen: 6}
	copy(destination.Addr[:], frame[:6])
	return unix.Sendto(fd, frame[:], 0, destination)
}
