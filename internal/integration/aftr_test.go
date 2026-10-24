//go:build linux && integration

// SPDX-License-Identifier: BSD-3-Clause

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
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
// worker in the two-ELF process tree; no external ip/nft/route helpers are used.
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

	doc := config.Document{SchemaVersion: 1,
		Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0", Addresses: []string{"2001:db8:42::1/64"}, AddressOptions: map[string]network.AddressOptions{"2001:db8:42::1/64": {NoDAD: true}}}}},
		Services: []serviceconfig.Service{{Name: "dslite", DSLite: &serviceconfig.DSLite{
			Interface: "wan0", Tunnel: "dslite0", LocalAddress: "2001:db8:42::1", Candidates: []string{"aftr.test"},
			Resolvers:      []serviceconfig.AFTRResolver{{Address: "2001:db8:42::53", Port: uint16(socket.LocalAddr().(*net.UDPAddr).Port)}},
			TimeoutSeconds: 1, RetrySeconds: 1, MTU: 1460, Metric: 200,
			B4Peer: "192.0.0.1/32", RouteGateway: "192.0.0.1",
		}}},
	}
	f := startFixtureDaemon(t, doc)
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
	worker := func() supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == "service.dslite" {
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
	initialWorker := worker()
	if !ok || initialWorker.PID == 0 || initialWorker.Role != "aftr" {
		t.Fatal("initial native AFTR worker or kernel projection missing")
	}
	t.Log("DNS AAAA -> AFTR worker -> coordinator -> native ip6tnl, B4 /32 peer and IPv4 gateway route")

	dnsMu.Lock()
	remote = "2001:db8:42::3"
	dnsMu.Unlock()
	until(t, 10*time.Second, func() bool { _, ok := endpoint("2001:db8:42::3"); return ok && state().Remote == "2001:db8:42::3" })
	updated, _ := endpoint("2001:db8:42::3")
	if updated != first {
		t.Fatal("AFTR answer change replaced the tunnel instead of updating its endpoint")
	}
	t.Log("new DNS answer updated the native tunnel without replacing its identity")

	baseline := state()
	dnsMu.Lock()
	silent = true
	dnsMu.Unlock()
	var failed aftr.Snapshot
	until(t, 8*time.Second, func() bool {
		failed = state()
		return failed.Failures > baseline.Failures && failed.LastError != ""
	})
	if !failed.Available || failed.Remote != "2001:db8:42::3" || failed.ValidUntil.IsZero() || !failed.ValidUntil.After(time.Now()) {
		t.Fatalf("DNS failure did not retain the live bounded last-good endpoint: %+v", failed)
	}
	expires := failed.ValidUntil
	until(t, 8*time.Second, func() bool {
		snapshot := state()
		if snapshot.Available && !snapshot.ValidUntil.Equal(expires) {
			t.Fatalf("DNS deadline changed during silence: endpoint_delta=%v first_failed=%+v next=%+v", snapshot.ValidUntil.Sub(expires), failed, snapshot)
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
		if time.Now().Before(expires.Add(-200 * time.Millisecond)) {
			t.Fatal("last-good endpoint was removed before its original DNS TTL")
		}
		return !snapshot.Available && !fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", 0)
	})
	if time.Now().After(expires.Add(3 * time.Second)) {
		t.Fatal("expired DNS endpoint survived beyond the coordinator reconciliation interval")
	}
	underlay()
	t.Log("DNS outage preserved the original TTL, then removed the tunnel and IPv4 route while retaining the IPv6 underlay")

	dnsMu.Lock()
	remote, silent = "2001:db8:42::4", false
	dnsMu.Unlock()
	until(t, 8*time.Second, func() bool { _, ok := endpoint("2001:db8:42::4"); return ok && state().Available })
	recovered := worker()
	if recovered.PID != initialWorker.PID || recovered.Generation != initialWorker.Generation {
		t.Fatal("DNS update, outage or recovery restarted the AFTR process")
	}
	dnsMu.Lock()
	queryCount, invalidCount := queries, wrongQueries
	dnsMu.Unlock()
	if queryCount < 4 || invalidCount != 0 {
		t.Fatalf("independent scoped DNS peer saw %d queries, %d invalid", queryCount, invalidCount)
	}
	doc.Services = nil
	var committed control.State
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-aftr", Config: doc}, &committed); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		_, err := netlink.LinkByName("dslite0")
		var missing netlink.LinkNotFoundError
		return errors.As(err, &missing) && worker().PID == 0 && !fixtureRoute(t, "0.0.0.0/0", "192.0.0.1", 0)
	})
	underlay()
	t.Log("DNS recovery recreated the projection in the same AFTR process; configuration removal released all DS-Lite resources")
}
