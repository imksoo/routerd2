// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/miekg/dns"
)

// These fixtures exercise DNS exchanges on loopback only. Responses and
// canceled requests are the acceptance evidence; watchdogs only bound a hung
// test, and do not qualify production DNS latency or Internet availability.
func failoverDoHServer(t *testing.T, reply func(http.ResponseWriter, *http.Request, *dns.Msg)) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read DoH query: %v", err)
			return
		}
		var query dns.Msg
		if err := query.Unpack(wire); err != nil {
			t.Errorf("unpack DoH query: %v", err)
			return
		}
		reply(w, r, &query)
	}))
	t.Cleanup(server.Close)
	return server
}

func failoverWriteDNS(t *testing.T, w http.ResponseWriter, response *dns.Msg) {
	t.Helper()
	wire, err := response.Pack()
	if err != nil {
		t.Errorf("pack fixture response: %v", err)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	if _, err := w.Write(wire); err != nil {
		t.Errorf("write fixture response: %v", err)
	}
}

func failoverDoHPool(t *testing.T, cfg upstreamPoolConfig, servers ...*httptest.Server) *upstreamPool {
	t.Helper()
	var urls []string
	roots := x509.NewCertPool()
	for _, server := range servers {
		urls = append(urls, server.URL+"/dns-query")
		roots.AddCert(server.Certificate())
	}
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{Upstreams: urls}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	transport := pool.client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(transport.CloseIdleConnections)
	return pool
}

func failoverSuccessfulReply(query *dns.Msg) *dns.Msg {
	response := new(dns.Msg)
	response.SetReply(query)
	response.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP("192.0.2.53"),
	}}
	return response
}

func failoverDualDNSServer(t *testing.T, handler dns.Handler) string {
	t.Helper()
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	for _, server := range []*dns.Server{
		{Listener: tcp, Handler: handler},
		{PacketConn: udp, Handler: handler},
	} {
		started := make(chan struct{})
		done := make(chan error, 1)
		server.NotifyStartedFunc = func() { close(started) }
		go func() { done <- server.ActivateAndServe() }()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("start DNS fixture: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("watchdog: DNS fixture startup was not observed")
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.ShutdownContext(ctx); err != nil {
				t.Errorf("stop DNS fixture: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("DNS fixture exited with error: %v", err)
				}
			case <-ctx.Done():
				t.Error("watchdog: DNS fixture did not stop")
			}
		})
	}
	return tcp.Addr().String()
}

func TestUpstreamTruncatedUDPRetriesSameServerOverTCP(t *testing.T) {
	var udpCalls, tcpCalls atomic.Int32
	address := failoverDualDNSServer(t, dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		response := failoverSuccessfulReply(query)
		if w.LocalAddr().Network() == "udp" {
			udpCalls.Add(1)
			response.Answer = nil
			response.Truncated = true
		} else {
			tcpCalls.Add(1)
		}
		if err := w.WriteMsg(response); err != nil {
			t.Errorf("write DNS fixture response: %v", err)
		}
	}))
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{Upstreams: []string{"udp://" + address}}, upstreamPoolConfig{QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var response dns.Msg
	if err := response.Unpack(wire); err != nil || response.Truncated || len(response.Answer) != 1 {
		t.Fatalf("TCP response=%v unpack error=%v", response, err)
	}
	if udpCalls.Load() != 1 || tcpCalls.Load() != 1 {
		t.Fatalf("requests UDP=%d TCP=%d, want one each", udpCalls.Load(), tcpCalls.Load())
	}
}

func TestUpstreamQueryTimeoutFallsBackWithoutWaitingForProbeTimeout(t *testing.T) {
	primaryCanceled := make(chan struct{})
	primary := failoverDoHServer(t, func(_ http.ResponseWriter, r *http.Request, _ *dns.Msg) {
		<-r.Context().Done()
		close(primaryCanceled)
	})
	var backupCalls atomic.Int32
	backup := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
		backupCalls.Add(1)
		failoverWriteDNS(t, w, failoverSuccessfulReply(query))
	})
	pool := failoverDoHPool(t, upstreamPoolConfig{
		QueryTimeout: time.Second,
		ProbeTimeout: 5 * time.Second,
	}, primary, backup)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	wire, err := pool.Exchange(ctx, dnsProbeQuery(), nil)
	if err != nil {
		t.Fatalf("query-timeout fallback did not produce an answer: %v", err)
	}
	var response dns.Msg
	if err := response.Unpack(wire); err != nil || response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 {
		t.Fatalf("fallback response=%v unpack error=%v", response, err)
	}
	if backupCalls.Load() != 1 {
		t.Fatalf("backup requests=%d, want 1", backupCalls.Load())
	}
	select {
	case <-primaryCanceled:
	case <-ctx.Done():
		t.Fatal("watchdog: primary request cancellation was not observed")
	}
	if got := pool.Snapshot()[0]; got.LastFailure.IsZero() || got.LastError == "" {
		t.Fatalf("primary timeout was not retained: %+v", got)
	}
}

