// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/localapi"
	mdns "github.com/miekg/dns"
)

func moduleConfig(t *testing.T) Config {
	t.Helper()
	dir, err := os.MkdirTemp("", "r2dns-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := Config{Runtime: testResolverConfig(t, []int{freeTCPPort(t)}), SocketPath: filepath.Join(dir, "api.sock"), StatePath: filepath.Join(dir, "state.json"), EventPath: filepath.Join(dir, "events.jsonl")}
	return c
}

func launchModule(t *testing.T, c Config) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- Run(ctx, c, nil, func() { close(ready) }) }()
	t.Cleanup(cancel)
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run before ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("DNS ready timeout")
	}
	return cancel, done
}

func queryModule(t *testing.T, c Config, network, qname, want string) {
	t.Helper()
	query := new(mdns.Msg)
	query.SetQuestion(qname, mdns.TypeA)
	addr := net.JoinHostPort(c.Runtime.Spec.Listen[0].Addresses[0], strconv.Itoa(c.Runtime.Spec.Listen[0].Port))
	reply, _, err := (&mdns.Client{Net: network, Timeout: time.Second}).Exchange(query, addr)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Rcode != mdns.RcodeSuccess || len(reply.Answer) != 1 {
		t.Fatalf("DNS response: %v", reply)
	}
	answer, ok := reply.Answer[0].(*mdns.A)
	if !ok || answer.A.String() != want {
		t.Fatalf("DNS response: %v, want %s", reply, want)
	}
	if !reply.Authoritative || reply.AuthenticatedData {
		t.Fatalf("local zone AA/AD flags: %v", reply)
	}
}

