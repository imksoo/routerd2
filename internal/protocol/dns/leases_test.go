// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	mdns "github.com/miekg/dns"
)

func nativeProjection(now time.Time) dnsconfig.LeaseProjection {
	return dnsconfig.LeaseProjection{Resource: "lab", ConfigDigest: strings.Repeat("a", 64), Sets: []dnsconfig.LeaseSet{{Zone: "lan-zone", Source: "DHCPv4Server/dhcpv4", Epoch: strings.Repeat("b", 32), Sequence: 1, ObservedAt: now, ValidUntil: now.Add(5 * time.Second), Records: []dnsconfig.LeaseRecord{{Hostname: "laptop", Address: "192.0.2.73", ExpiresAt: now.Add(time.Hour)}}}}}
}

func answerNative(t *testing.T, d *daemon, name string, typ uint16) *mdns.Msg {
	t.Helper()
	q := new(mdns.Msg)
	q.SetQuestion(name, typ)
	_, _, zones, _ := d.runtimeSnapshot()
	out, ok := zones.Answer(q, nil)
	if !ok {
		return nil
	}
	return out
}

func TestNativeLeaseSourceScopeStaticPrecedenceAndMonotonicExpiry(t *testing.T) {
	d := newTestDaemon(t, "", testResolverConfig(nil), true)
	d.nativeLeaseMode = true
	p := nativeProjection(time.Now())
	p.Sets[0].Records = append(p.Sets[0].Records, dnsconfig.LeaseRecord{Hostname: "router", Address: "192.0.2.99", ExpiresAt: time.Now().Add(time.Hour)})
	if err := d.applyLeaseProjection(p); err != nil {
		t.Fatal(err)
	}
	a := answerNative(t, d, "laptop.lab.example.", mdns.TypeA)
	if a == nil || len(a.Answer) != 1 || a.Answer[0].Header().Ttl > 4 || a.Answer[0].(*mdns.A).A.String() != "192.0.2.73" {
		t.Fatalf("native A lacks finite ownership lifetime: %v", a)
	}
	ptr := answerNative(t, d, "73.2.0.192.in-addr.arpa.", mdns.TypePTR)
	if ptr == nil || ptr.Answer[0].Header().Ttl > 4 {
		t.Fatalf("native PTR lacks finite lifetime: %v", ptr)
	}
	static := answerNative(t, d, "router.lab.example.", mdns.TypeA)
	if static.Answer[0].(*mdns.A).A.String() != "192.0.2.1" {
		t.Fatal("DHCP hostname replaced static ownership", static)
	}
	bad := dnsconfig.NormalizeLeaseProjection(p)
	bad.Sets[0].Source = "unauthorized"
	if err := d.applyLeaseProjection(bad); err == nil {
		t.Fatal("unconfigured DHCP source reached the DNS zone")
	}
	generation := d.generation
	d.cache["preserve-upstream"] = cacheEntry{Message: []byte("retained")}
	p.Sets[0].Sequence++
	p.Sets[0].ObservedAt = time.Now()
	p.Sets[0].ValidUntil = p.Sets[0].ObservedAt.Add(5 * time.Second)
	if err := d.applyLeaseProjection(p); err != nil {
		t.Fatal(err)
	}
	if d.generation != generation || len(d.cache) != 1 {
		t.Fatal("ownership heartbeat invalidated unrelated upstream cache")
	}
	key := "lan-zone/dhcpv4"
	receipt := d.leaseReceipts[key]
	receipt.deadline = time.Now().Add(-time.Second)
	d.leaseReceipts[key] = receipt
	if err := d.applyLeaseProjection(p); err != nil {
		t.Fatal(err)
	}
	if out := answerNative(t, d, "laptop.lab.example.", mdns.TypeA); out == nil || out.Rcode != mdns.RcodeNameError {
		t.Fatalf("duplicate sequence renewed expired ownership: %v", out)
	}
	if out := answerNative(t, d, "73.2.0.192.in-addr.arpa.", mdns.TypePTR); out != nil {
		t.Fatal("expired native lease retained its PTR", out)
	}
	p.Sets[0].Sequence++
	if err := d.applyLeaseProjection(p); err != nil {
		t.Fatal(err)
	}
	if out := answerNative(t, d, "laptop.lab.example.", mdns.TypeA); len(out.Answer) != 1 {
		t.Fatal("new acknowledged source did not restore lease")
	}
	p.Sets = nil
	if err := d.applyLeaseProjection(p); err != nil {
		t.Fatal(err)
	}
	if out := answerNative(t, d, "laptop.lab.example.", mdns.TypeA); out.Rcode != mdns.RcodeNameError {
		t.Fatal("complete source removal retained lease")
	}
}

func TestNativeLeaseUpdatesKeepTCPConnectionAndExpireWhenInputCloses(t *testing.T) {
	cfg := moduleConfig(t)
	updates := make(chan dnsconfig.LeaseProjection, 1)
	p := nativeProjection(time.Now())
	updates <- p
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- RunWithLeaseUpdates(ctx, cfg, updates, func() { close(ready) }) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("native DNS lease worker did not drain")
		}
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("DNS native lease readiness timed out")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Runtime.Spec.Listen[0].Port))
	conn, err := mdns.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	query := func(name string) (*mdns.Msg, error) {
		q := new(mdns.Msg)
		q.SetQuestion(name, mdns.TypeA)
		out, _, err := (&mdns.Client{Net: "tcp", Timeout: time.Second}).ExchangeWithConn(q, conn)
		return out, err
	}
	out, err := query("laptop.lab.example.")
	if err != nil || len(out.Answer) != 1 || out.Answer[0].(*mdns.A).A.String() != "192.0.2.73" {
		t.Fatalf("initial leased answer: %v %v", out, err)
	}
	p.Sets[0].Records[0].Address = "192.0.2.74"
	p.Sets[0].Sequence++
	p.Sets[0].ObservedAt = time.Now()
	p.Sets[0].ValidUntil = p.Sets[0].ObservedAt.Add(2 * time.Second)
	updates <- p
	deadline := time.Now().Add(time.Second)
	for {
		out, err = query("laptop.lab.example.")
		if err != nil {
			t.Fatalf("native update closed existing TCP connection: %v", err)
		}
		if len(out.Answer) == 1 && out.Answer[0].(*mdns.A).A.String() == "192.0.2.74" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("updated lease did not reach the existing DNS connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client := localapi.NewClient(cfg.SocketPath)
	if err := client.Do(ctx, http.MethodPost, "/v1/leases", dhcpLeaseEvent{Action: daemonapi.DHCPLeaseActionAdded, IP: "192.0.2.55", Hostname: "bypass"}, nil); err == nil {
		t.Fatal("unscoped compatibility event bypassed native lease ownership")
	}
	close(updates)
	deadline = time.Now().Add(3 * time.Second)
	for {
		out, err = query("laptop.lab.example.")
		if err != nil {
			t.Fatal(err)
		}
		if out.Rcode == mdns.RcodeNameError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed input made DHCP DNS ownership permanent")
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, err = query("router.lab.example.")
	if err != nil || len(out.Answer) != 1 {
		t.Fatal(fmt.Errorf("lease expiration affected static DNS: %v %v", out, err))
	}
}