func TestUpstreamQueryAndProbeTimeoutsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		query, probe  time.Duration
		querySucceeds bool
	}{
		{"QueryLonger", time.Second, 20 * time.Millisecond, true},
		{"ProbeLonger", 20 * time.Millisecond, time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The response fits only the longer operation deadline. Neither
			// operation may inherit the other's timeout through the transport.
			server := failoverDoHServer(t, func(w http.ResponseWriter, r *http.Request, query *dns.Msg) {
				timer := time.NewTimer(100 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					failoverWriteDNS(t, w, failoverSuccessfulReply(query))
				case <-r.Context().Done():
				}
			})
			pool := failoverDoHPool(t, upstreamPoolConfig{QueryTimeout: tc.query, ProbeTimeout: tc.probe}, server)
			_, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil)
			if (err == nil) != tc.querySucceeds {
				t.Fatalf("query result does not match its own deadline: %v", err)
			}
			pool.probe(context.Background(), pool.snapshotPointers()[0], nil)
			got := pool.Snapshot()[0]
			if tc.querySucceeds {
				if got.LastError == "" || got.LastFailure.IsZero() {
					t.Fatalf("health probe did not retain its shorter deadline: %+v", got)
				}
			} else if got.LastError != "" || got.LastSuccess.IsZero() {
				t.Fatalf("health probe inherited the shorter query deadline: %+v", got)
			}
		})
	}
}

func TestUpstreamRejectsFailuresAndMismatchedResponsesBeforeFallback(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*dns.Msg)
		raw    []byte
	}{
		{name: "SERVFAIL", mutate: func(m *dns.Msg) { m.Rcode = dns.RcodeServerFailure }},
		{name: "FORMERR", mutate: func(m *dns.Msg) { m.Rcode = dns.RcodeFormatError }},
		{name: "REFUSED", mutate: func(m *dns.Msg) { m.Rcode = dns.RcodeRefused }},
		{name: "wrong ID", mutate: func(m *dns.Msg) { m.Id++ }},
		{name: "query instead of response", mutate: func(m *dns.Msg) { m.Response = false }},
		{name: "wrong opcode", mutate: func(m *dns.Msg) { m.Opcode = dns.OpcodeStatus }},
		{name: "missing question", mutate: func(m *dns.Msg) { m.Question = nil }},
		{name: "wrong name", mutate: func(m *dns.Msg) { m.Question[0].Name = "different.example." }},
		{name: "wrong type", mutate: func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeAAAA }},
		{name: "wrong class", mutate: func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS }},
		{name: "truncated", mutate: func(m *dns.Msg) { m.Truncated = true }},
		{name: "malformed", raw: []byte{1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
				if tt.raw != nil {
					_, _ = w.Write(tt.raw)
					return
				}
				response := failoverSuccessfulReply(query)
				tt.mutate(response)
				failoverWriteDNS(t, w, response)
			})
			var backupCalls atomic.Int32
			backup := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
				backupCalls.Add(1)
				failoverWriteDNS(t, w, failoverSuccessfulReply(query))
			})
			pool := failoverDoHPool(t, upstreamPoolConfig{QueryTimeout: time.Second}, primary, backup)
			wire, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var response dns.Msg
			if err := response.Unpack(wire); err != nil || response.Rcode != dns.RcodeSuccess || backupCalls.Load() != 1 {
				t.Fatalf("fallback response=%v unpack error=%v backup requests=%d", response, err, backupCalls.Load())
			}
			if got := pool.Snapshot()[0]; got.LastError == "" {
				t.Fatalf("invalid response failure was not retained: %+v", got)
			}
		})
	}
}

