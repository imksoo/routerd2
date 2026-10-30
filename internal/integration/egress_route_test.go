//go:build linux && integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonEgressRouteHealthyMultipath(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonEgressRouteHealthyMultipath", 100*time.Second) {
		return
	}
	a := healthPBRPeer(t, "wan0", "provider0", "192.0.2.1/24", "192.0.2.2")
	b := healthPBRPeer(t, "wan1", "provider1", "203.0.113.1/24", "203.0.113.2")
	applications := []string{"198.51.100.20", "198.51.100.21", "198.51.100.22"}
	for _, ns := range []netns.NsHandle{a, b} {
		if err := fixtureInsideNamespace(ns, func() error {
			lo, err := netlink.LinkByName("lo")
			if err != nil {
				return err
			}
			for _, address := range applications[1:] {
				prefix, _ := netlink.ParseAddr(address + "/32")
				if err := netlink.AddrAdd(lo, prefix); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	readHash := func() [2]string {
		t.Helper()
		var result [2]string
		for i, name := range []string{"fib_multipath_hash_fields", "fib_multipath_hash_policy"} {
			b, err := os.ReadFile("/proc/sys/net/ipv4/" + name)
			if err != nil {
				t.Fatal(err)
			}
			result[i] = string(b)
		}
		return result
	}
	originalHash := readHash()
	lan := fixturePeerNamespace(t, "lan0", "client0")
	h, err := netlink.NewHandleAt(lan, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	client, err := h.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	for host := 2; host < 66; host++ {
		address := &netlink.Addr{IPNet: &net.IPNet{IP: net.IPv4(198, 18, 0, byte(host)), Mask: net.CIDRMask(24, 32)}}
		if err := h.AddrAdd(client, address); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: client.Attrs().Index, Gw: net.ParseIP("198.18.0.1")}); err != nil {
		t.Fatal(err)
	}
	probeA := healthPBRListener(t, a, "198.51.100.10:18080", "A")
	healthPBRListener(t, b, "198.51.100.11:18080", "B")
	vrrpVIPResponders(t, a, "4", applications, "A")
	vrrpVIPResponders(t, b, "4", applications, "B")
	capA, capB := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "wan1")
	program, healthA, healthB := importedHealthPBRProgram(t)
	var resources []resourceprogram.Resource
	for _, r := range program.Resources {
		if r.Forwarding != nil {
			r.Forwarding.IPv4ECMPHash = "source"
		}
		if r.ID != "primary" && r.ID != "backup" && r.ID != "conditional-address" {
			resources = append(resources, r)
		}
	}
	group := resourceprogram.EgressRoute{Destination: "0.0.0.0/0", Table: 100, Metric: 100,
		Candidates: []resourceprogram.EgressCandidate{
			{Preference: 120, When: &resourceprogram.FactCondition{Fact: healthA + ".active", MaxAge: "2500ms", For: "300ms"}, NextHop: network.NextHop{Interface: "wan0", Gateway: "192.0.2.1", Weight: 1}},
			{Preference: 120, When: &resourceprogram.FactCondition{Fact: healthB + ".active", MaxAge: "2500ms"}, NextHop: network.NextHop{Interface: "wan1", Gateway: "203.0.113.1", Weight: 2}},
		}}
	program.Resources = append(resources, resourceprogram.Resource{ID: "egress", EgressRoute: &group})
	f := startFixtureDaemon(t, program)
	if current := readHash(); current != [2]string{"1\n", "3\n"} {
		t.Fatal("source hash was not applied", current)
	}
	waitPaths := func(want int) {
		t.Helper()
		until(t, 8*time.Second, func() bool {
			routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
			if err != nil {
				return false
			}
			if len(routes) != 1 {
				return false
			}
			if want == 2 {
				weights := make(map[string]int)
				for _, hop := range routes[0].MultiPath {
					link, err := netlink.LinkByIndex(hop.LinkIndex)
					if err != nil {
						return false
					}
					weights[link.Attrs().Name] = hop.Hops + 1
				}
				return len(weights) == 2 && weights["wan0"] == 1 && weights["wan1"] == 2
			}
			return len(routes[0].MultiPath) == 0 && routes[0].Gw.Equal(net.ParseIP("203.0.113.1"))
		})
	}
	traffic := func(want string) {
		t.Helper()
		paths := make(map[int]string)
		for _, protocol := range []string{"tcp4", "udp4"} {
			seen := map[string]bool{}
			for host := 2; host < 66; host++ {
				for _, application := range applications {
					source := net.IPv4(198, 18, 0, byte(host))
					dialer := net.Dialer{Timeout: time.Second, LocalAddr: &net.TCPAddr{IP: source}}
					if protocol == "udp4" {
						dialer.LocalAddr = &net.UDPAddr{IP: source}
					}
					if err := fixtureInsideNamespace(lan, func() error {
						port := "18093"
						if protocol == "udp4" {
							port = "18094"
						}
						conn, err := dialer.Dial(protocol, net.JoinHostPort(application, port))
						if err != nil {
							return err
						}
						defer conn.Close()
						if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
							return err
						}
						if protocol == "udp4" {
							if _, err := conn.Write([]byte("egress")); err != nil {
								return err
							}
						}
						var response [1]byte
						if _, err := io.ReadFull(conn, response[:]); err != nil {
							return err
						}
						if previous := paths[host]; previous != "" && previous != string(response[:]) {
							t.Fatal("one source used different WANs across destinations/transports", protocol, source, previous, string(response[:]))
						}
						paths[host] = string(response[:])
						seen[string(response[:])] = true
						return nil
					}); err != nil {
						t.Fatal(protocol, source, err)
					}
				}
				if want == "B" || seen["A"] && seen["B"] {
					break
				}
			}
			if want == "both" && (!seen["A"] || !seen["B"]) || want == "B" && (len(seen) != 1 || !seen["B"]) {
				t.Fatal("actual egress paths differ", protocol, want, seen)
			}
		}
	}
	waitPaths(2)
	traffic("both")
	probeA.Close()
	waitPaths(1)
	traffic("B")
	healthPBRListener(t, a, "198.51.100.10:18080", "A")
	waitPaths(2)
	traffic("both")
	trial := program
	trial.Resources = append([]resourceprogram.Resource(nil), program.Resources...)
	changed := group
	changed.Candidates = append([]resourceprogram.EgressCandidate(nil), group.Candidates...)
	changed.Candidates[1].Preference = 150
	trial.Resources[len(trial.Resources)-1].EgressRoute = &changed
	var state control.State
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "egress-preference", Config: trial, ConfirmTimeoutSeconds: 5}, &state); err != nil {
		t.Fatal(err)
	}
	waitPaths(1)
	traffic("B")
	until(t, 10*time.Second, func() bool {
		state = control.State{}
		return f.control.Do(context.Background(), "GET", "/v1/config", nil, &state) == nil && state.Revision == 3 && state.Confirmation == nil
	})
	waitPaths(2)
	traffic("both")
	// Removing the setting restores the real pre-product namespace values;
	// confirmed rollback then reinstalls source hashing with the same route.
	withoutHash := program
	withoutHash.Resources = append([]resourceprogram.Resource(nil), program.Resources...)
	for i := range withoutHash.Resources {
		if setting := withoutHash.Resources[i].Forwarding; setting != nil {
			changed := *setting
			changed.IPv4ECMPHash = ""
			withoutHash.Resources[i].Forwarding = &changed
		}
	}
	if err := f.control.Do(context.Background(), "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 3, RequestID: "egress-hash-restore", Config: withoutHash, ConfirmTimeoutSeconds: 5}, &state); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool { return readHash() == originalHash })
	until(t, 10*time.Second, func() bool {
		state = control.State{}
		return f.control.Do(context.Background(), "GET", "/v1/config", nil, &state) == nil && state.Revision == 5 && state.Confirmation == nil
	})
	if current := readHash(); current != [2]string{"1\n", "3\n"} {
		t.Fatal("rollback did not restore source hashing", current)
	}
	waitPaths(2)
	traffic("both")
	// Freeze observation owners and inspect the actual writer's accepted
	// route. Its original authority must survive writer replacement unchanged.
	var status supervisor.Status
	if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	var frozen []supervisor.ModuleStatus
	var writer supervisor.ModuleStatus
	for _, m := range status.Modules {
		if m.Name == "network" {
			writer = m
		}
		if m.Name == "control" || m.Name == healthA || m.Name == healthB {
			if err := syscall.Kill(m.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			frozen = append(frozen, m)
			t.Cleanup(func() { _ = syscall.Kill(m.PID, syscall.SIGCONT) })
		}
	}
	var page network.DesiredPage
	if err := f.network.Do(context.Background(), "GET", "/v1/observations/desired", nil, &page); err != nil || page.NextOffset != nil {
		t.Fatal("inspect frozen writer projection", err)
	}
	var desired network.Desired
	if err := json.Unmarshal(page.Data, &desired); err != nil {
		t.Fatal(err)
	}
	var deadline uint64
	for _, route := range desired.Routes {
		if route.Table == 100 {
			deadline = route.ExpiresNanos
		}
	}
	clock, err := boottime.Read()
	if err != nil || deadline <= uint64(clock.BoottimeNanoseconds) || len(frozen) != 3 {
		t.Fatal("missing live multipath authority or actual producers", deadline, frozen, err)
	}
	if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, time.Duration(deadline-uint64(clock.BoottimeNanoseconds))+time.Second, func() bool {
		routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
		return err == nil && len(routes) == 0
	})
	status = supervisor.Status{}
	if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	currentModules := make(map[string]supervisor.ModuleStatus)
	for _, current := range status.Modules {
		currentModules[current.Name] = current
	}
	if current := currentModules["network"]; current.PID == writer.PID || current.Generation <= writer.Generation || !current.Ready {
		t.Fatal("route withdrawal did not use a recovered writer", writer, current)
	}
	for _, before := range frozen {
		if current := currentModules[before.Name]; current.PID != before.PID || current.Generation != before.Generation || !current.Ready {
			t.Fatal("producer replacement masked original route expiry", before, current)
		}
		if err := syscall.Kill(before.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	waitPaths(2)
	traffic("both")
	for _, capture := range []*healthPBRCaptureState{capA, capB} {
		for _, protocol := range []byte{6, 17} {
			port := uint16(18093)
			if protocol == 17 {
				port = 18094
			}
			packets := 0
			for host := 2; host < 66; host++ {
				packets += capture.count(net.IPv4(198, 18, 0, byte(host)).String(), "198.51.100.20", protocol, port)
			}
			if packets == 0 {
				t.Fatal("WAN capture lacks application traffic", protocol)
			}
		}
	}
	t.Log("real two-WAN weighted ECMP, health removal/recovery, preference/rollback and original expiry across writer replacement passed new TCP/UDP exchanges")
}
