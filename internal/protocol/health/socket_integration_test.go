//go:build linux && integration

package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestScopedTCPAndHTTPProbes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	for _, protocol := range []string{"tcp", "http"} {
		t.Run(protocol, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := Probe(ctx, Config{Resource: "test", Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: protocol, Port: port})
			if !result.OK {
				t.Fatal(result)
			}
			if result.EgressInterface != "lo" || result.SourceAddress != "127.0.0.1" {
				t.Fatal("missing scope evidence", result)
			}
		})
	}
}
func TestScopedDNSProbe(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		response := new(dns.Msg)
		response.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			response.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("192.0.2.1")}}
		}
		_ = w.WriteMsg(response)
	})}
	done := make(chan error, 1)
	go func() { done <- server.ActivateAndServe() }()
	defer func() { _ = server.Shutdown(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := Probe(ctx, Config{Resource: "test", Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: "dns", Port: conn.LocalAddr().(*net.UDPAddr).Port})
	if !result.OK {
		t.Fatal(result)
	}
}

func TestScopedICMPProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := Probe(ctx, Config{Resource: "test", Interface: "lo", SourceAddress: "127.0.0.1", Target: "127.0.0.1", Protocol: "icmp"})
	if !result.OK {
		t.Fatal(result)
	}
}
