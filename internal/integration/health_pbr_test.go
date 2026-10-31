//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

//go:embed testdata/health-import.yaml
var legacyHealth []byte

//go:embed testdata/health-import-policy.yaml
var legacyHealthPolicy []byte

// The router, LAN and two providers have independent network namespaces. Both
// providers offer the same application address and probe address; a probe that
// escapes its selected path would therefore hide the injected WAN-A outage.
func TestDaemonHealthPBRFailoverAndRecovery(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonHealthPBRFailoverAndRecovery", 180*time.Second) {
		return
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ctx := context.Background()
	a := healthPBRPeer(t, "wan0", "provider0", "192.0.2.1/24", "192.0.2.2")
	b := healthPBRPeer(t, "wan1", "provider1", "203.0.113.1/24", "203.0.113.2")
	lan := fixturePeerNamespace(t, "lan0", "client0")
	h, err := netlink.NewHandleAt(lan, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	client, _ := h.LinkByName("client0")
	addr, _ := netlink.ParseAddr("198.18.0.2/24")
	if err := h.AddrAdd(client, addr); err != nil {
		t.Fatal(err)
	}
	if err := h.RouteAdd(&netlink.Route{LinkIndex: client.Attrs().Index, Gw: net.ParseIP("198.18.0.1")}); err != nil {
		t.Fatal(err)
	}
	probeA := healthPBRListener(t, a, "198.51.100.10:18080", "A")
	_ = healthPBRListener(t, b, "198.51.100.10:18080", "decoy-B")
	_ = healthPBRListener(t, b, "198.51.100.11:18080", "B")
	healthPBRListener(t, a, "198.51.100.20:18081", "A")
	healthPBRListener(t, b, "198.51.100.20:18081", "B")
	healthPBRUDP(t, a, "A")
	healthPBRUDP(t, b, "B")
	assertMarks := healthPBRMarkCounters(t)

	// Deliberately use the worker's route protocol for an unowned object.
	wan, _ := netlink.LinkByName("wan0")
	if err := netlink.LinkSetUp(wan); err != nil {
		t.Fatal(err)
	}
	foreignAddr, _ := netlink.ParseAddr("192.0.2.99/24")
	if err := netlink.AddrAdd(wan, foreignAddr); err != nil {
		t.Fatal(err)
	}
	_, foreignPrefix, _ := net.ParseCIDR("198.19.77.0/24")
	if err := netlink.RouteAdd(&netlink.Route{Dst: foreignPrefix, LinkIndex: wan.Attrs().Index, Gw: net.ParseIP("192.0.2.1"), Protocol: network.RouteProtocol, Table: 254, Priority: 201}); err != nil {
		t.Fatal(err)
	}
	foreignRule := netlink.NewRule()
	foreignRule.Family, foreignRule.Priority, foreignRule.Table = unix.AF_INET, 9000, 177
	if err := netlink.RuleAdd(foreignRule); err != nil {
		t.Fatal(err)
	}
	program, healthA, healthB := importedHealthPBRProgram(t)
	f := startFixtureDaemon(t, program)
	routerNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer routerNS.Close()
	vrrpVIPResponders(t, routerNS, "4", []string{"192.0.2.42"}, "R")
	capA, capB := healthPBRCapture(t, "wan0"), healthPBRCapture(t, "wan1")
	t.Cleanup(func() {
		if t.Failed() {
			routes, _ := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 101}, netlink.RT_FILTER_TABLE)
			rules, _ := netlink.RuleList(unix.AF_INET)
			t.Logf("probe routes: %+v rules: %+v", routes, rules)
			route, lookupErr := network.LookupRoute(network.RouteLookupRequest{Target: "198.51.100.10", Interface: "wan0", Source: "192.0.2.2", Mark: 42})
			t.Logf("probe lookup: %+v %v", route, lookupErr)
			for _, name := range []string{"wan0", "wan1"} {
				l, _ := netlink.LinkByName(name)
				t.Logf("%s link stats: %+v", name, l.Attrs().Statistics)
			}
		}
	})
	read := func() runtimeplan.Status {
		t.Helper()
		var s runtimeplan.Status
		if err := f.control.Do(ctx, "GET", "/v1/services", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	instance := func(name string) runtimeplan.InstanceStatus {
		t.Helper()
		var i runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+name, nil, &i); err != nil {
			t.Fatal(err)
		}
		return i
	}
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var s supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &s); err != nil {
			t.Fatal(err)
		}
		for _, m := range s.Modules {
			if m.Name == name {
				return m
			}
		}
		return supervisor.ModuleStatus{}
	}
	record := func(stage string) {
		t.Helper()
		if dir := os.Getenv("ROUTERD2_TEST_EVIDENCE_DIR"); dir != "" {
			routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.MarshalIndent(struct {
				Stage   string                       `json:"stage"`
				Runtime runtimeplan.Status           `json:"runtime"`
				Health  []runtimeplan.InstanceStatus `json:"health"`
				Routes  []netlink.Route              `json:"routes"`
			}{stage, read(), []runtimeplan.InstanceStatus{instance(healthA), instance(healthB)}, routes}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stage+".json"), append(b, '\n'), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	primary := func() bool { return healthPBRPrimary(t) }
	inactiveReason := func(want string) bool {
		t.Helper()
		var page runtimeplan.ResourceObservationPage
		if err := f.control.Do(ctx, "GET", "/v1/services/resources", nil, &page); err != nil {
			t.Fatal(err)
		}
		for _, resource := range page.Resources {
			if resource.ID == "primary" {
				return !resource.State.Active && resource.State.Reason == want
			}
		}
		return false
	}
	waitPrimary := func(want bool) {
		t.Helper()
		until(t, 12*time.Second, func() bool { return primary() == want })
	}
	assertPreserved := func() {
		t.Helper()
		if !resourceProgramAddress(t, wan, "192.0.2.99/24") || !resourceProgramRoute(t, "198.19.77.0/24", "192.0.2.1", wan.Attrs().Index, 201) {
			t.Fatal("foreign address/route removed")
		}
		rules, err := netlink.RuleList(unix.AF_INET)
		if err != nil {
			t.Fatal(err)
		}
		found := map[int]bool{}
		for _, r := range rules {
			if r.Priority == 1000 && r.Table == 100 || r.Priority == 1100 && r.Table == 101 || r.Priority == 9000 && r.Table == 177 {
				found[r.Priority] = true
			}
		}
		if len(found) != 3 {
			t.Fatal("application, probe or foreign rule lost", rules)
		}
		for _, target := range []struct{ address, iface, source string }{{"198.51.100.10", "wan0", "192.0.2.2"}, {"198.51.100.11", "wan1", "203.0.113.2"}} {
			route, err := network.LookupRoute(network.RouteLookupRequest{Target: target.address, Interface: target.iface, Source: target.source, Mark: 42})
			if err != nil || route.Interface != target.iface || route.Table != 101 {
				t.Fatalf("marked probe host route lost: %+v %v", route, err)
			}
		}
		until(t, 5*time.Second, func() bool {
			var resources runtimeplan.ResourceObservationPage
			if err := f.control.Do(ctx, "GET", "/v1/services/resources", nil, &resources); err != nil {
				t.Fatal(err)
			}
			if resources.Phase != "ready" {
				return false
			}
			routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, resource := range resources.Resources {
				gateway := ""
				switch resource.ID {
				case "primary":
					gateway = "192.0.2.1"
				case "backup":
					gateway = "203.0.113.1"
				default:
					continue
				}
				present := false
				for _, route := range routes {
					present = present || route.Gw.Equal(net.ParseIP(gateway))
				}
				if resource.State.Active != present {
					return false
				}
				checked++
			}
			if checked != 2 || resources.Total != len(program.Resources) {
				t.Fatal("resource status omitted declared owners", resources)
			}
			return true
		})
	}
	traffic := func(want string) {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			var conn net.Conn
			err := fixtureInsideNamespace(lan, func() error {
				var err error
				conn, err = net.DialTimeout(protocol, "198.51.100.20:18081", 2*time.Second)
				return err
			})
			if err != nil {
				t.Fatal(protocol, err)
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, err = conn.Write([]byte("path?"))
			var response [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, response[:])
			}
			conn.Close()
			if err != nil || string(response[:]) != want {
				t.Fatalf("%s path got %q, want %q: %v", protocol, response, want, err)
			}
		}
		assertPreserved()
	}
	until(t, 12*time.Second, func() bool {
		var a, b health.Snapshot
		return json.Unmarshal(instance(healthA).Snapshot, &a) == nil && json.Unmarshal(instance(healthB).Snapshot, &b) == nil && a.State.Phase == "Healthy" && b.State.Phase == "Healthy" && primary()
	})
	traffic("A")
	record("initial-primary")
	initial := read()
	if initial.Revision != 1 || initial.ProjectionGeneration == 0 {
		t.Fatal("missing independent operator/projection generations", initial)
	}
	old := healthPBRUpdate(instance(healthA))
	probeA.Close()
	// A consumer freshness deadline can withdraw before the producer's third
	// failed probe, especially under TCG scheduling. Wait for both observations
	// to prove threshold failure rather than assuming simultaneous publication.
	until(t, 12*time.Second, func() bool {
		var sample health.Snapshot
		return json.Unmarshal(instance(healthA).Snapshot, &sample) == nil && sample.State.Phase == "Unhealthy" && !sample.Active && !primary()
	})
	traffic("B")
	record("wan-a-failed")
	var down health.Snapshot
	i := instance(healthA)
	if err := json.Unmarshal(i.Snapshot, &down); err != nil || down.Active || down.State.Phase != "Unhealthy" {
		t.Fatalf("status disagrees with real fallback: %+v %v", down, err)
	}
	var upB health.Snapshot
	_ = json.Unmarshal(instance(healthB).Snapshot, &upB)
	current := read()
	if !upB.Active || current.Revision != 1 || current.ProjectionGeneration <= initial.ProjectionGeneration {
		t.Fatalf("independent WAN health or projection generation incorrect: b=%+v initial=%+v current=%+v", upB, initial, current)
	}
	if err := f.control.Do(ctx, "POST", "/v1/services/state", old, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("delayed old result was not fenced", err)
	}
	probeA = healthPBRListener(t, a, "198.51.100.10:18080", "A")
	until(t, 6*time.Second, func() bool {
		var page runtimeplan.ResourceObservationPage
		if f.control.Do(ctx, "GET", "/v1/services/resources", nil, &page) != nil {
			return false
		}
		for _, resource := range page.Resources {
			if resource.ID == "primary" {
				return resource.State.Reason == "when_hold" && !resource.State.Active
			}
		}
		return false
	})
	traffic("B")
	record("recovery-held")
	waitPrimary(true)
	traffic("A")
	record("wan-a-recovered")
	t.Log("two independent health workers: primary -> outage -> backup -> threshold recovery -> consumer hold -> primary; operator revision stays 1")

	for _, name := range []string{healthA, "control", "network"} {
		victim := module(name)
		if victim.PID <= 0 || !victim.Ready {
			t.Fatal("missing real worker", name, victim)
		}
		stale := healthPBRUpdate(instance(healthA))
		if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 15*time.Second, func() bool {
			m := module(name)
			return m.Ready && m.PID != victim.PID && m.Generation > victim.Generation
		})
		until(t, 12*time.Second, func() bool {
			// Ready can precede the replacement producer's first healthy
			// sample. A retained old snapshot/route cannot prove recovery.
			producer := module(healthA)
			var i runtimeplan.InstanceStatus
			var sample health.Snapshot
			if !producer.Ready || f.control.Do(ctx, "GET", "/v1/services/"+healthA, nil, &i) != nil || i.Generation != producer.Generation || i.ConfigDigest != producer.ConfigDigest || json.Unmarshal(i.Snapshot, &sample) != nil || !sample.Active || sample.State.Phase != "Healthy" {
				return false
			}
			var s runtimeplan.Status
			return f.control.Do(ctx, "GET", "/v1/services", nil, &s) == nil && s.Phase == "ready" && primary()
		})
		if name == healthA || name == "control" {
			if err := f.control.Do(ctx, "POST", "/v1/services/state", stale, nil); !localapi.IsHTTPStatus(err, 409) {
				t.Fatal("previous worker generation accepted", err)
			}
		}
		traffic("A")
		record("restart-" + name)
		t.Log("recovered actual worker", name)
	}

	// Fault injection shortens a real accepted observation's lease through the
	// real control socket. The producer remains stopped and supervisor-ready;
	// CLOCK_BOOTTIME expiry, rather than heartbeat death, must withdraw PBR.
	victim := module(healthA)
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	expiryPID := victim.PID
	t.Cleanup(func() { _ = syscall.Kill(expiryPID, syscall.SIGCONT) })
	expiring := healthPBRUpdate(instance(healthA))
	var observation health.Snapshot
	if err := json.Unmarshal(expiring.Snapshot, &observation); err != nil {
		t.Fatal(err)
	}
	clock, err := bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	observation.ObservedNanos = uint64(clock.BoottimeNanoseconds)
	observation.ExpiresNanos = observation.ObservedNanos + uint64(1500*time.Millisecond)
	expiring.Sequence++
	expiring.Snapshot, _ = json.Marshal(observation)
	if err := f.control.Do(ctx, "POST", "/v1/services/state", expiring, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 12*time.Second, func() bool {
		clock, err := bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		return uint64(clock.BoottimeNanoseconds) >= observation.ExpiresNanos && !primary() && inactiveReason("when_indeterminate")
	})
	if m := module(healthA); !m.Ready || m.PID != victim.PID || m.Generation != victim.Generation {
		t.Fatal("heartbeat loss masked finite fact expiry", m)
	}
	if err := f.control.Do(ctx, "POST", "/v1/services/state", expiring, nil); err != nil {
		t.Fatal("identical duplicate not acknowledged", err)
	}
	traffic("B")
	record("fact-expired")
	if primary() {
		t.Fatal("duplicate renewed expired fact")
	}
	expiring.Sequence++
	if err := f.control.Do(ctx, "POST", "/v1/services/state", expiring, nil); !localapi.IsHTTPStatus(err, 422) {
		t.Fatal("expired observation accepted under a new sequence", err)
	}
	if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitPrimary(true)
	traffic("A")
	t.Log("finite observation expired while producer stayed supervisor-ready; duplicate and resequenced replay did not renew it")

	// A real healthy observation still has its two-minute producer lease, but
	// this consumer requires a sample no older than 2.5 seconds. Keep the worker
	// supervisor-ready so only max_age can cause this independent withdrawal.
	victim = module(healthA)
	if err := syscall.Kill(victim.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	stoppedPID := victim.PID
	t.Cleanup(func() { _ = syscall.Kill(stoppedPID, syscall.SIGCONT) })
	stale := healthPBRUpdate(instance(healthA))
	if err := json.Unmarshal(stale.Snapshot, &observation); err != nil || !observation.Active {
		t.Fatal("missing real healthy sample", err)
	}
	until(t, 12*time.Second, func() bool {
		clock, err := bfd.ReadClock()
		if err != nil {
			t.Fatal(err)
		}
		return uint64(clock.BoottimeNanoseconds) > observation.ObservedNanos+uint64(2500*time.Millisecond) && !primary() && inactiveReason("when_indeterminate")
	})
	clock, err = bfd.ReadClock()
	if err != nil || uint64(clock.BoottimeNanoseconds) >= observation.ExpiresNanos || uint64(clock.BoottimeNanoseconds)-observation.ObservedNanos <= uint64(2500*time.Millisecond) {
		t.Fatal("max_age withdrawal was not independent of the producer lease", observation, clock, err)
	}
	if m := module(healthA); !m.Ready || m.PID != victim.PID || m.Generation != victim.Generation {
		t.Fatal("heartbeat loss masked max_age", m)
	}
	stale.Sequence++
	if err := f.control.Do(ctx, "POST", "/v1/services/state", stale, nil); err != nil {
		t.Fatal("valid producer lease should be accepted without renewing sample age", err)
	}
	// Observe at least two monitor ticks after acceptance, rather than just
	// checking the forwarding state left over from before the replay.
	time.Sleep(500 * time.Millisecond)
	traffic("B")
	record("consumer-max-age")
	if primary() {
		t.Fatal("resequenced replay renewed consumer sample age")
	}
	if err := syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitPrimary(true)
	traffic("A")
	t.Log("consumer max_age withdrew a still-valid healthy sample; transport replay stayed stale and fresh worker observation restored TCP/UDP")

	// A confirmed trial deliberately swaps priority. Timeout must restore both
	// the durable source and the actual TCP/UDP path without operator repair.
	trial := resourceProgramClone(t, program)
	resourceProgramResource(t, &trial, "primary").Route.Metric = 300
	var state control.State
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: 1, RequestID: "health-pbr-trial", Config: trial, ConfirmTimeoutSeconds: 5}, &state); err != nil {
		t.Fatal(err)
	}
	until(t, 4*time.Second, func() bool {
		routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		a, b := false, false
		for _, r := range routes {
			a = a || r.Gw.Equal(net.ParseIP("192.0.2.1")) && r.Priority == 300
			b = b || r.Gw.Equal(net.ParseIP("203.0.113.1")) && r.Priority == 200
		}
		return a && b
	})
	traffic("B")
	record("confirmed-trial")
	until(t, 12*time.Second, func() bool {
		state = control.State{}
		return f.control.Do(ctx, "GET", "/v1/config", nil, &state) == nil && state.Revision == 3 && state.Confirmation == nil && state.Healthy
	})
	waitPrimary(true)
	traffic("A")
	record("confirmed-rollback")
	want, _ := json.Marshal(program)
	got, _ := json.Marshal(state.Config)
	if !bytes.Equal(want, got) || read().Revision != 3 {
		t.Fatal("rollback did not restore authoritative source")
	}
	var hstatus control.Health
	until(t, 5*time.Second, func() bool {
		hstatus = control.Health{}
		return f.control.Do(ctx, "GET", "/v1/health", nil, &hstatus) == nil && hstatus.Healthy && hstatus.Services != nil && hstatus.Services.Revision == 3 && hstatus.Services.ProjectionGeneration == read().ProjectionGeneration
	})
	// A completely new supervisor legitimately starts generation 1 again.
	// The former tree's larger counters must not overwrite its first reports.
	oldTree := healthPBRUpdate(instance(healthA))
	f = f.restart()
	waitPrimary(true)
	fresh := healthPBRUpdate(instance(healthA))
	var tree supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &tree); err != nil {
		t.Fatal(err)
	}
	if !serviceconfig.ValidSupervisorInstance(fresh.SupervisorInstance) || fresh.SupervisorInstance == oldTree.SupervisorInstance || tree.InstanceID != fresh.SupervisorInstance || fresh.Generation >= oldTree.Generation {
		t.Fatal("fixture did not replace the supervisor authority and reuse lower generation counters")
	}
	if err := f.control.Do(ctx, "POST", "/v1/services/state", oldTree, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("previous supervisor notification was accepted", err)
	}
	if read().Revision != 3 {
		t.Fatal("complete supervisor restart lost the committed rollback revision")
	}
	traffic("A")
	record("restart-supervisor")
	t.Log("complete supervisor restart reused lower worker generations; new process-tree instance fenced delayed former-tree observations and preserved revision 3 and TCP/UDP")
	identityTraffic := func() {
		t.Helper()
		for protocol, port := range map[string]string{"tcp4": "18093", "udp4": "18094"} {
			var conn net.Conn
			if err := fixtureInsideNamespace(a, func() error {
				var err error
				conn, err = net.DialTimeout(protocol, net.JoinHostPort("192.0.2.42", port), time.Second)
				return err
			}); err != nil {
				t.Fatal("conditional address", protocol, err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, err := conn.Write([]byte("identity?"))
			var response [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, response[:])
			}
			conn.Close()
			if err != nil || string(response[:]) != "R" {
				t.Fatal("conditional address did not serve TCP/UDP", protocol, response, err)
			}
		}
	}
	identityTraffic()
	var frozen []supervisor.ModuleStatus
	for _, name := range []string{healthA, healthB} {
		process := module(name)
		if process.PID <= 0 {
			t.Fatal("missing final probe worker", name)
		}
		if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(process.PID, syscall.SIGCONT) })
		frozen = append(frozen, process)
	}
	var original, backup health.Snapshot
	if err := json.Unmarshal(instance(healthA).Snapshot, &original); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(instance(healthB).Snapshot, &backup); err != nil {
		t.Fatal(err)
	}
	controller, writer := module("control"), module("network")
	if err := syscall.Kill(controller.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(controller.PID, syscall.SIGCONT) })
	frozen = append(frozen, controller)
	var desiredPage network.DesiredPage
	if err := f.network.Do(ctx, "GET", "/v1/observations/desired", nil, &desiredPage); err != nil || desiredPage.NextOffset != nil {
		t.Fatal("cannot inspect frozen writer projection", err)
	}
	var desired network.Desired
	if err := json.Unmarshal(desiredPage.Data, &desired); err != nil {
		t.Fatal(err)
	}
	checked := 0
	var addressUntil, routeUntil uint64
	for _, iface := range desired.Interfaces {
		if iface.Name == "wan0" {
			deadline := iface.AddressOptions["192.0.2.42/32"].BootLifetime
			addressUntil = deadline.ValidNanos
			if deadline.BootID != original.BootID || deadline.ValidNanos == 0 || deadline.ValidNanos > original.ObservedNanos+uint64(2500*time.Millisecond)+1 {
				t.Fatal("address did not retain original consumer authority", deadline)
			}
			checked++
		}
	}
	for _, route := range desired.Routes {
		if route.Table == 100 {
			sample := original
			if route.Metric == 200 {
				sample = backup
			}
			routeUntil = max(routeUntil, route.ExpiresNanos)
			if route.BootID != sample.BootID || route.ExpiresNanos == 0 || route.ExpiresNanos > sample.ObservedNanos+uint64(2500*time.Millisecond)+1 {
				t.Fatal("PBR route did not retain original consumer authority", route)
			}
			checked++
		}
	}
	if checked != 3 || len(desired.Links) != 0 {
		t.Fatal("fixture did not use independent conditional address and route on a physical link", desired)
	}
	clock, err = bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("writer replacement original authority: now=%d addressUntil=%d routeUntil=%d addressRemaining=%s routeRemaining=%s", clock.BoottimeNanoseconds, addressUntil, routeUntil, time.Duration(int64(addressUntil)-clock.BoottimeNanoseconds), time.Duration(int64(routeUntil)-clock.BoottimeNanoseconds))
	if err := syscall.Kill(writer.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 2*time.Second, func() bool {
		current := module("network")
		return current.Ready && current.Generation > writer.Generation
	})
	clock, err = bfd.ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	// The original grant may still be live after a fast writer replacement.
	// Keep the existing observation budget after that actual deadline.
	remaining := max(time.Duration(int64(max(addressUntil, routeUntil))-clock.BoottimeNanoseconds), 0)
	until(t, remaining+2*time.Second, func() bool {
		routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		return len(routes) == 0 && !resourceProgramAddress(t, wan, "192.0.2.42/32")
	})
	for _, before := range frozen {
		if current := module(before.Name); current.PID != before.PID || current.Generation != before.Generation || !current.Ready {
			t.Fatal("worker restart masked original authority expiry", before, current)
		}
	}
	if current, err := netlink.LinkByName("wan0"); err != nil || current.Attrs().Flags&net.FlagUp == 0 || !resourceProgramAddress(t, wan, "192.0.2.2/24") || !resourceProgramAddress(t, wan, "192.0.2.99/24") {
		t.Fatal("conditional expiry disturbed the physical link, probe address or foreign address", err)
	}
	for _, target := range []struct {
		ns                netns.NsHandle
		address, tcp, udp string
	}{{lan, "198.51.100.20", "18081", "18081"}, {a, "192.0.2.42", "18093", "18094"}} {
		for protocol, port := range map[string]string{"tcp4": target.tcp, "udp4": target.udp} {
			var conn net.Conn
			var dialErr error
			if err := fixtureInsideNamespace(target.ns, func() error {
				conn, dialErr = net.DialTimeout(protocol, net.JoinHostPort(target.address, port), 250*time.Millisecond)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if dialErr != nil {
				continue
			}
			_ = conn.SetDeadline(time.Now().Add(250 * time.Millisecond))
			_, err := conn.Write([]byte("expired?"))
			var response [1]byte
			if err == nil {
				_, err = conn.Read(response[:])
			}
			conn.Close()
			if err == nil {
				t.Fatal("expired address or route still forwarded application traffic", target.address, protocol)
			}
		}
	}
	for _, process := range frozen {
		if err := syscall.Kill(process.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	waitPrimary(true)
	until(t, 3*time.Second, func() bool { return resourceProgramAddress(t, wan, "192.0.2.42/32") })
	var renewed health.Snapshot
	if err := json.Unmarshal(instance(healthA).Snapshot, &renewed); err != nil || renewed.ObservedNanos <= original.ObservedNanos {
		t.Fatal("recovery lacked a fresh real health observation", err)
	}
	traffic("A")
	identityTraffic()
	if capA.count("192.0.2.1", "192.0.2.42", 6, 18093) == 0 || capA.count("192.0.2.1", "192.0.2.42", 17, 18094) == 0 {
		t.Fatal("missing conditional-address TCP/UDP capture")
	}
	t.Log("restarted writer revoked original conditional address/PBR authority with control and Health paused; expired TCP/UDP stopped, physical/foreign state survived and fresh probes restored both paths")
	// Captures are bounded and retained as conventional Ethernet PCAP files.
	capA.assertTraffic(t, "192.0.2.2", "198.51.100.10")
	capB.assertTraffic(t, "203.0.113.2", "198.51.100.11")
	assertMarks()
	if capB.count("192.0.2.2", "198.51.100.10", 6, 18080) != 0 || capA.count("203.0.113.2", "198.51.100.11", 6, 18080) != 0 {
		t.Fatal("health probe escaped its configured WAN")
	}
	t.Log("confirmed rollback restored source and primary TCP/UDP; foreign objects and probe paths survived every transition")
	t.Log("ROUTERD_HEALTH_PBR_DAEMON_TEST_PASS")
}

// Marks are local socket metadata and are not carried in an Ethernet PCAP.
// A separate, foreign nft table observes OUTPUT without changing its verdict.
// Compare all probe packets with the mark=42 subset on each actual interface.
func healthPBRMarkCounters(t *testing.T) func() {
	t.Helper()
	c := &nftables.Conn{}
	table := c.AddTable(&nftables.Table{Name: "health_pbr_evidence", Family: nftables.TableFamilyINet})
	chain := c.AddChain(&nftables.Chain{Table: table, Name: "output", Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityFilter})
	for _, iface := range []string{"wan0", "wan1"} {
		for _, marked := range []bool{false, true} {
			name := make([]byte, 16)
			copy(name, iface)
			es := []expr.Any{
				&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: name},
				&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x46, 0xa0}}, // 18080
				// Only connection initiation belongs to the probe socket. After
				// worker death Linux can emit socketless, unmarked TCP resets.
				&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
				&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x12}, Xor: []byte{0}},
				&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x02}},
			}
			if marked {
				mark := make([]byte, 4)
				binary.NativeEndian.PutUint32(mark, 42)
				es = append(es, &expr.Meta{Key: expr.MetaKeyMARK, Register: 1}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: mark})
			}
			es = append(es, &expr.Counter{})
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: es, UserData: []byte(fmt.Sprintf("%s/%t", iface, marked))})
		}
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.DelTable(table)
		if err := c.Flush(); err != nil {
			t.Error(err)
		}
	})
	return func() {
		t.Helper()
		rules, err := c.GetRules(table, chain)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]uint64{}
		for _, r := range rules {
			for _, e := range r.Exprs {
				if counter, ok := e.(*expr.Counter); ok {
					counts[string(r.UserData)] = counter.Packets
				}
			}
		}
		for _, iface := range []string{"wan0", "wan1"} {
			all, marked := counts[iface+"/false"], counts[iface+"/true"]
			if all == 0 || all != marked {
				t.Fatalf("actual socket mark missing on %s: all=%d marked=%d", iface, all, marked)
			}
			t.Logf("OUTPUT %s probe SYN packets=%d all carry socket mark 42; foreign nft table preserved", iface, all)
		}
	}
}

