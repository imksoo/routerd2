// SPDX-License-Identifier: BSD-3-Clause

// Package ethernet constructs Ethernet frames for protocols that must reach
// IPv4 clients before they can answer ARP. It owns no sockets or kernel state.
package ethernet

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
)

// UDP4 constructs one unfragmented Ethernet/IPv4/UDP frame with both checksums.
func UDP4(sourceMAC, destinationMAC net.HardwareAddr, source, destination netip.Addr, sourcePort, destinationPort, id uint16, payload []byte) ([]byte, error) {
	if len(sourceMAC) != 6 || len(destinationMAC) != 6 || !source.Is4() || !destination.Is4() || len(payload) > 65507 {
		return nil, errors.New("invalid Ethernet IPv4/UDP frame")
	}
	b := make([]byte, 42+len(payload))
	copy(b[:6], destinationMAC)
	copy(b[6:12], sourceMAC)
	binary.BigEndian.PutUint16(b[12:14], 0x0800)
	ip := b[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(28+len(payload)))
	binary.BigEndian.PutUint16(ip[4:6], id)
	ip[8], ip[9] = 64, 17
	sourceBytes := source.As4()
	copy(ip[12:16], sourceBytes[:])
	destinationBytes := destination.As4()
	copy(ip[16:20], destinationBytes[:])
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
	udp := b[34:]
	binary.BigEndian.PutUint16(udp[:2], sourcePort)
	binary.BigEndian.PutUint16(udp[2:4], destinationPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	pseudo := make([]byte, 12+len(udp))
	copy(pseudo[:4], sourceBytes[:])
	copy(pseudo[4:8], destinationBytes[:])
	pseudo[9] = 17
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(udp)))
	copy(pseudo[12:], udp)
	sum := checksum(pseudo)
	if sum == 0 {
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(udp[6:8], sum)
	return b, nil
}
func checksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	if len(b) > 0 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