func TestPublicRunUDPAndTCPConfigUpdateRollbackQueryLogAndStop(t *testing.T) {
	c := moduleConfig(t)
	c.Runtime.Spec.QueryLog.Enabled = true
	c.Runtime.Spec.QueryLog.Path = filepath.Join(filepath.Dir(c.StatePath), "queries.db")
	cancel, done := launchModule(t, c)
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(4 * time.Second):
			t.Error("module did not stop")
		}
	}()
	for _, path := range []string{c.SocketPath, c.StatePath, c.EventPath, c.Runtime.Spec.QueryLog.Path} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private path %s: %v %v", path, info, err)
		}
	}
	queryModule(t, c, "udp", "router.lab.example.", "192.0.2.1")
	queryModule(t, c, "tcp", "router.lab.example.", "192.0.2.1")
	client := localapi.NewClient(c.SocketPath)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status daemonapi.DaemonStatus
		if err := client.Do(context.Background(), http.MethodGet, "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		observed := status.Resources[0].Observed
		if observed["queryLogPath"] == c.Runtime.Spec.QueryLog.Path && observed["queryLogRecords"] != "0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new query database not recording: %#v", observed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPublicRunCancelsInFlightUpstream(t *testing.T) {
	seen := make(chan struct{})
	var once sync.Once
	upstream := startFailureTestUpstream(t, func(_ mdns.ResponseWriter, q *mdns.Msg) {
		if q.Question[0].Name == "pending.example." {
			once.Do(func() { close(seen) })
		}
	})
	c := moduleConfig(t)
	port := c.Runtime.Spec.Listen[0].Port
	c.Runtime = testForwardConfig(t, "waiting", upstream)
	c.Runtime.Spec.Listen[0].Port = port
	c.Runtime.Spec.Sources[0].QueryTimeout = "30s"
	cancel, done := launchModule(t, c)
	queryDone := make(chan error, 1)
	go func() {
		q := new(mdns.Msg)
		q.SetQuestion("pending.example.", mdns.TypeA)
		_, _, err := (&mdns.Client{Timeout: 4 * time.Second}).Exchange(q, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		queryDone <- err
	}()
	select {
	case <-seen:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive query")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run waited for upstream timeout after cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow DNS shutdown")
	}
	select {
	case <-queryDone:
	case <-time.After(2 * time.Second):
		t.Fatal("DNS request did not finish")
	}
	data, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil || state["phase"] != daemonapi.PhaseStopped {
		t.Fatalf("stopped state: %s %v", data, err)
	}
}

func TestCacheAgesAllRecordTTLsAndPreservesOPTFlags(t *testing.T) {
	d := &daemon{cache: map[string]cacheEntry{}, generation: 1}
	q := new(mdns.Msg)
	q.SetQuestion("cached.example.", mdns.TypeA)
	m := failoverSuccessfulReply(q)
	m.SetEdns0(1232, true)
	wire, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	d.cache["answer"] = cacheEntry{Message: wire, Stored: time.Now().Add(-5 * time.Second), Expires: time.Now().Add(20 * time.Second), Generation: 1}
	got, ok := d.cacheGet("answer", 1)
	if !ok {
		t.Fatal("cache miss")
	}
	var reply mdns.Msg
	if err := reply.Unpack(got); err != nil {
		t.Fatal(err)
	}
	if ttl := reply.Answer[0].Header().Ttl; ttl < 19 || ttl > 20 {
		t.Fatalf("TTL %d did not age/cap", ttl)
	}
	if opt := reply.IsEdns0(); opt == nil || !opt.Do() {
		t.Fatal("OPT DNSSEC flags changed as TTL")
	}
	d.cache["answer"] = cacheEntry{Message: wire, Stored: time.Now().Add(-time.Minute), Expires: time.Now().Add(-time.Second), Generation: 1}
	if _, ok := d.cacheGet("answer", 1); ok {
		t.Fatal("expired cache returned")
	}
}

func TestCacheKeysSeparateFlagsClassAndEDNSButNotIDAndCase(t *testing.T) {
	q := new(mdns.Msg)
	q.SetQuestion("Mixed.Example.", mdns.TypeA)
	key := dnsCacheKey("127.0.0.1:53", q)
	same := q.Copy()
	same.Id++
	same.Question[0].Name = strings.ToLower(same.Question[0].Name)
	if dnsCacheKey("127.0.0.1:53", same) != key {
		t.Fatal("case/ID split equivalent query")
	}
	for name, change := range map[string]func(*mdns.Msg){"CD": func(q *mdns.Msg) { q.CheckingDisabled = true }, "DO": func(q *mdns.Msg) { q.SetEdns0(1232, true) }, "class": func(q *mdns.Msg) { q.Question[0].Qclass = mdns.ClassCHAOS }, "RD": func(q *mdns.Msg) { q.RecursionDesired = !q.RecursionDesired }} {
		t.Run(name, func(t *testing.T) {
			other := q.Copy()
			change(other)
			if dnsCacheKey("127.0.0.1:53", other) == key {
				t.Fatal("different query hit same cache key")
			}
		})
	}
}

func TestMinimumRRAndNegativeSOATTL(t *testing.T) {
	q := new(mdns.Msg)
	q.SetQuestion("cached.example.", mdns.TypeA)
	m := failoverSuccessfulReply(q)
	second := mdns.Copy(m.Answer[0])
	second.Header().Ttl = 7
	m.Answer = append(m.Answer, second)
	if got := dnsMessageTTL(m, api.DNSResolverCacheSpec{}); got != 7*time.Second {
		t.Fatal(got)
	}
	m.Answer = nil
	m.Rcode = mdns.RcodeNameError
	m.Ns = []mdns.RR{&mdns.SOA{Hdr: mdns.RR_Header{Name: "example.", Rrtype: mdns.TypeSOA, Class: mdns.ClassINET, Ttl: 60}, Minttl: 7}}
	if got := dnsMessageTTL(m, api.DNSResolverCacheSpec{NegativeTTL: "30s"}); got != 7*time.Second {
		t.Fatal(got)
	}
}

func TestLocalZonesRequireLabelBoundaryAndNeverInventAD(t *testing.T) {
	c := testResolverConfig(t, nil)
	c.Zones[0].Spec.DNSSEC.Enabled = true
	if err := validateRuntime(normalizedRuntime(c, "/tmp/state")); err == nil {
		t.Fatal("unsupported local DNSSEC accepted")
	}
	z := newZoneTable(c.Zones)
	q := new(mdns.Msg)
	q.SetQuestion("router.lab.example.", mdns.TypeA)
	if reply, ok := z.Answer(q, nil); !ok || reply.AuthenticatedData {
		t.Fatalf("invented AD or no answer: %v", reply)
	}
	q.SetQuestion("router.evillab.example.", mdns.TypeA)
	if reply, ok := z.Answer(q, nil); ok {
		t.Fatalf("unrelated suffix claimed: %v", reply)
	}
}

func TestWildcardListenerKeepsSourceRestriction(t *testing.T) {
	c := dnsresolver.RuntimeConfig{Spec: api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Addresses: []string{"0.0.0.0"}, Port: 53, Sources: []string{"private"}}}}}
	s := []runtimeSource{{Spec: api.DNSResolverSourceSpec{Name: "private"}}, {Spec: api.DNSResolverSourceSpec{Name: "public"}}}
	got := sourcesForListen(c, s, "192.0.2.1:53")
	if len(got) != 1 || got[0].Spec.Name != "private" {
		t.Fatalf("wildcard policy bypass: %v", got)
	}
}

func TestModuleRejectsPathCollisionAndUnownedLeaseFile(t *testing.T) {
	c := moduleConfig(t)
	c.Runtime.Spec.QueryLog = api.DNSResolverQueryLogSpec{Enabled: true, Path: c.StatePath}
	if err := Validate(c); err == nil {
		t.Fatal("query log can overwrite state")
	}
	c.Runtime.Spec.QueryLog.Enabled = false
	c.Runtime.Zones[0].Spec.DHCPDerived.LeaseFile = filepath.Join(filepath.Dir(c.StatePath), "missing.leases")
	err := Run(context.Background(), c, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "native input") {
		t.Fatal(fmt.Sprintf("unowned lease file: %v", err))
	}
}
