package bgp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
)

// An independent minimal wire peer tests interoperability with real TCP
// sessions. It deliberately does not use GoBGP's parser or packet encoder.
// Both endpoints use loopback high ports; no host route/address is modified.
type wirePeer struct {
	conn net.Conn
	mu   sync.Mutex
}

func (p *wirePeer) send(typ byte, body []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := bytes.Repeat([]byte{255}, 19)
	binary.BigEndian.PutUint16(b[16:18], uint16(19+len(body)))
	b[18] = typ
	b = append(b, body...)
	_ = p.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err := p.conn.Write(b)
	return err
}

func readWireMessage(c net.Conn) (byte, []byte, error) {
	h := make([]byte, 19)
	if _, err := io.ReadFull(c, h); err != nil {
		return 0, nil, err
	}
	if !bytes.Equal(h[:16], bytes.Repeat([]byte{255}, 16)) {
		return 0, nil, fmt.Errorf("bad BGP marker")
	}
	n := int(binary.BigEndian.Uint16(h[16:18]))
	if n < 19 || n > 4096 {
		return 0, nil, fmt.Errorf("bad BGP frame length %d", n)
	}
	b := make([]byte, n-19)
	_, err := io.ReadFull(c, b)
	return h[18], b, err
}

func acceptWirePeer(l net.Listener) (*wirePeer, error) {
	c, err := l.Accept()
	if err != nil {
		return nil, err
	}
	p := &wirePeer{conn: c}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, _, err := readWireMessage(c)
	if err != nil || typ != 1 {
		_ = c.Close()
		return nil, fmt.Errorf("expected BGP OPEN, got type %d: %v", typ, err)
	}
	// OPEN: version 4, ASN 64513, hold 30, ID 192.0.2.2. Capabilities
	// negotiate IPv4/IPv6 unicast and the four-octet ASN encoding.
	caps := []byte{1, 4, 0, 1, 0, 1, 1, 4, 0, 2, 0, 1, 65, 4, 0, 0, 252, 1}
	open := []byte{4, 252, 1, 0, 30, 192, 0, 2, 2, byte(2 + len(caps)), 2, byte(len(caps))}
	open = append(open, caps...)
	if err := p.send(1, open); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := p.send(4, nil); err != nil {
		_ = c.Close()
		return nil, err
	}
	for {
		typ, body, err := readWireMessage(c)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		if typ == 3 {
			_ = c.Close()
			return nil, fmt.Errorf("BGP notification during handshake: %x", body)
		}
		if typ == 4 {
			break
		}
	}
	_ = c.SetReadDeadline(time.Time{})
	return p, nil
}

func wireNLRI(prefix string) []byte {
	p := netip.MustParsePrefix(prefix).Masked()
	return append([]byte{byte(p.Bits())}, p.Addr().AsSlice()[:(p.Bits()+7)/8]...)
}

func wireUpdate(prefix, nextHop string, withdraw bool) []byte {
	p := netip.MustParsePrefix(prefix)
	nlri := wireNLRI(prefix)
	attrs := []byte{}
	if !withdraw {
		// ORIGIN IGP; AS_PATH AS_SEQUENCE containing four-octet 64513.
		attrs = []byte{0x40, 1, 1, 0, 0x40, 2, 6, 2, 1, 0, 0, 252, 1}
	}
	if p.Addr().Is4() {
		if withdraw {
			b := []byte{0, byte(len(nlri))}
			b = append(b, nlri...)
			return append(b, 0, 0)
		}
		attrs = append(attrs, 0x40, 3, 4)
		attrs = append(attrs, netip.MustParseAddr(nextHop).AsSlice()...)
	} else {
		mp := []byte{0, 2, 1}
		kind := byte(15) // MP_UNREACH_NLRI
		if !withdraw {
			kind = 14 // MP_REACH_NLRI: 16-byte global IPv6 next hop.
			mp = append(mp, 16)
			mp = append(mp, netip.MustParseAddr(nextHop).AsSlice()...)
			mp = append(mp, 0)
		}
		mp = append(mp, nlri...)
		attrs = append(attrs, 0x80, kind, byte(len(mp)))
		attrs = append(attrs, mp...)
	}
	out := []byte{0, 0, byte(len(attrs) >> 8), byte(len(attrs))}
	out = append(out, attrs...)
	if p.Addr().Is4() {
		out = append(out, nlri...)
	}
	return out
}