func healthPBRUpdate(i runtimeplan.InstanceStatus) serviceconfig.Update {
	return serviceconfig.Update{SupervisorInstance: i.SupervisorInstance, Name: i.Name, Role: i.Role, ConfigDigest: i.ConfigDigest, Generation: i.Generation, ProducerEpoch: i.ProducerEpoch, Sequence: i.Sequence, Snapshot: i.Snapshot}
}

func healthPBRPrimary(t *testing.T) bool {
	t.Helper()
	routes, err := netlink.RouteListFiltered(unix.AF_INET, &netlink.Route{Table: 100}, netlink.RT_FILTER_TABLE)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Gw.Equal(net.ParseIP("192.0.2.1")) && r.Priority == 100 {
			return true
		}
	}
	return false
}

func healthPBRPeer(t *testing.T, local, peer, cidr, gateway string) netns.NsHandle {
	t.Helper()
	ns := fixturePeerNamespace(t, local, peer)
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	link, _ := h.LinkByName(peer)
	for _, cidr := range []string{cidr, "198.51.100.10/32", "198.51.100.11/32", "198.51.100.20/32"} {
		addr, _ := netlink.ParseAddr(cidr)
		if err := h.AddrAdd(link, addr); err != nil {
			t.Fatal(err)
		}
	}
	_, dst, _ := net.ParseCIDR("198.18.0.0/24")
	if err := h.RouteAdd(&netlink.Route{Dst: dst, LinkIndex: link.Attrs().Index, Gw: net.ParseIP(gateway)}); err != nil {
		t.Fatal(err)
	}
	return ns
}

