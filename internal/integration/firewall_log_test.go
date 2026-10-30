//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonFirewallPacketLogs(t *testing.T) {
	if !isolateFixture(t, "TestDaemonFirewallPacketLogs") {
		return
	}
	peer := fixturePeerNamespace(t, "wan0", "client0")
	_ = healthPBRCapture(t, "wan0")
	if err := fixtureInsideNamespace(peer, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		for _, cidr := range []string{"198.51.100.2/24", "198.51.100.3/32", "2001:db8:1::2/64"} {
			address, err := netlink.ParseAddr(cidr)
			if err != nil {
				return err
			}
			address.Flags = unix.IFA_F_NODAD // This independent peer has static fixture addresses.
			if err := netlink.AddrAdd(link, address); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var archive net.PacketConn
	if err := fixtureInsideNamespace(peer, func() error {
		var err error
		archive, err = net.ListenPacket("udp4", "198.51.100.3:5514")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var archiveMu sync.Mutex
	var archived []string
	archiveDone := make(chan struct{})
	go func() {
		defer close(archiveDone)
		var buffer [65536]byte
		for {
			n, _, err := archive.ReadFrom(buffer[:])
			if err != nil {
				return
			}
			archiveMu.Lock()
			archived = append(archived, string(buffer[:n]))
			archiveMu.Unlock()
		}
	}()
	defer func() { archive.Close(); <-archiveDone }()
	raw := []byte(firewallLogFixture)
	imported, err := legacyconfig.Compile(append(append([]byte{}, raw...), []byte(firewallHistoryPolicy)...))
	if err != nil {
		t.Fatal(err)
	}
	// NFLOG is family-independent. The IPv6 fixture address is explicit native
	// configuration; no DHCP/PD setup is needed to test packet logging.
	imported.Document.Resources = append(imported.Document.Resources, resourceprogram.Resource{ID: "address/router-v6", Address: &resourceprogram.Address{Interface: "wan0", CIDR: "2001:db8:1::1/64"}})
	work := t.TempDir()
	var historyPath string
	for _, resource := range imported.Document.Resources {
		if resource.Service != nil && resource.Service.FirewallLog != nil {
			cfg := resource.Service.FirewallLog
			if cfg.Retention == nil || cfg.Retention.MaxAgeSeconds != 8 || cfg.Retention.IntervalSeconds != 86400 || len(cfg.Retention.Syslog) != 1 {
				t.Fatal("missing imported firewall retention", cfg)
			}
			// Only shorten the daily maintenance schedule in this isolated
			// fixture. Packet timestamps and expiry use the actual clock.
			cfg.Retention.IntervalSeconds = 1
			historyPath = filepath.Join(work, "state", "services", resource.Service.Name, "firewall-events.db")
		}
	}
	if historyPath == "" {
		t.Fatal("missing firewall history owner")
	}
	f := startFixtureDaemonAt(t, imported.Document, work)
	history := func() []logstore.FirewallLogEntry {
		t.Helper()
		db, err := logstore.OpenFirewallLogReadOnly(historyPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.List(t.Context(), logstore.FirewallLogFilter{Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	echo, err := net.ListenPacket("udp", ":18081")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		var buffer [64]byte
		for {
			n, peer, err := echo.ReadFrom(buffer[:])
			if err != nil {
				return
			}
			if _, err := echo.WriteTo(buffer[:n], peer); err != nil {
				return
			}
		}
	}()
	logs := localapi.NewClient(filepath.Join(work, "run", logging.SocketName))
	type packetFields struct {
		Action          string `json:"action"`
		Rule            string `json:"rule"`
		Source          string `json:"source_address"`
		Destination     string `json:"destination_address"`
		DestinationPort int    `json:"destination_port"`
		Protocol        string `json:"protocol"`
		Family          string `json:"family"`
		InInterface     string `json:"in_interface"`
		PacketBytes     int    `json:"packet_bytes"`
		Group           int    `json:"group"`
	}
	type packetRecord struct {
		Sequence   uint64       `json:"sequence"`
		Module     string       `json:"module"`
		Message    string       `json:"message"`
		Attributes packetFields `json:"attributes"`
	}
	query := func() []packetRecord {
		t.Helper()
		var result struct {
			Records []packetRecord `json:"records"`
		}
		if err := logs.Do(context.Background(), "GET", "/v1/logs?limit=200", nil, &result); err != nil {
			t.Fatal(err)
		}
		return result.Records
	}
	probe := func(address string, port int) error {
		return fixtureInsideNamespace(peer, func() error {
			conn, err := net.DialTimeout("udp", net.JoinHostPort(address, fmt.Sprint(port)), time.Second)
			if err != nil {
				return err
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(400 * time.Millisecond)); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("firewall-log-peer")); err != nil {
				return err
			}
			var response [64]byte
			n, err := conn.Read(response[:])
			if err == nil && string(response[:n]) != "firewall-log-peer" {
				return errors.New("unexpected echo payload")
			}
			return err
		})
	}
	exercise := func(phase string) {
		t.Helper()
		// Worker readiness precedes Linux's normal DAD completion for the static
		// IPv6 fixture. Observe the address instead of disabling DAD or sleeping.
		until(t, 5*time.Second, func() bool {
			link, err := netlink.LinkByName("wan0")
			if err != nil {
				t.Fatal(err)
			}
			addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
			if err != nil {
				t.Fatal(err)
			}
			for _, address := range addresses {
				if address.IP.String() == "2001:db8:1::1" && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
					return true
				}
			}
			return false
		})
		var after uint64
		for _, record := range query() {
			after = max(after, record.Sequence)
		}
		for _, address := range []string{"198.51.100.1", "2001:db8:1::1"} {
			if err := probe(address, 18081); err != nil {
				t.Fatalf("%s accepted traffic %s: %v", phase, address, err)
			}
			var timeout net.Error
			if err := probe(address, 18080); !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("%s drop should time out: %v", phase, err)
			}
			if err := probe(address, 18082); !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatalf("%s reject should return ICMP port unreachable: %v", phase, err)
			}
		}
		until(t, 5*time.Second, func() bool {
			seen := make(map[string]bool)
			for _, record := range query() {
				p := record.Attributes
				if record.Sequence <= after {
					continue
				}
				if record.Message != "firewall packet" || !strings.HasPrefix(record.Module, "service.") || p.Group != 1 || p.InInterface != "wan0" || p.Protocol != "udp" || p.PacketBytes == 0 {
					continue
				}
				if p.Family == "ipv4" && (p.Source != "198.51.100.2" || p.Destination != "198.51.100.1") || p.Family == "ipv6" && (p.Source != "2001:db8:1::2" || p.Destination != "2001:db8:1::1") {
					t.Fatal("packet tuple changed before local recording", p)
				}
				if p.Action == "drop" && p.DestinationPort == 18080 || p.Action == "reject" && p.DestinationPort == 18082 {
					seen[p.Family+":"+p.Action] = true
				}
				if p.DestinationPort == 18081 {
					t.Fatal("acceptSampleRate zero unexpectedly logged accepted traffic")
				}
			}
			return seen["ipv4:drop"] && seen["ipv4:reject"] && seen["ipv6:drop"] && seen["ipv6:reject"]
		})
		for _, record := range query() {
			if record.Sequence > after && record.Message == "firewall packet" {
				t.Logf("%s log sequence=%d module=%s packet=%+v", phase, record.Sequence, record.Module, record.Attributes)
			}
		}
		t.Logf("%s: IPv4/IPv6 accepted UDP echoed, denied UDP timed out, rejected UDP received ICMP; both families reached the product log with drop/reject identity", phase)
	}
	exercise("initial")
	for _, address := range []string{"198.51.100.1", "2001:db8:1::1"} {
		err := fixtureInsideNamespace(peer, func() error {
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(address, "18083"), 400*time.Millisecond)
			if conn != nil {
				conn.Close()
			}
			return err
		})
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatal("denied TCP SYN should time out", err)
		}
	}
	until(t, 2*time.Second, func() bool {
		v4, v6 := false, false
		for _, row := range history() {
			if row.Protocol != "tcp" || row.DstPort != 18083 || row.TCPFlags != "SYN" {
				continue
			}
			v4 = v4 || row.L3Proto == "ipv4"
			v6 = v6 || row.L3Proto == "ipv6"
		}
		return v4 && v6
	})
	t.Log("actual IPv4/IPv6 TCP SYN flags retained in packet history")
	var original logstore.FirewallLogEntry
	until(t, 2*time.Second, func() bool {
		for _, row := range history() {
			if row.Action == "drop" && row.Protocol == "udp" && row.SrcAddress == "198.51.100.2" && row.DstPort == 18080 {
				original = row
				return true
			}
		}
		return false
	})
	f = f.restart()
	persisted := false
	for _, row := range history() {
		persisted = persisted || row.ID == original.ID && row.Timestamp.Equal(original.Timestamp)
	}
	if !persisted {
		t.Fatal("firewall history was not retained through product restart")
	}
	exercise("restart")
	// Isolate only the archive destination. A silent UDP receiver loss can
	// still return send success; this route makes the product's send fail.
	_, archiveSubnet, _ := net.ParseCIDR("198.51.100.3/32")
	blocked := netlink.Route{Dst: archiveSubnet, Type: unix.RTN_UNREACHABLE, Table: unix.RT_TABLE_MAIN}
	if err := netlink.RouteAdd(&blocked); err != nil {
		t.Fatal(err)
	}
	defer netlink.RouteDel(&blocked)
	until(t, 12*time.Second, func() bool {
		if err := probe("198.51.100.1", 18081); err != nil {
			t.Fatal("archive failure interrupted traffic", err)
		}
		if time.Since(original.Timestamp) < 8*time.Second {
			return false
		}
		for _, record := range query() {
			if record.Message == "firewall history retention failed" {
				return true
			}
		}
		return false
	})
	for attempt := 0; attempt < 2; attempt++ {
		exercise("archive-unreachable")
		preserved, newer := false, false
		for _, row := range history() {
			preserved = preserved || row.ID == original.ID && row.Timestamp.Equal(original.Timestamp)
			newer = newer || row.Timestamp.After(original.Timestamp.Add(time.Second))
		}
		if !preserved || !newer {
			t.Fatal("archive failure deleted old rows or prevented new history")
		}
	}
	t.Log("real archive send failure preserved expired history while both IP families kept allow/drop/reject and new history recording")
	if err := netlink.RouteDel(&blocked); err != nil {
		t.Fatal(err)
	}
	rounds := 0
	until(t, 12*time.Second, func() bool {
		rounds++
		for _, address := range []string{"198.51.100.1", "2001:db8:1::1"} {
			if err := probe(address, 18081); err != nil {
				t.Fatal("retention interrupted accepted traffic", err)
			}
			var timeout net.Error
			if err := probe(address, 18080); !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatal("retention changed drop", err)
			}
			if err := probe(address, 18082); !errors.Is(err, syscall.ECONNREFUSED) {
				t.Fatal("retention changed reject", err)
			}
		}
		old, fresh := false, false
		for _, row := range history() {
			old = old || row.ID == original.ID && row.Timestamp.Equal(original.Timestamp)
			fresh = fresh || row.Timestamp.After(original.Timestamp.Add(time.Second))
		}
		if old || !fresh {
			return false
		}
		archiveMu.Lock()
		defer archiveMu.Unlock()
		for _, message := range archived {
			_, body, ok := strings.Cut(message, "firewallEvents ")
			if !ok {
				continue
			}
			var row struct {
				ID              int64  `json:"id"`
				Timestamp       int64  `json:"ts"`
				Source          string `json:"src_address"`
				DestinationPort int    `json:"dst_port"`
				Action          string `json:"action"`
			}
			if err := json.NewDecoder(strings.NewReader(body)).Decode(&row); err != nil {
				t.Fatal("invalid archived firewall row", err)
			}
			if row.ID == original.ID && row.Timestamp == original.Timestamp.UnixNano() && row.Source == original.SrcAddress && row.DestinationPort == original.DstPort && row.Action == original.Action {
				t.Logf("independent UDP archive matched expired record: %s", message)
				return true
			}
		}
		return false
	})
	t.Logf("firewall history survived restart, expired at its original packet age, and reached independent UDP archive before deletion; newer rows retained; IPv4/IPv6 allow/drop/reject held for %d maintenance rounds", rounds)

	// Remove only logging through the same import boundary; packet verdicts stay.
	disabled, err := legacyconfig.Compile([]byte(strings.Replace(string(raw), "enabled: true", "enabled: false", 1)))
	if err != nil {
		t.Fatal(err)
	}
	disabled.Document.Resources = append(disabled.Document.Resources, imported.Document.Resources[len(imported.Document.Resources)-1])
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "remove-packet-log", Config: disabled.Document}, nil); err != nil {
		t.Fatal(err)
	}
	// A stopped collector releases its group; the kernel has no listener ownership
	// left after the service's normal shutdown.
	until(t, 5*time.Second, func() bool {
		var status struct {
			Modules []struct {
				Role  string `json:"role"`
				State string `json:"state"`
			} `json:"modules"`
		}
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Role == "firewall_log" {
				return false
			}
		}
		return true
	})
	var cursor uint64
	for _, record := range query() {
		cursor = max(cursor, record.Sequence)
	}
	for _, address := range []string{"198.51.100.1", "2001:db8:1::1"} {
		if err := probe(address, 18081); err != nil {
			t.Fatal("logging removal changed allowed traffic", err)
		}
		if err := probe(address, 18082); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatal("logging removal changed reject", err)
		}
	}
	for _, record := range query() {
		if record.Sequence > cursor && record.Message == "firewall packet" {
			t.Fatal("removed packet collector still records", record)
		}
	}
	f.stop()
	t.Log("logging removal stopped its worker without changing packet verdicts; complete product shutdown succeeded")
}

