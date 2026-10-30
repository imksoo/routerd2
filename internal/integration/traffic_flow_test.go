//go:build linux && integration

package integration

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/protocol/webconsoleconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestDaemonTrafficFlowHistory(t *testing.T) { testDaemonTrafficFlowHistory(t, false) }
func TestDaemonTrafficFlowNDPI(t *testing.T)    { testDaemonTrafficFlowHistory(t, true) }

func testDaemonTrafficFlowHistory(t *testing.T, native bool) {
	engine, source := "builtin", "builtin-payload"
	flags := "source: conntrack, includeTLSSNI: true}"
	if native {
		engine, source = "ndpi", "libndpi"
		flags = "source: conntrack, includeTLSSNI: true, includeApplicationLayer: true}"
	}
	if !isolateFixture(t, t.Name()) {
		return
	}
	lan := fixturePeerNamespace(t, "lan0", "client0")
	wan := fixturePeerNamespace(t, "wan0", "peer0")
	_ = healthPBRCapture(t, "lan0")
	_ = healthPBRCapture(t, "wan0")
	for _, peer := range []struct {
		ns                   netns.NsHandle
		iface, cidr, gateway string
	}{
		{lan, "client0", "192.0.2.2/24", "192.0.2.1"}, {wan, "peer0", "198.51.100.2/24", "198.51.100.1"},
	} {
		if err := fixtureInsideNamespace(peer.ns, func() error {
			link, err := netlink.LinkByName(peer.iface)
			if err != nil {
				return err
			}
			addr, err := netlink.ParseAddr(peer.cidr)
			if err != nil {
				return err
			}
			if err := netlink.AddrAdd(link, addr); err != nil {
				return err
			}
			return netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: net.ParseIP(peer.gateway)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Start with accounting disabled in this isolated namespace. The imported
	// product setting must enable it before the first connection is created.
	initialTimestamp := "0"
	if native {
		initialTimestamp = "1"
	}
	for key, value := range map[string]string{"nf_conntrack_acct": "0", "nf_conntrack_timestamp": initialTimestamp, "nf_conntrack_udp_timeout": "3", "nf_conntrack_udp_timeout_stream": "3"} {
		if err := os.WriteFile("/proc/sys/net/netfilter/"+key, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var udp, archive, dnsPackets net.PacketConn
	var tcp net.Listener
	if err := fixtureInsideNamespace(wan, func() error {
		var err error
		udp, err = net.ListenPacket("udp4", "198.51.100.2:18081")
		if err != nil {
			return err
		}
		archive, err = net.ListenPacket("udp4", "198.51.100.2:5514")
		if err != nil {
			return err
		}
		dnsPackets, err = net.ListenPacket("udp4", "198.51.100.2:53")
		if err != nil {
			return err
		}
		tcp, err = net.Listen("tcp4", "198.51.100.2:18081")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	var dnsRequests, stunPackets atomic.Int32
	defer dnsPackets.Close()
	go func() {
		var payload [2048]byte
		for {
			n, peer, err := dnsPackets.ReadFrom(payload[:])
			if err != nil {
				return
			}
			if n >= 20 && payload[4] == 0x21 && payload[5] == 0x12 && payload[6] == 0xa4 && payload[7] == 0x42 {
				stunPackets.Add(1)
				continue
			}
			request := new(dns.Msg)
			if err := request.Unpack(payload[:n]); err != nil || len(request.Question) == 0 {
				continue
			}
			dnsRequests.Add(1)
			if request.Question[0].Name != "single.routerd.test." {
				continue
			}
			answer := new(dns.Msg)
			answer.SetReply(request)
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP("192.0.2.123")}}
			wire, err := answer.Pack()
			if err == nil {
				_, _ = dnsPackets.WriteTo(wire, peer)
			}
		}
	}()
	defer tcp.Close()
	// Use the standard TLS implementation, not a synthetic ClientHello. X25519
	// keeps its hello inside the old classifier's single-packet parsing boundary.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"flow.routerd.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	var plainHTTP atomic.Bool
	defer archive.Close()
	go func() {
		var b [4096]byte
		for {
			n, p, err := udp.ReadFrom(b[:])
			if err != nil {
				return
			}
			_, _ = udp.WriteTo(b[:n], p)
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			httpMode := plainHTTP.Load()
			go func() {
				defer c.Close()
				if httpMode {
					request, err := http.ReadRequest(bufio.NewReader(c))
					if err != nil {
						return
					}
					request.Body.Close()
					response := &http.Response{StatusCode: 200, ProtoMajor: 1, ProtoMinor: 1, ContentLength: 2, Body: io.NopCloser(strings.NewReader("OK"))}
					if err := response.Write(c); err != nil {
						return
					}
				} else {
					c = tls.Server(c, tlsConfig)
				}
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	archived := make(chan string, 128)
	go func() {
		var b [65536]byte
		for {
			n, _, err := archive.ReadFrom(b[:])
			if err != nil {
				return
			}
			select {
			case archived <- string(b[:n]):
			default:
			}
		}
	}()
	imported, err := legacyconfig.Compile([]byte(strings.Replace(trafficFlowFixture, "source: conntrack}", flags, 1)))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	var historyPath, historyService string
	for _, res := range imported.Document.Resources {
		if res.Service != nil && res.Service.TrafficFlow != nil {
			cfg := res.Service.TrafficFlow
			historyService = res.Service.ModuleName()
			if cfg.IntervalSeconds != 0 || cfg.Retention == nil || cfg.Retention.IntervalSeconds != 86400 {
				t.Fatal("legacy defaults changed", cfg)
			}
			// Shorter configured sampling and expiry intervals keep this isolated gate
			// bounded; retention still uses actual first-observation timestamps.
			cfg.IntervalSeconds = 1
			cfg.Retention.IntervalSeconds = 1
			historyPath = filepath.Join(work, "state", "services", res.Service.Name, "traffic-flows.db")
		}
	}
	if historyPath == "" {
		t.Fatal("missing traffic flow owner")
	}
	if err := os.MkdirAll(historyPath, 0700); err != nil {
		t.Fatal(err)
	}
	imported.Document.Resources = append(imported.Document.Resources, resourceprogram.Resource{
		ID: "service/history-console", Service: &serviceconfig.Service{Name: "history-console", WebConsole: &webconsoleconfig.Config{Listen: netip.MustParseAddrPort("192.0.2.1:8080"), BasePath: "/ops/", Title: "Communication history"}},
	})
	f := startFixtureDaemonAt(t, imported.Document, work)
	console := func(query url.Values) (int, string) {
		t.Helper()
		query.Set("service", historyService)
		var connection net.Conn
		if err := fixtureInsideNamespace(lan, func() error {
			var err error
			connection, err = net.DialTimeout("tcp4", "192.0.2.1:8080", time.Second)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest(http.MethodGet, "http://192.0.2.1:8080/ops/flows?"+query.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = request.Write(connection); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	for _, key := range []string{"nf_conntrack_acct", "nf_conntrack_timestamp"} {
		if value, err := os.ReadFile("/proc/sys/net/netfilter/" + key); err != nil || strings.TrimSpace(string(value)) != "1" {
			t.Fatal("product did not enable connection observation", key, string(value), err)
		}
	}
	connect := func(proto string) net.Conn {
		t.Helper()
		var c net.Conn
		if err := fixtureInsideNamespace(lan, func() error {
			var err error
			c, err = net.DialTimeout(proto, "198.51.100.2:18081", time.Second)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(proto, "tcp") {
			secure := tls.Client(c, &tls.Config{ServerName: "flow.routerd.test", InsecureSkipVerify: true, CurvePreferences: []tls.CurveID{tls.X25519}})
			if err := secure.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := secure.Handshake(); err != nil {
				c.Close()
				t.Fatal(err)
			}
			c = secure
		}
		return c
	}
	probe := func(c net.Conn) {
		t.Helper()
		payload := []byte(strings.Repeat("traffic-flow", 32))
		if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write(payload); err != nil {
			t.Fatal(err)
		}
		reply := make([]byte, len(payload))
		if _, err := io.ReadFull(c, reply); err != nil || string(reply) != string(payload) {
			t.Fatal("forwarded echo", err)
		}
	}
	udpClient, tcpClient := connect("udp4"), connect("tcp4")
	defer udpClient.Close()
	defer tcpClient.Close()
	probe(udpClient)
	probe(tcpClient)
	t.Log("TCP/UDP forwarded through source NAT while the history path was unusable")
	if status, _ := console(url.Values{}); status != http.StatusServiceUnavailable {
		t.Fatal("unreadable history must not look empty", status)
	}
	probe(udpClient)
	probe(tcpClient)
	if err := os.Remove(historyPath); err != nil {
		t.Fatal(err)
	}
	historyCreated := false
	history := func() []logstore.TrafficFlow {
		t.Helper()
		db, err := logstore.OpenTrafficFlowLogReadOnly(historyPath)
		if err != nil {
			if historyCreated {
				t.Fatal("read previously created history", err)
			}
			return nil
		}
		defer db.Close()
		rows, err := db.List(t.Context(), logstore.TrafficFlowFilter{Limit: 100})
		if err != nil {
			if historyCreated {
				t.Fatal("query previously created history", err)
			}
			return nil
		}
		historyCreated = true
		return rows
	}
	var first, firstTLS logstore.TrafficFlow
	until(t, 6*time.Second, func() bool {
		probe(udpClient)
		probe(tcpClient)
		protocols := map[string]bool{}
		for _, row := range history() {
			if row.ClientAddress == "192.0.2.2" && row.PeerAddress == "198.51.100.2" && row.PeerPort == 18081 && row.NATTranslatedAddress == "198.51.100.1" && row.Accounting && row.BytesOut > 0 && row.BytesIn > 0 {
				if row.Protocol == "tcp" && (row.TLSSNI != "flow.routerd.test" || row.Engine != engine || row.Source != source || (native && (row.Metadata["libndpi.version"] == "" || row.AppConfidence < 85))) {
					continue
				}
				protocols[row.Protocol] = true
				if row.Protocol == "udp" {
					first = row
				} else if row.Protocol == "tcp" {
					firstTLS = row
				}
			}
		}
		return protocols["tcp"] && protocols["udp"]
	})
	// The operator uses the real HTTP console and the existing collector database.
	status, body := console(url.Values{"client": {"192.0.2.2"}, "protocol": {"TCP"}, "since": {firstTLS.StartedAt.Format(time.RFC3339Nano)}})
	if status != http.StatusOK || !strings.Contains(body, firstTLS.FlowKey) || strings.Contains(body, first.FlowKey) || !strings.Contains(body, "flow.routerd.test") || !strings.Contains(body, "<form") {
		t.Fatal("operator could not search classified TCP history", status, body)
	}
	if status, body := console(url.Values{"client": {"203.0.113.99"}}); status != http.StatusOK || strings.Contains(body, firstTLS.FlowKey) || strings.Contains(body, first.FlowKey) {
		t.Fatal("client filter returned unrelated history", status, body)
	}
	if status, _ := console(url.Values{"until": {"bad-timestamp"}}); status != http.StatusBadRequest {
		t.Fatal("invalid time accepted", status)
	}
	t.Log("independent HTTP client searched the real classified history by address, protocol and start time")
	// One real DNS request and one response must survive connection confirmation;
	// neither the client nor this assertion retransmits the application request.
	var dnsClient net.Conn
	if err := fixtureInsideNamespace(lan, func() error { var err error; dnsClient, err = net.Dial("udp4", "198.51.100.2:53"); return err }); err != nil {
		t.Fatal(err)
	}
	defer dnsClient.Close()
	if err := dnsClient.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	question := new(dns.Msg)
	question.SetQuestion("single.routerd.test.", dns.TypeA)
	exchange := &dns.Conn{Conn: dnsClient}
	if err := exchange.WriteMsg(question); err != nil {
		t.Fatal(err)
	}
	answer, err := exchange.ReadMsg()
	if err != nil || len(answer.Answer) != 1 {
		t.Fatal("single DNS response", err)
	}
	until(t, 5*time.Second, func() bool {
		for _, row := range history() {
			if row.Protocol == "udp" && row.PeerPort == 53 && row.DNSQuery == "single.routerd.test" && row.Engine == engine && row.Source == source {
				if row.TLSSNI != "" || row.Metadata["tls.sni"] != "" {
					t.Fatalf("DNS incorrectly stored TLS SNI: engine=%s sni=%q metadata_sni=%q", row.Engine, row.TLSSNI, row.Metadata["tls.sni"])
				}
				return true
			}
		}
		return false
	})
	if dnsRequests.Load() != 1 {
		t.Fatal("DNS acceptance retransmitted", dnsRequests.Load())
	}
	t.Log("one independent DNS request/response retained its application and query metadata")
	// A single forwarded DNS request with no reply still creates its classification.
	// Once the kernel expires it, the same UDP socket sends a different application.
	var oneWay net.Conn
	if err := fixtureInsideNamespace(lan, func() error { var err error; oneWay, err = net.Dial("udp4", "198.51.100.2:53"); return err }); err != nil {
		t.Fatal(err)
	}
	defer oneWay.Close()
	question.SetQuestion("oneway.routerd.test.", dns.TypeA)
	if err := (&dns.Conn{Conn: oneWay}).WriteMsg(question); err != nil {
		t.Fatal(err)
	}
	var oneWayDNS logstore.TrafficFlow
	until(t, 5*time.Second, func() bool {
		probe(udpClient)
		for _, row := range history() {
			if row.ClientPort == oneWay.LocalAddr().(*net.UDPAddr).Port && row.Protocol == "udp" && row.DNSQuery == "oneway.routerd.test" && row.Engine == engine && row.Source == source && row.PacketsOut == 1 && row.PacketsIn == 0 {
				if row.TLSSNI != "" || row.Metadata["tls.sni"] != "" {
					t.Fatal("one-way DNS has TLS metadata", row.TLSSNI, row.Metadata["tls.sni"])
				}
				oneWayDNS = row
				return true
			}
		}
		return false
	})
	until(t, 5*time.Second, func() bool {
		probe(udpClient)
		for _, row := range history() {
			if row.FlowKey == oneWayDNS.FlowKey {
				return !row.EndedAt.IsZero()
			}
		}
		return false
	})
	// STUN Binding request, one datagram and no response or retransmission.
	binding := []byte{0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	if _, err := oneWay.Write(binding); err != nil {
		t.Fatal(err)
	}
	var nextUDP logstore.TrafficFlow
	until(t, 5*time.Second, func() bool {
		probe(udpClient)
		for _, row := range history() {
			if row.ClientPort == oneWay.LocalAddr().(*net.UDPAddr).Port && row.Protocol == "udp" && row.AppName == "stun" && row.FlowKey != oneWayDNS.FlowKey && row.StartedAt.After(oneWayDNS.StartedAt) && row.DNSQuery == "" && row.Metadata["dns.query"] == "" && row.PacketsOut == 1 && row.PacketsIn == 0 {
				if row.TLSSNI != "" || row.Metadata["tls.sni"] != "" {
					t.Fatal("STUN has TLS metadata", row.TLSSNI, row.Metadata["tls.sni"])
				}
				nextUDP = row
				return true
			}
		}
		return false
	})
	if dnsRequests.Load() != 2 || stunPackets.Load() != 1 {
		t.Fatal("single direction classification retransmitted", dnsRequests.Load(), stunPackets.Load())
	}
	t.Logf("one-way DNS retained its sole request; same UDP endpoints became STUN engine=%s source=%s without old DNS metadata", nextUDP.Engine, nextUDP.Source)
	// The observation budget is a connection packet count, not a permanent
	// copy of all established traffic. Force this TLS connection beyond it.
	for i := 0; i < 20; i++ {
		probe(tcpClient)
	}
	observed := func() uint64 {
		t.Helper()
		table := &nftables.Table{Name: "routerd2", Family: nftables.TableFamilyINet}
		rules, err := (&nftables.Conn{}).GetRules(table, &nftables.Chain{Name: "flow_observation", Table: table})
		if err != nil {
			t.Fatal(err)
		}
		var count uint64
		found := false
		for _, rule := range rules {
			for _, e := range rule.Exprs {
				if counter, ok := e.(*expr.Counter); ok {
					found = true
					count += counter.Packets
				}
			}
		}
		if !found {
			t.Fatal("packet observation counter is missing")
		}
		return count
	}
	before := observed()
	for i := 0; i < 8; i++ {
		probe(tcpClient)
	}
	if after := observed(); after != before {
		t.Fatalf("packet observation did not stop at its connection budget: %d -> %d", before, after)
	}
	t.Logf("TLS classification engine=%s source=%s; established traffic no longer generated packet observations", engine, source)
	lock, err := sql.Open("sqlite", historyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		probe(udpClient)
		probe(tcpClient)
		time.Sleep(600 * time.Millisecond)
	}
	if _, err := lock.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		probe(udpClient)
		for _, row := range history() {
			if row.FlowKey == first.FlowKey && row.StartedAt.Equal(first.StartedAt) && row.BytesOut > first.BytesOut {
				return true
			}
		}
		return false
	})
	t.Log("real TLS/SNI classification was stored with source NAT counters; history recovered from a locked database; counters advanced without splitting the connection or blocking traffic")
	if native {
		// Reuse the exact endpoints for a new application. Abortive close avoids
		// TIME_WAIT without changing any shared TCP settings in the fixture.
		local := tcpClient.LocalAddr().(*net.TCPAddr)
		if err := tcpClient.(*tls.Conn).NetConn().(*net.TCPConn).SetLinger(0); err != nil {
			t.Fatal(err)
		}
		tcpClient.Close()
		plainHTTP.Store(true)
		var next net.Conn
		if err := fixtureInsideNamespace(lan, func() error {
			var err error
			next, err = (&net.Dialer{LocalAddr: local, Timeout: time.Second}).Dial("tcp4", "198.51.100.2:18081")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		defer next.Close()
		if err := next.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("GET", "http://changed.routerd.test/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := request.Write(next); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(next), request)
		if err != nil {
			t.Fatal(err)
		}
		if body, err := io.ReadAll(response.Body); err != nil || string(body) != "OK" {
			t.Fatal("reused TCP HTTP response", err)
		}
		response.Body.Close()
		until(t, 5*time.Second, func() bool {
			oldEnded, newHTTP := false, false
			for _, row := range history() {
				if row.FlowKey == firstTLS.FlowKey {
					oldEnded = !row.EndedAt.IsZero() && row.TLSSNI == "flow.routerd.test"
				}
				if row.ClientPort == local.Port && row.Protocol == "tcp" && row.PeerPort == 18081 && row.AppName == "http" && row.Engine == "ndpi" && row.HTTPHost == "changed.routerd.test" {
					newHTTP = row.FlowKey != firstTLS.FlowKey && row.StartedAt.After(firstTLS.StartedAt) && row.EndedAt.IsZero() && row.TLSSNI == "" && row.Metadata["tls.sni"] == ""
				}
			}
			return oldEnded && newHTTP
		})
		next.Close()
		plainHTTP.Store(false)
		tcpClient = connect("tcp4")
		defer tcpClient.Close()
		probe(tcpClient)
		t.Log("same TCP endpoints created a separate HTTP history row, without old TLS metadata; the earlier TLS row ended and retained its original start")
	}
	f = f.restart()
	probe(udpClient)
	probe(tcpClient)
	until(t, 4*time.Second, func() bool {
		udpRetained, tlsRetained := false, false
		for _, row := range history() {
			if row.FlowKey == first.FlowKey && row.StartedAt.Equal(first.StartedAt) {
				udpRetained = true
			}
			if row.Protocol == "tcp" && row.ClientAddress == "192.0.2.2" && row.PeerPort == 18081 && row.TLSSNI == "flow.routerd.test" && row.Source == source {
				tlsRetained = true
			}
		}
		return udpRetained && tlsRetained
	})
	udpClient.Close()
	tcpClient.Close()
	until(t, 7*time.Second, func() bool {
		for _, row := range history() {
			if row.FlowKey == first.FlowKey {
				return !row.EndedAt.IsZero()
			}
		}
		return false
	})
	var archiveMatch bool
	until(t, 25*time.Second, func() bool {
		fresh := connect("udp4")
		probe(fresh)
		fresh.Close()
		present := false
		for _, row := range history() {
			present = present || row.FlowKey == first.FlowKey
		}
		for {
			select {
			case message := <-archived:
				_, body, ok := strings.Cut(message, "trafficFlows ")
				if !ok {
					continue
				}
				var record struct {
					FlowKey string `json:"flow_key"`
					Started int64  `json:"ts_started"`
				}
				if json.NewDecoder(strings.NewReader(body)).Decode(&record) == nil && record.FlowKey == first.FlowKey && record.Started == first.StartedAt.UnixNano() {
					archiveMatch = true
				}
			default:
				return !present && archiveMatch
			}
		}
	})
	t.Log("closed connection was marked ended, survived restart and reached the UDP archive before age-based deletion")
	// Packet observation owns its required accounting after the explicit
	// setting is withdrawn, then restores the original value when removed.
	observationOnly := imported.Document
	observationOnly.Resources = nil
	for _, res := range imported.Document.Resources {
		if res.Forwarding != nil {
			forwarding := *res.Forwarding
			forwarding.ConntrackAccounting = nil
			res.Forwarding = &forwarding
		}
		observationOnly.Resources = append(observationOnly.Resources, res)
	}
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "accounting-owned-by-observation", Config: observationOnly}, nil); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_acct"); err != nil || strings.TrimSpace(string(value)) != "1" {
		t.Fatal("observation lost required accounting", string(value), err)
	}
	disabled := observationOnly
	disabled.Resources = nil
	for _, res := range imported.Document.Resources {
		if res.Firewall != nil {
			next := *res.Firewall
			next.FlowObservationGroup = 0
			res.Firewall = &next
		}
		if res.Service == nil || res.Service.TrafficFlow == nil {
			if res.Forwarding != nil {
				forwarding := *res.Forwarding
				forwarding.ConntrackAccounting = nil
				res.Forwarding = &forwarding
			}
			disabled.Resources = append(disabled.Resources, res)
		}
	}
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 2, RequestID: "remove-flow-history", Config: disabled}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		var state struct {
			Modules []struct {
				Role string `json:"role"`
			} `json:"modules"`
		}
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &state); err != nil {
			return false
		}
		for _, m := range state.Modules {
			if m.Role == "traffic_flow" {
				return false
			}
		}
		return true
	})
	for key, original := range map[string]string{"nf_conntrack_acct": "0", "nf_conntrack_timestamp": initialTimestamp} {
		if value, err := os.ReadFile("/proc/sys/net/netfilter/" + key); err != nil || strings.TrimSpace(string(value)) != original {
			t.Fatal("removal did not restore original observation setting", key, string(value), err)
		}
	}
	chains, err := (&nftables.Conn{}).ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatal(err)
	}
	for _, chain := range chains {
		if chain.Table.Name == "routerd2" && chain.Name == "flow_observation" {
			t.Fatal("removed observation chain is still present")
		}
	}
	c := connect("tcp4")
	probe(c)
	c.Close()
	f.stop()
	t.Logf("product enabled accounting and timestamps; removal restored accounting=0 timestamp=%s while new TCP forwarding remained usable", initialTimestamp)
}

const trafficFlowFixture = `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: traffic-flows}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: wan}
      spec: {ifname: wan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: lan}
      spec: {ifname: lan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: router-wan}
      spec: {interface: wan, address: 198.51.100.1/24}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: router-lan}
      spec: {interface: lan, address: 192.0.2.1/24}
    - apiVersion: system.routerd.net/v1alpha1
      kind: Sysctl
      metadata: {name: forwarding}
      spec: {key: net.ipv4.ip_forward, value: "1", runtime: true}
    - apiVersion: system.routerd.net/v1alpha1
      kind: Sysctl
      metadata: {name: connection-counters}
      spec: {key: net.netfilter.nf_conntrack_acct, value: "1", runtime: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: NAT44Rule
      metadata: {name: outgoing}
      spec:
        type: snat
        egressInterface: wan
        sourceRanges: [192.0.2.0/24]
        snatAddressFrom: {resource: IPv4StaticAddress/router-wan, field: address}
    - apiVersion: net.routerd.net/v1alpha1
      kind: TrafficFlowLog
      metadata: {name: connections}
      spec: {enabled: true, source: conntrack}
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogSink
      metadata: {name: archive}
      spec: {type: syslog, syslog: {network: udp, address: "198.51.100.2:5514", tag: traffic-flow-archive}}
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: flows}
      spec: {retention: 20s, schedule: daily, signals: [trafficFlows], sinks: [LogSink/archive]}
`