func healthPBRListener(t *testing.T, ns netns.NsHandle, address, response string) net.Listener {
	t.Helper()
	var listener net.Listener
	if err := fixtureInsideNamespace(ns, func() error {
		var err error
		listener, err = net.Listen("tcp4", address)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = c.Write([]byte(response[:1]))
			c.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return listener
}

func healthPBRUDP(t *testing.T, ns netns.NsHandle, response string) {
	t.Helper()
	var conn *net.UDPConn
	if err := fixtureInsideNamespace(ns, func() error {
		var err error
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 18081})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			_, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP([]byte(response), peer)
		}
	}()
	t.Cleanup(func() { conn.Close(); <-done })
}

type healthPBRPacket struct {
	at   time.Time
	data []byte
}
type healthPBRCaptureState struct {
	mu      sync.Mutex
	packets []healthPBRPacket
	err     error
	dropped bool
}

func healthPBRCapture(t *testing.T, iface string) *healthPBRCaptureState {
	t.Helper()
	link, err := netlink.LinkByName(iface)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_ALL)))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: link.Attrs().Index, Protocol: htons(unix.ETH_P_ALL)}); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	c := &healthPBRCaptureState{}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65536)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, _, err := unix.Recvfrom(fd, buf, 0)
			// A startup capture can precede link-up; retain the bounded poll
			// through link-down rather than losing every later packet.
			if err == unix.EAGAIN || err == unix.EINTR || err == unix.ENETDOWN {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				c.err = err
				return
			}
			c.mu.Lock()
			if len(c.packets) < 4096 {
				c.packets = append(c.packets, healthPBRPacket{time.Now(), bytes.Clone(buf[:n])})
			} else {
				c.dropped = true
			}
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
		unix.Close(fd)
		if c.err != nil {
			t.Error("packet capture failed", c.err)
		}
		if c.dropped {
			t.Error("packet capture exceeded its 4096 frame bound")
		}
		if dir := os.Getenv("ROUTERD2_TEST_EVIDENCE_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Error(err)
				return
			}
			var pcap bytes.Buffer
			_ = binary.Write(&pcap, binary.LittleEndian, []uint32{0xa1b2c3d4, 0x00040002, 0, 0, 65535, 1})
			for _, p := range c.packets {
				_ = binary.Write(&pcap, binary.LittleEndian, []uint32{uint32(p.at.Unix()), uint32(p.at.Nanosecond() / 1000), uint32(len(p.data)), uint32(len(p.data))})
				pcap.Write(p.data)
			}
			if err := os.WriteFile(filepath.Join(dir, iface+".pcap"), pcap.Bytes(), 0644); err != nil {
				t.Error(err)
			}
		}
	})
	return c
}