func TestUpstreamPreservesAuthoritativeAndFilteredAnswers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*dns.Msg)
	}{
		{name: "NXDOMAIN", mutate: func(m *dns.Msg) { m.Rcode = dns.RcodeNameError; m.Answer = nil }},
		{name: "NODATA", mutate: func(m *dns.Msg) { m.Answer = nil }},
		{name: "filtered address", mutate: func(m *dns.Msg) { m.Answer[0].(*dns.A).A = net.IPv4zero }},
		{name: "case insensitive question", mutate: func(m *dns.Msg) { m.Question[0].Name = strings.ToUpper(m.Question[0].Name) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expected := make(chan []byte, 1)
			primary := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
				response := failoverSuccessfulReply(query)
				tt.mutate(response)
				wire, _ := response.Pack()
				expected <- wire
				failoverWriteDNS(t, w, response)
			})
			var backupCalls atomic.Int32
			backup := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
				backupCalls.Add(1)
				failoverWriteDNS(t, w, failoverSuccessfulReply(query))
			})
			pool := failoverDoHPool(t, upstreamPoolConfig{QueryTimeout: time.Second}, primary, backup)
			response, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil)
			if err != nil {
				t.Fatal(err)
			}
			want := <-expected
			if string(response) != string(want) || backupCalls.Load() != 0 {
				t.Fatalf("answer changed or bypassed: response=%x want=%x backup requests=%d", response, want, backupCalls.Load())
			}
		})
	}
}

func TestUpstreamAllFailuresReturnError(t *testing.T) {
	var requests atomic.Int32
	server := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
		requests.Add(1)
		response := new(dns.Msg)
		response.SetRcode(query, dns.RcodeServerFailure)
		failoverWriteDNS(t, w, response)
	})
	pool := failoverDoHPool(t, upstreamPoolConfig{QueryTimeout: time.Second}, server, server)
	response, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil)
	if err == nil || response != nil || requests.Load() != 2 || !strings.Contains(err.Error(), "SERVFAIL") {
		t.Fatalf("all failures: response=%x err=%v requests=%d", response, err, requests.Load())
	}
}

func TestUpstreamCancellationStopsUDPAndDoesNotTryFallback(t *testing.T) {
	primary, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	var backupCalls atomic.Int32
	backup := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
		backupCalls.Add(1)
		failoverWriteDNS(t, w, failoverSuccessfulReply(query))
	})
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{Upstreams: []string{"udp://" + primary.LocalAddr().String(), backup.URL}}, upstreamPoolConfig{QueryTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(backup.Certificate())
	transport := pool.client.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = roots
	t.Cleanup(transport.CloseIdleConnections)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pool.Exchange(ctx, dnsProbeQuery(), nil)
		done <- err
	}()
	_ = primary.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := primary.ReadFrom(make([]byte, 512)); err != nil {
		t.Fatalf("fixture did not observe the initial query: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog: cancellation did not complete the pending exchange")
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("canceled exchange reached fallback %d times", backupCalls.Load())
	}
	for _, upstream := range pool.Snapshot() {
		if !upstream.LastFailure.IsZero() {
			t.Fatalf("caller cancellation marked an upstream failed: %+v", upstream)
		}
	}
}

func TestBootstrapResolverConcurrentSelection(t *testing.T) {
	first, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{BootstrapResolver: []string{first.LocalAddr().String(), second.LocalAddr().String()}}, upstreamPoolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := pool.resolver
	var firstCalls, secondCalls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := resolver.Dial(context.Background(), "udp", "unused")
			if err != nil {
				t.Errorf("bootstrap Dial: %v", err)
				return
			}
			defer conn.Close()
			switch conn.RemoteAddr().String() {
			case first.LocalAddr().String():
				firstCalls.Add(1)
			case second.LocalAddr().String():
				secondCalls.Add(1)
			default:
				t.Errorf("unexpected bootstrap server %s", conn.RemoteAddr())
			}
		}()
	}
	wg.Wait()
	if firstCalls.Load() != 16 || secondCalls.Load() != 16 {
		t.Fatalf("bootstrap selection counts=%d/%d, want 16/16", firstCalls.Load(), secondCalls.Load())
	}
}