type snapshotCapture struct {
	mu      sync.Mutex
	latest  Snapshot
	changed chan struct{}
}

func (c *snapshotCapture) Update(_ context.Context, s Snapshot) error {
	c.mu.Lock()
	c.latest = cloneSnapshot(s)
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return nil
}
func (c *snapshotCapture) get() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneSnapshot(c.latest)
}

func waitSnapshot(t *testing.T, capture *snapshotCapture, want func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		s := capture.get()
		if want(s) {
			return s
		}
		select {
		case <-capture.changed:
		case <-timer.C:
			t.Fatalf("BGP snapshot condition timed out: %#v", s)
		}
	}
}

func TestNativeTCPBGPIPv4IPv6PolicyAndWithdrawal(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	peerReady := make(chan *wirePeer, 1)
	peerErr := make(chan error, 1)
	go func() {
		p, err := acceptWirePeer(l)
		if err != nil {
			peerErr <- err
			return
		}
		peerReady <- p
	}()
	cfg := moduleConfig(t)
	cfg.Runtime.Peers = map[string]bgpdaemon.AppliedPeer{"127.0.0.1": {Address: "127.0.0.1", ASN: 64513, TimersProfile: "fast", ImportPolicy: bgpdaemon.AppliedImportPolicy{AllowedPrefixes: []string{"203.0.113.0/24", "2001:db8:77::/64"}, NextHopRewrite: "unchanged"}}}
	configured := cfg.Runtime.Peers["127.0.0.1"]
	configured.ExportPolicyName = "dual-stack-export"
	configured.ExportPolicy.AllowedPrefixes = []string{"10.20.0.0/24", "2001:db8:20::/64"}
	cfg.Runtime.Peers["127.0.0.1"] = configured
	cfg.FIBInterface = "wan0"
	cfg.PeerTransports = map[string]PeerTransport{"127.0.0.1": {LocalAddress: "127.0.0.1", RemotePort: l.Addr().(*net.TCPAddr).Port, FIBInterface: "peer0"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &snapshotCapture{changed: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, capture, nil) }()
	// Ensure all protocol goroutines have stopped even when an assertion fails.
	finished := false
	defer func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Error("native BGP did not stop")
			}
		}
	}()
	var peer *wirePeer
	select {
	case peer = <-peerReady:
	case err := <-peerErr:
		t.Fatal(err)
	case err := <-done:
		finished = true
		t.Fatal(err)
	case <-time.After(6 * time.Second):
		t.Fatal("wire peer did not connect")
	}
	defer peer.conn.Close()
	go func() {
		for {
			typ, b, err := readWireMessage(peer.conn)
			if err != nil {
				return
			}
			if typ == 3 {
				select {
				case peerErr <- fmt.Errorf("BGP notification: %x", b):
				default:
				}
				return
			}
		}
	}()
	waitSnapshot(t, capture, func(s Snapshot) bool { return len(s.Peers) == 1 && s.Peers[0].State == "SESSION_STATE_ESTABLISHED" })
	for _, path := range []struct{ prefix, hop string }{{"203.0.113.0/24", "127.0.0.1"}, {"2001:db8:77::/64", "2001:db8::2"}, {"198.51.100.0/24", "127.0.0.1"}} {
		if err := peer.send(2, wireUpdate(path.prefix, path.hop, false)); err != nil {
			t.Fatal(err)
		}
	}
	s := waitSnapshot(t, capture, func(s Snapshot) bool { return len(s.Routes) == 2 })
	for _, route := range s.Routes {
		if route.Prefix == "198.51.100.0/24" || route.Interface != "peer0" || route.Table != 254 || route.Metric != 200 || route.PeerAddress != "127.0.0.1" {
			t.Fatalf("unexpected FIB route %#v", route)
		}
	}
	if err := peer.send(2, wireUpdate("203.0.113.0/24", "", true)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, capture, func(s Snapshot) bool { return len(s.Routes) == 1 && s.Routes[0].Family == "ipv6-unicast" })
	if err := peer.send(2, wireUpdate("2001:db8:77::/64", "", true)); err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, capture, func(s Snapshot) bool { return s.Active && len(s.Routes) == 0 })
	select {
	case err := <-peerErr:
		t.Fatal(err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		finished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("shutdown exceeded bound")
	}
	if capture.get().Active {
		t.Fatal("shutdown did not withdraw FIB ownership")
	}
}