func htons(n uint16) uint16 { return n<<8 | n>>8 }

// Count plain IPv4/IPv6 transport packets; extension-header layouts are not
// decoded by this small capture check.
func (c *healthPBRCaptureState) count(source, destination string, protocol byte, port uint16) int {
	return c.countPort(source, destination, protocol, port, false)
}

func (c *healthPBRCaptureState) countSourcePort(source, destination string, protocol byte, port uint16) int {
	return c.countPort(source, destination, protocol, port, true)
}

func (c *healthPBRCaptureState) countPort(source, destination string, protocol byte, port uint16, sourcePort bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	wantSource, wantDestination := net.ParseIP(source), net.ParseIP(destination)
	count := 0
	for _, packet := range c.packets {
		b := packet.data
		if len(b) < 14 {
			continue
		}
		var from, to net.IP
		var transport int
		switch binary.BigEndian.Uint16(b[12:14]) {
		case unix.ETH_P_IP:
			if len(b) < 38 || b[14]>>4 != 4 || b[23] != protocol {
				continue
			}
			ihl := int(b[14]&15) * 4
			if ihl < 20 || len(b) < 14+ihl+4 {
				continue
			}
			from, to, transport = net.IP(b[26:30]), net.IP(b[30:34]), 14+ihl
		case unix.ETH_P_IPV6:
			if len(b) < 58 || b[14]>>4 != 6 || b[20] != protocol {
				continue
			}
			from, to, transport = net.IP(b[22:38]), net.IP(b[38:54]), 54
		default:
			continue
		}
		portOffset := transport + 2
		if sourcePort {
			portOffset = transport
		}
		if from.Equal(wantSource) && to.Equal(wantDestination) && binary.BigEndian.Uint16(b[portOffset:portOffset+2]) == port {
			count++
		}
	}
	return count
}

