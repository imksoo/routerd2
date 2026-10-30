//go:build linux && integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func TestDaemonTrafficFlowHistory(t *testing.T) {
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
	for key, value := range map[string]string{"nf_conntrack_acct": "0", "nf_conntrack_udp_timeout": "3", "nf_conntrack_udp_timeout_stream": "3"} {
		if err := os.WriteFile("/proc/sys/net/netfilter/"+key, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var udp, archive net.PacketConn
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
		tcp, err = net.Listen("tcp4", "198.51.100.2:18081")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	defer tcp.Close()
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
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
	imported, err := legacyconfig.Compile([]byte(trafficFlowFixture))
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	var historyPath string
	for _, res := range imported.Document.Resources {
		if res.Service != nil && res.Service.TrafficFlow != nil {
			cfg := res.Service.TrafficFlow
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
	f := startFixtureDaemonAt(t, imported.Document, work)
	if value, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_acct"); err != nil || strings.TrimSpace(string(value)) != "1" {
		t.Fatal("product did not enable connection accounting", string(value), err)
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
	var first logstore.TrafficFlow
	until(t, 6*time.Second, func() bool {
		probe(udpClient)
		probe(tcpClient)
		protocols := map[string]bool{}
		for _, row := range history() {
			if row.ClientAddress == "192.0.2.2" && row.PeerAddress == "198.51.100.2" && row.PeerPort == 18081 && row.NATTranslatedAddress == "198.51.100.1" && row.Accounting && row.BytesOut > 0 && row.BytesIn > 0 {
				protocols[row.Protocol] = true
				if row.Protocol == "udp" {
					first = row
				}
			}
		}
		return protocols["tcp"] && protocols["udp"]
	})
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
	t.Log("history recovered from a locked database; counters advanced without splitting the connection or blocking traffic")
	f = f.restart()
	probe(udpClient)
	until(t, 4*time.Second, func() bool {
		for _, row := range history() {
			if row.FlowKey == first.FlowKey && row.StartedAt.Equal(first.StartedAt) {
				return true
			}
		}
		return false
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
	disabled := imported.Document
	disabled.Resources = nil
	for _, res := range imported.Document.Resources {
		if res.Service == nil || res.Service.TrafficFlow == nil {
			if res.Forwarding != nil {
				forwarding := *res.Forwarding
				forwarding.ConntrackAccounting = nil
				res.Forwarding = &forwarding
			}
			disabled.Resources = append(disabled.Resources, res)
		}
	}
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-flow-history", Config: disabled}, nil); err != nil {
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
	if value, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_acct"); err != nil || strings.TrimSpace(string(value)) != "0" {
		t.Fatal("removal did not restore original accounting", string(value), err)
	}
	c := connect("tcp4")
	probe(c)
	c.Close()
	f.stop()
	t.Log("product enabled conntrack accounting; removal restored zero and stopped collection while new TCP forwarding remained usable")
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
