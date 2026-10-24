//go:build linux

package health

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"syscall"

	old "github.com/imksoo/routerd/pkg/healthcheck"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

func Probe(ctx context.Context, raw Config) old.ProbeResult {
	c := Normalize(raw)
	if err := Validate(c); err != nil {
		return old.ProbeResult{Message: err.Error()}
	}
	// TCP/DNS/HTTP retain the pinned implementation, including HTTP 2xx..4xx
	// reachability and DNS example.com lookup. Literal targets avoid unscoped
	// bootstrap DNS. Do not call EnrichEvidence: it invokes external ip(8).
	if c.Protocol != "icmp" {
		return old.Probe(ctx, c.spec())
	}
	result := old.ProbeResult{ProbeEvidence: old.ProbeEvidence{EgressInterface: c.Interface, SourceAddress: c.SourceAddress}}
	err := probeICMP(ctx, c)
	result.OK = err == nil
	if err != nil {
		result.Message = err.Error()
		var timeout net.Error
		result.Timeout = errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout())
	} else {
		result.Message = "scoped icmp echo succeeded"
	}
	return result
}

func socketControl(c Config) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		var socketErr error
		if err := conn.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface)
			if socketErr == nil && c.Mark != 0 {
				socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, int(c.Mark))
			}
		}); err != nil {
			return err
		}
		return socketErr
	}
}
func probeICMP(ctx context.Context, c Config) error {
	target := netip.MustParseAddr(c.Target)
	network, source, protocol := "ip6:ipv6-icmp", "::", 58
	var request, reply icmp.Type = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	if target.Is4() {
		network, source, protocol = "ip4:icmp", "0.0.0.0", 1
		request, reply = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	}
	if c.SourceAddress != "" {
		source = c.SourceAddress
	}
	lc := net.ListenConfig{Control: socketControl(c)}
	conn, err := lc.ListenPacket(ctx, network, source)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	id := int(binary.BigEndian.Uint16(nonce[:2]))
	echo := &icmp.Echo{ID: id, Seq: 1, Data: nonce[2:]}
	packet, err := (&icmp.Message{Type: request, Code: 0, Body: echo}).Marshal(nil)
	if err != nil {
		return err
	}
	zone := ""
	if target.Is6() && target.IsLinkLocalUnicast() {
		zone = c.Interface
	}
	if _, err = conn.WriteTo(packet, &net.IPAddr{IP: net.IP(target.AsSlice()), Zone: zone}); err != nil {
		return err
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		ip, ok := peer.(*net.IPAddr)
		if !ok || !ip.IP.Equal(net.IP(target.AsSlice())) {
			continue
		}
		message, err := icmp.ParseMessage(protocol, buf[:n])
		if err != nil || message.Type != reply || message.Code != 0 {
			continue
		}
		received, ok := message.Body.(*icmp.Echo)
		if ok && received.ID == id && received.Seq == 1 && bytes.Equal(received.Data, echo.Data) {
			return nil
		}
	}
}