func (c *healthPBRCaptureState) assertTraffic(t *testing.T, source, target string) {
	t.Helper()
	for _, expected := range []struct {
		source, target string
		protocol       byte
		port           uint16
	}{{source, target, 6, 18080}, {"198.18.0.2", "198.51.100.20", 6, 18081}, {"198.18.0.2", "198.51.100.20", 17, 18081}} {
		n := c.count(expected.source, expected.target, expected.protocol, expected.port)
		if n == 0 {
			t.Fatal("capture missing actual packet", fmt.Sprint(expected))
		}
		t.Logf("capture %s -> %s protocol=%d port=%d packets=%d", expected.source, expected.target, expected.protocol, expected.port, n)
	}
}

func importedHealthPBRProgram(t *testing.T) (config.Document, string, string) {
	t.Helper()
	work := t.TempDir()
	input, policy := filepath.Join(work, "legacy.yaml"), filepath.Join(work, "policy.yaml")
	if err := os.WriteFile(input, legacyHealth, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, legacyHealthPolicy, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", policy, input)
	var review bytes.Buffer
	command.Stderr = &review
	body, err := command.Output()
	if err != nil {
		t.Fatal(err, review.String())
	}
	imported, err := config.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	var a, b *serviceconfig.Service
	for _, resource := range imported.Resources {
		if resource.Service == nil || resource.Service.Health == nil {
			continue
		}
		switch resource.Service.Health.Interface {
		case "wan0":
			a = resource.Service
		case "wan1":
			b = resource.Service
		}
	}
	if a == nil || b == nil {
		t.Fatal("migration omitted a scoped WAN probe")
	}
	healthA, healthB := a.ModuleName(), b.ModuleName()
	source := strings.ReplaceAll(healthPBRProgram, "service.health-a.", healthA+".")
	source = strings.ReplaceAll(source, "service.health-b.", healthB+".")
	program, err := config.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	// The CLI supplies the complete probe settings. Native resources supply
	// this fixture's addresses, routes, marks and conditional failover policy.
	for i := range program.Resources {
		r := &program.Resources[i]
		if r.Service == nil || r.Service.Health == nil {
			continue
		}
		if r.Service.Health.Interface == "wan0" {
			r.Service = a
		} else {
			r.Service = b
		}
	}
	return program, healthA, healthB
}

const healthPBRProgram = `schema_version: 2
resources:
  - id: wan-a
    interface: {name: wan0}
  - id: wan-b
    interface: {name: wan1}
  - id: lan
    interface: {name: lan0}
  - id: address-a
    owner: wan-a
    address: {interface: wan0, cidr: 192.0.2.2/24}
  - id: address-b
    owner: wan-b
    address: {interface: wan1, cidr: 203.0.113.2/24}
  - id: address-lan
    owner: lan
    address: {interface: lan0, cidr: 198.18.0.1/24}
  - id: probe-a
    depends_on: [address-a]
    route: {destination: 198.51.100.10/32, gateway: 192.0.2.1, interface: wan0, table: 101}
  - id: probe-b
    depends_on: [address-b]
    route: {destination: 198.51.100.11/32, gateway: 203.0.113.1, interface: wan1, table: 101}
  - id: probe-return-a
    depends_on: [address-a]
    route: {destination: 198.51.100.10/32, gateway: 192.0.2.1, interface: wan0, table: 254}
  - id: probe-return-b
    depends_on: [address-b]
    route: {destination: 198.51.100.11/32, gateway: 203.0.113.1, interface: wan1, table: 254}
  - id: probe-policy
    policy_rule: {family: 4, priority: 1100, table: 101, mark: 42}
  - id: health-a
    depends_on: [probe-a, probe-policy]
    service:
      name: health-a
      health: {resource: health-a, interface: wan0, source_address: 192.0.2.2, target: 198.51.100.10, mark: 42, protocol: tcp, port: 18080, interval_ms: 500, timeout_ms: 500, healthy_threshold: 2, unhealthy_threshold: 3}
  - id: health-b
    depends_on: [probe-b, probe-policy]
    service:
      name: health-b
      health: {resource: health-b, interface: wan1, source_address: 203.0.113.2, target: 198.51.100.11, mark: 42, protocol: tcp, port: 18080, interval_ms: 500, timeout_ms: 500, healthy_threshold: 2, unhealthy_threshold: 3}
  - id: primary
    depends_on: [address-a]
    when: {fact: service.health-a.active, max_age: 2500ms, for: 1500ms}
    route: {destination: 0.0.0.0/0, gateway: 192.0.2.1, interface: wan0, table: 100, metric: 100}
  - id: conditional-address
    when: {fact: service.health-a.active, max_age: 2500ms}
    address: {interface: wan0, cidr: 192.0.2.42/32}
  - id: backup
    depends_on: [address-b]
    when: {fact: service.health-b.active, max_age: 2500ms}
    route: {destination: 0.0.0.0/0, gateway: 203.0.113.1, interface: wan1, table: 100, metric: 200}
  - id: application-policy
    policy_rule: {family: 4, priority: 1000, table: 100, source: 198.18.0.0/24}
  - id: forwarding
    forwarding: {ipv4: true}
`