const firewallLogFixture = `apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: firewall-events}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: wan}
      spec: {ifname: wan0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: router}
      spec: {interface: wan, address: 198.51.100.1/24}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: outside}
      spec: {role: untrust, interfaces: [Interface/wan]}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallPolicy
      metadata: {name: policy}
      spec: {logDeny: true, sameRoleAccept: true}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: allow-echo}
      spec: {fromZone: outside, toZone: self, protocol: udp, port: 18081, action: accept}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallRule
      metadata: {name: reject-probe}
      spec: {fromZone: outside, toZone: self, protocol: udp, port: 18082, action: reject, log: true}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallEventLog
      metadata: {name: packets}
      spec:
        enabled: true
        path: /var/log/routerd/firewall-events.db
        nflogGroup: 1
        log: {acceptSampleRate: 0, dropSampleRate: 1, copyRange: 2048, reject: true}
`

const firewallHistoryPolicy = `
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogSink
      metadata: {name: archive}
      spec:
        type: syslog
        minLevel: info
        syslog: {network: udp, address: "198.51.100.3:5514", facility: local0, tag: firewall-archive}
    - apiVersion: system.routerd.net/v1alpha1
      kind: LogRetention
      metadata: {name: packets}
      spec: {retention: 8s, schedule: daily, vacuum: true, signals: [firewallEvents], sinks: [LogSink/archive]}
`