func TestDoHBootstrapTimeoutAllowsNextLookup(t *testing.T) {
	var answerBootstrap atomic.Bool
	bootstrap := failoverDualDNSServer(t, dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		if !answerBootstrap.Load() {
			return
		}
		response := new(dns.Msg)
		response.SetReply(query)
		if query.Question[0].Qtype == dns.TypeA {
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("127.0.0.1"),
			}}
		}
		if err := w.WriteMsg(response); err != nil {
			t.Errorf("write bootstrap reply: %v", err)
		}
	}))
	server := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
		failoverWriteDNS(t, w, failoverSuccessfulReply(query))
	})
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{
		Upstreams:         []string{"https://bootstrap-timeout.test:" + port + "/dns-query"},
		BootstrapResolver: []string{bootstrap},
	}, upstreamPoolConfig{QueryTimeout: 100 * time.Millisecond, ProbeTimeout: 100 * time.Millisecond, RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	pool.client.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com"
	t.Cleanup(pool.client.CloseIdleConnections)
	resolved := make(chan httptrace.DNSDoneInfo, 1)
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		DNSDone: func(info httptrace.DNSDoneInfo) { resolved <- info },
	})
	if _, err := pool.Exchange(ctx, dnsProbeQuery(), nil); err == nil {
		t.Fatal("unanswered bootstrap lookup succeeded")
	}
	select {
	case result := <-resolved:
		if result.Err == nil {
			t.Fatal("unanswered bootstrap lookup resolved")
		}
	case <-time.After(time.Second):
		t.Fatal("watchdog: DoH connection kept resolving after its query timeout")
	}
	answerBootstrap.Store(true)
	if _, err := pool.Exchange(context.Background(), dnsProbeQuery(), nil); err != nil {
		t.Fatalf("query after bootstrap recovery: %v", err)
	}
}

func TestDoHProbeBootstrapCanOutlastQueryTimeout(t *testing.T) {
	var bootstrapCalls atomic.Int32
	bootstrap := failoverDualDNSServer(t, dns.HandlerFunc(func(w dns.ResponseWriter, query *dns.Msg) {
		bootstrapCalls.Add(1)
		time.Sleep(100 * time.Millisecond)
		response := new(dns.Msg)
		response.SetReply(query)
		if query.Question[0].Qtype == dns.TypeA {
			response.Answer = []dns.RR{&dns.A{
				Hdr: dns.RR_Header{Name: query.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("127.0.0.1"),
			}}
		}
		if err := w.WriteMsg(response); err != nil {
			t.Errorf("write delayed bootstrap reply: %v", err)
		}
	}))
	server := failoverDoHServer(t, func(w http.ResponseWriter, _ *http.Request, query *dns.Msg) {
		failoverWriteDNS(t, w, failoverSuccessfulReply(query))
	})
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{
		Upstreams:         []string{"https://probe-bootstrap.test:" + port + "/dns-query"},
		BootstrapResolver: []string{bootstrap},
	}, upstreamPoolConfig{QueryTimeout: 20 * time.Millisecond, ProbeTimeout: time.Second, PassThreshold: 1, RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	pool.client.Transport.(*http.Transport).TLSClientConfig.ServerName = "example.com"
	t.Cleanup(pool.client.CloseIdleConnections)
	pool.probe(context.Background(), pool.snapshotPointers()[0], nil)
	if got := pool.Snapshot()[0]; bootstrapCalls.Load() == 0 || got.Phase != upstreamHealthy || got.LastSuccess.IsZero() {
		t.Fatalf("first DoH probe did not complete bootstrap within its own deadline: calls=%d status=%+v", bootstrapCalls.Load(), got)
	}
}

func TestDoHReusedConnectionResetReplaysQueryOnce(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "HTTP1"
		if http2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var connection net.Conn
			requests := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (r.ProtoMajor == 2) != http2 {
					t.Errorf("unexpected negotiated HTTP version %s", r.Proto)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var query dns.Msg
				if err := query.Unpack(body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests++
				attempt := requests
				current := connection
				mu.Unlock()
				if attempt == 2 {
					tcp := current.(*tls.Conn).NetConn().(*net.TCPConn)
					if err := tcp.SetLinger(0); err != nil {
						t.Error(err)
					}
					if err := tcp.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				failoverWriteDNS(t, w, new(dns.Msg).SetReply(&query))
			}))
			server.EnableHTTP2 = http2
			server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
				if state == http.StateNew {
					mu.Lock()
					connection = conn
					mu.Unlock()
				}
			}
			server.StartTLS()
			t.Cleanup(server.Close)
			pool := failoverDoHPool(t, upstreamPoolConfig{QueryTimeout: time.Second}, server)
			for _, question := range []string{"warm.test.", "after-reset.test."} {
				query := new(dns.Msg)
				query.SetQuestion(question, dns.TypeA)
				wire, err := query.Pack()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				response, err := pool.exchangeOne(ctx, pool.upstreams[0], wire)
				cancel()
				if err != nil {
					t.Fatal("query over reused TLS connection", err)
				}
				var answer dns.Msg
				if err := answer.Unpack(response); err != nil || answer.Rcode != dns.RcodeSuccess {
					t.Fatal(answer.Rcode, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 3 {
				t.Fatal("reset query was not replayed exactly once", requests)
			}
		})
	}
}
