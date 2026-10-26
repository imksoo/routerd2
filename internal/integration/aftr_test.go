//go:build linux && integration

// SPDX-License-Identifier: BSD-3-Clause

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The independent DNS peer lives in a second isolated namespace. The only
// router-side mutations after fixture creation come from the actual network
// worker in the native process tree; no external ip/nft/route helpers are used.
func TestDaemonAFTRResolutionAndExpiry(t *testing.T) {
	if !isolateFixture(t, "TestDaemonAFTRResolutionAndExpiry") {
		return
	}
	peerNS := fixturePeerNamespace(t, "wan0", "provider0")
	h, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	peer, err := h.LinkByName("provider0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("2001:db8:42::53/64")
	address.Flags = unix.IFA_F_NODAD
	if err := h.AddrReplace(peer, address); err != nil {
		t.Fatal(err)
	}
	for _, cidr := range []string{"2001:db8:42::2/64", "2001:db8:42::3/64", "2001:db8:42::4/64"} {
		address, _ := netlink.ParseAddr(cidr)
		address.Flags = unix.IFA_F_NODAD
		if err := h.AddrAdd(peer, address); err != nil {
			t.Fatal(err)
		}
	}
	loopback, err := h.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	backend, _ := netlink.ParseAddr("198.18.0.1/32")
	if err := h.AddrAdd(loopback, backend); err != nil {
		t.Fatal(err)
	}
	// The independent peer uses Linux's own IPv4-in-IPv6 dataplane.
	peerTunnel := &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: "aftr0"}, Link: uint32(peer.Attrs().Index), Local: net.ParseIP("2001:db8:42::2"), Remote: net.ParseIP("2001:db8:42::1"), Proto: unix.IPPROTO_IPIP, Ttl: 64, Flags: 1}
	if err := h.LinkAdd(peerTunnel); err != nil {
		t.Fatal(err)
	}
	b4Peer, _ := netlink.ParseAddr("192.0.0.1/32")
	b4Peer.Peer = &net.IPNet{IP: net.ParseIP("192.0.0.2"), Mask: net.CIDRMask(32, 32)}
	if err := h.AddrAdd(peerTunnel, b4Peer); err != nil {
		t.Fatal(err)
	}
	if err := h.LinkSetUp(peerTunnel); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, peerNS, "4", []string{"198.18.0.1"}, "S")
	capture := healthPBRCapture(t, "wan0")
	traffic := func(protocol string) error {
		port := "18093"
		dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP("192.0.0.2")}}
		if protocol == "udp4" {
			port, dialer.LocalAddr = "18094", &net.UDPAddr{IP: net.ParseIP("192.0.0.2")}
		}
		conn, err := dialer.Dial(protocol, net.JoinHostPort("198.18.0.1", port))
		if err != nil {
			return err
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		if protocol == "udp4" {
			if _, err := conn.Write([]byte("dslite")); err != nil {
				return err
			}
		}
		var answer [1]byte
		if _, err := io.ReadFull(conn, answer[:]); err != nil {
			return err
		}
		if answer[0] != 'S' {
			return fmt.Errorf("unexpected AFTR backend response %q", answer)
		}
		return nil
	}
	communicate := func() {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			if err := traffic(protocol); err != nil {
				t.Fatal("DS-Lite backend traffic", protocol, err)
			}
		}
	}
	movePeer := func(remote string) {
		t.Helper()
		peerTunnel.Local = net.ParseIP(remote)
		if err := h.LinkModify(peerTunnel); err != nil {
			t.Fatal(err)
		}
	}
	var socket net.PacketConn
	err = fixtureInsideNamespace(peerNS, func() error {
		lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "provider0")
			})
			return errors.Join(err, inner)
		}}
		var err error
		socket, err = lc.ListenPacket(context.Background(), "udp6", "[2001:db8:42::53]:0")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	const ttl = 8
	var dnsMu sync.Mutex
	remote, silent, queries, wrongQueries := "2001:db8:42::2", false, 0, 0
	server := &dns.Server{Net: "udp", PacketConn: socket, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		dnsMu.Lock()
		defer dnsMu.Unlock()
		queries++
		if len(q.Question) != 1 || q.Question[0] != (dns.Question{Name: "aftr.test.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}) {
			wrongQueries++
			return
		}
		if silent {
			return
		}
		m := new(dns.Msg).SetReply(q)
		m.Authoritative = true
		m.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: "aftr.test.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}, AAAA: net.ParseIP(remote)}}
		_ = w.WriteMsg(m)
	})}
	ready, done := make(chan struct{}), make(chan error, 1)
	server.NotifyStartedFunc = func() { close(ready) }
	go func() { done <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		select {
		case err := <-done:
			if err != nil {
				t.Error("DNS fixture shutdown:", err)
			}
		case <-time.After(time.Second):
			t.Error("DNS fixture failed to stop")
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("DNS fixture startup timeout")
	}

	doc := config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8:42::1/64"}, AddressOptions: map[string]network.AddressOptions{"2001:db8:42::1/64": {NoDAD: true}}}}},
		Services: []serviceconfig.Service{{Name: "dslite", DSLite: &serviceconfig.DSLite{
			Interface: "wan0", Tunnel: "dslite0", LocalAddress: "2001:db8:42::1", Candidates: []string{"aftr.test"},
			Resolvers:      []serviceconfig.AFTRResolver{{Address: "2001:db8:42::53", Port: uint16(socket.LocalAddr().(*net.UDPAddr).Port)}},
			TimeoutSeconds: 1, RetrySeconds: 1, MTU: 1460, Metric: 200,
			B4Peer: "192.0.0.1/32", RouteGateway: "192.0.0.1",
		}}},
	}
	f := startFixtureDaemon(t, config.FromNative(doc))
	t.Cleanup(func() {
		if t.Failed() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var snapshot network.Snapshot
			err := f.control.Do(ctx, "GET", "/v1/network", nil, &snapshot)
			t.Logf("network at failure: %+v error=%v", snapshot, err)
		}
	})
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	wanIndex := wan.Attrs().Index
	state := func() aftr.Snapshot {
		t.Helper()
		var status runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/service.dslite", nil, &status); err != nil {
			t.Fatal(err)
		}
		var snapshot aftr.Snapshot
		if len(status.Snapshot) != 0 {
			if err := json.Unmarshal(status.Snapshot, &snapshot); err != nil {
				t.Fatal(err)
			}
		}
		return snapshot
	}
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == name {
				return module
			}
		}
		return supervisor.ModuleStatus{}
	}
	endpoint := func(want string) (int, bool) {
		t.Helper()
		link, err := netlink.LinkByName("dslite0")
		if err != nil {
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				return 0, false
			}
			t.Fatal(err)
		}
		tunnel, ok := link.(*netlink.Ip6tnl)
		if !ok || tunnel.Proto != unix.IPPROTO_IPIP || tunnel.Local.String() != "2001:db8:42::1" || tunnel.Remote.String() != want || tunnel.Attrs().MTU != 1460 || tunnel.Attrs().Flags&net.FlagUp == 0 {
			return link.Attrs().Index, false
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		b4 := false
		for _, address := range addresses {
			b4 = b4 || address.IPNet.String() == "192.0.0.2/32" && address.Peer != nil && address.Peer.String() == "192.0.0.1/32"
		}
		return link.Attrs().Index, b4 && fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", link.Attrs().Index)
	}
	underlay := func() {
		t.Helper()
		link, err := netlink.LinkByName("wan0")
		if err != nil || link.Attrs().Index != wanIndex || link.Attrs().Flags&net.FlagUp == 0 {
			t.Fatal("AFTR lifecycle altered the independent underlay", err)
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if address.IPNet.String() == "2001:db8:42::1/64" {
				return
			}
		}
		t.Fatal("AFTR lifecycle removed the independent source address")
	}
	until(t, 10*time.Second, func() bool { _, ok := endpoint("2001:db8:42::2"); return ok && state().Available })
	first, ok := endpoint("2001:db8:42::2")
	initialWorker := module("service.dslite")
	if !ok || initialWorker.PID == 0 || initialWorker.Role != "aftr" {
		t.Fatal("initial native AFTR worker or kernel projection missing")
	}
	communicate()
	t.Log("DNS AAAA -> AFTR worker -> coordinator -> native ip6tnl and independent Linux AFTR TCP/UDP")

	dnsMu.Lock()
	remote = "2001:db8:42::3"
	dnsMu.Unlock()
	until(t, 10*time.Second, func() bool { _, ok := endpoint("2001:db8:42::3"); return ok && state().Remote == "2001:db8:42::3" })
	updated, _ := endpoint("2001:db8:42::3")
	if updated != first {
		t.Fatal("AFTR answer change replaced the tunnel instead of updating its endpoint")
	}
	movePeer("2001:db8:42::3")
	communicate()
	t.Log("new DNS answer updated the native tunnel without replacing its identity; TCP/UDP passed")

	baseline := state()
	dnsMu.Lock()
	silent = true
	dnsMu.Unlock()
	var failed aftr.Snapshot
	until(t, 8*time.Second, func() bool {
		failed = state()
		return failed.Failures > baseline.Failures && failed.LastError != ""
	})
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !failed.Available || failed.Remote != "2001:db8:42::3" || failed.ValidUntil == (lifetime.Deadline{}) || !failed.ValidUntil.Live(clock) {
		t.Fatalf("DNS failure did not retain the live bounded last-good endpoint: %+v", failed)
	}
	expires := failed.ValidUntil
	until(t, 8*time.Second, func() bool {
		snapshot := state()
		if snapshot.Available && snapshot.ValidUntil != expires {
			t.Fatalf("DNS deadline changed during silence: endpoint_delta=%v first_failed=%+v next=%+v", int64(snapshot.ValidUntil.Nanos)-int64(expires.Nanos), failed, snapshot)
		}
		link, err := netlink.LinkByName("dslite0")
		if err == nil {
			_ = link
			return false
		}
		var missing netlink.LinkNotFoundError
		if !errors.As(err, &missing) {
			t.Fatal(err)
		}
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if expires.Remaining(clock) > 200*time.Millisecond {
			t.Fatal("last-good endpoint was removed before its original DNS TTL")
		}
		return !snapshot.Available && !fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", 0)
	})
	clock, err = boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	if uint64(clock.BoottimeNanoseconds) > expires.Nanos+uint64(3*time.Second) {
		t.Fatal("expired DNS endpoint survived beyond the coordinator reconciliation interval")
	}
	underlay()
	t.Log("DNS outage preserved the original TTL, then removed the tunnel and IPv4 route while retaining the IPv6 underlay")

	dnsMu.Lock()
	remote, silent = "2001:db8:42::4", false
	dnsMu.Unlock()
	until(t, 8*time.Second, func() bool { _, ok := endpoint("2001:db8:42::4"); return ok && state().Available })
	movePeer("2001:db8:42::4")
	communicate()
	recovered := module("service.dslite")
	if recovered.PID != initialWorker.PID || recovered.Generation != initialWorker.Generation {
		t.Fatal("DNS update, outage or recovery restarted the AFTR process")
	}
	dnsMu.Lock()
	queryCount, invalidCount := queries, wrongQueries
	dnsMu.Unlock()
	if queryCount < 4 || invalidCount != 0 {
		t.Fatalf("independent scoped DNS peer saw %d queries, %d invalid", queryCount, invalidCount)
	}
	// Restart the real writer while the original DNS budget is live, then
	// stop the control owner and its supervisor. Only the writer can fence it.
	writer := module("network")
	if writer.PID < 2 || unix.Kill(writer.PID, unix.SIGKILL) != nil {
		t.Fatal("missing real network worker")
	}
	until(t, 5*time.Second, func() bool { next := module("network"); return next.PID > 1 && next.PID != writer.PID && next.Ready })
	until(t, 10*time.Second, func() bool {
		now, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		_, ok := endpoint("2001:db8:42::4")
		return ok && state().ValidUntil.Remaining(now) > 6*time.Second
	})
	communicate()
	controller := module("control")
	if controller.PID < 2 {
		t.Fatal("missing control worker")
	}
	dnsMu.Lock()
	silent = true
	dnsMu.Unlock()
	var original runtimeplan.InstanceStatus
	if err := f.control.Do(context.Background(), "GET", "/v1/services/service.dslite", nil, &original); err != nil {
		t.Fatal(err)
	}
	expires = state().ValidUntil
	for _, pid := range []int{f.pid, controller.PID} {
		if err := unix.Kill(pid, unix.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGCONT) })
	}
	until(t, 10*time.Second, func() bool {
		link, err := netlink.LinkByName("dslite0")
		if err != nil {
			t.Fatal("writer deleted a link rather than fencing its authority", err)
		}
		now, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if expires.Live(now) {
			if link.Attrs().Flags&net.FlagUp == 0 {
				t.Fatal("original DNS budget was revoked early")
			}
			return false
		}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatal(err)
		}
		return link.Attrs().Flags&net.FlagUp == 0 && len(addresses) == 0 && !fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", 0)
	})
	for _, protocol := range []string{"tcp4", "udp4"} {
		if err := traffic(protocol); err == nil {
			t.Fatal("expired AFTR still carried traffic", protocol)
		}
	}
	underlay()
	t.Log("writer restart retained original DNS budget; stopped control/supervisor: tunnel DOWN, B4 and IPv4 route removed, TCP/UDP stopped")
	for _, pid := range []int{controller.PID, f.pid} {
		if err := unix.Kill(pid, unix.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	// A resumed controller cannot renew the expired observation from receipt.
	until(t, 5*time.Second, func() bool {
		_, err := netlink.LinkByName("dslite0")
		var missing netlink.LinkNotFoundError
		return errors.As(err, &missing)
	})
	replay := healthPBRUpdate(original)
	replay.Sequence++
	if err := f.control.Do(context.Background(), "POST", "/v1/services/state", replay, nil); err != nil && !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("expired AFTR replay", err)
	}
	if _, err := netlink.LinkByName("dslite0"); err == nil {
		t.Fatal("delivery renewed expired AFTR authority")
	}
	dnsMu.Lock()
	silent = false
	dnsMu.Unlock()
	until(t, 10*time.Second, func() bool { _, ok := endpoint("2001:db8:42::4"); return ok })
	communicate()
	// Inspect actual outer IPv6 and inner IPv4 packets in both directions.
	until(t, time.Second, func() bool {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		type flow struct {
			protocol byte
			reply    bool
		}
		counts := map[flow]int{}
		for _, packet := range capture.packets {
			frame := packet.data
			if len(frame) < 14+40+20 || binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_IPV6 || frame[20] != unix.IPPROTO_IPIP || frame[54]>>4 != 4 {
				continue
			}
			inner := frame[54:]
			if inner[9] != unix.IPPROTO_TCP && inner[9] != unix.IPPROTO_UDP {
				continue
			}
			reply := net.IP(inner[12:16]).Equal(net.ParseIP("198.18.0.1")) && net.IP(inner[16:20]).Equal(net.ParseIP("192.0.0.2"))
			request := net.IP(inner[12:16]).Equal(net.ParseIP("192.0.0.2")) && net.IP(inner[16:20]).Equal(net.ParseIP("198.18.0.1"))
			if request || reply {
				counts[flow{inner[9], reply}]++
			}
		}
		return counts[flow{unix.IPPROTO_TCP, false}] > 0 && counts[flow{unix.IPPROTO_TCP, true}] > 0 && counts[flow{unix.IPPROTO_UDP, false}] > 0 && counts[flow{unix.IPPROTO_UDP, true}] > 0
	})
	t.Log("fresh DNS restored actual TCP/UDP after fencing; captured IPv4 TCP and UDP inside IPv6 on the underlay")
	doc.Services = nil
	var committed control.State
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-aftr", Config: config.FromNative(doc)}, &committed); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		_, err := netlink.LinkByName("dslite0")
		var missing netlink.LinkNotFoundError
		return errors.As(err, &missing) && module("service.dslite").PID == 0 && !fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", 0)
	})
	underlay()
	t.Log("configuration removal released all generated DS-Lite resources; the underlay remained configured")
}
