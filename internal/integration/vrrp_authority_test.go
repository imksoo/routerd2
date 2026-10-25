//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/clocktest"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonVRRPOriginalOwnership(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonVRRPOriginalOwnership", 100*time.Second) {
		return
	}
	runVRRPAuthority(t, nil, false)
}

func TestDaemonVRRPClockCorrections(t *testing.T) {
	file, err := clocktest.ForwardFileFromEnv()
	if err != nil {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("dedicated no-NIC QEMU clock broker required")
	}
	_ = file.Close()
	if !isolateFixtureTimeout(t, "TestDaemonVRRPClockCorrections", 100*time.Second) {
		return
	}
	client, err := clocktest.ClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.Step("restore"); err != nil {
			t.Error(err)
		}
		_ = client.Close()
	})
	runVRRPAuthority(t, client, false)
}

func TestDaemonVRRPConditionalOwnership(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonVRRPConditionalOwnership", 100*time.Second) {
		return
	}
	runVRRPAuthority(t, nil, true)
}

func TestDaemonVRRPGracefulAdmission(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonVRRPGracefulAdmission", 100*time.Second) {
		return
	}
	runVRRPAuthorityOptions(t, nil, true, true)
}

func runVRRPAuthority(t *testing.T, clockClient *clocktest.Client, conditional bool) {
	t.Helper()
	runVRRPAuthorityOptions(t, clockClient, conditional, false)
}
func runVRRPAuthorityOptions(t *testing.T, clockClient *clocktest.Client, conditional, graceful bool) {
	t.Helper()
	ctx := context.Background()
	peerNS := fixturePeerNamespace(t, "lan0", "peer0")
	peer, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peerLink, err := peer.LinkByName("peer0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("192.0.2.1/24")
	if err := peer.AddrAdd(peerLink, address); err != nil {
		t.Fatal(err)
	}
	lan, err := netlink.LinkByName("lan0")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := netlink.ParseAddr("192.0.2.99/32")
	if err := netlink.AddrAdd(lan, foreign); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "r2-vrrp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := vrrp.Config{Interface: "lan0", VIPInterface: "ha4", Family: "ipv4", SourceAddress: "192.0.2.2", VirtualRouterID: 42, Priority: 255, AddressOwner: true, AdvertIntervalCentiseconds: 200, Addresses: []string{"192.0.2.254/24"}}
	if graceful {
		c.GracefulActivation = &vrrp.GracefulActivation{ReadyWhen: resourceprogram.FactCondition{Fact: "service.probe.active"}, Timeout: "1s"}
	}
	source := config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.2/24"}}}}, Services: []serviceconfig.Service{{Name: "ha", VRRP: &c}}})
	if graceful {
		source.Resources = append(source.Resources, resourceprogram.Resource{ID: "probe", Service: &serviceconfig.Service{Name: "probe", Health: &health.Config{Interface: "lan0", SourceAddress: "192.0.2.2", Target: "192.0.2.1", Protocol: "tcp", Port: 18097, IntervalMillis: 100, TimeoutMillis: 50, HealthyThreshold: 1, UnhealthyThreshold: 1}}})
	}
	if conditional {
		for i := range source.Resources {
			if source.Resources[i].Service != nil && source.Resources[i].Service.VRRP != nil {
				source.Resources[i].ReadyWhen = &resourceprogram.FactCondition{Fact: "service.ha.ready"}
			}
		}
	}
	if evidence := os.Getenv("ROUTERD2_TEST_EVIDENCE_DIR"); evidence != "" {
		t.Setenv("ROUTERD2_TEST_EVIDENCE_DIR", filepath.Join(evidence, "vrrp-authority"))
	}
	capture := healthPBRCapture(t, "lan0")
	f := startFixtureDaemonAt(t, source, dir)
	nc := control.NewNetworkClient(filepath.Join(dir, "run"))
	instance := func() runtimeplan.InstanceStatus {
		var s runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.ha", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	snapshot := func() vrrp.Snapshot {
		var s vrrp.Snapshot
		if err := json.Unmarshal(instance().Snapshot, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	module := func(name string) supervisor.ModuleStatus {
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
	hasVIP := func() bool {
		link, err := netlink.LinkByName("ha4")
		if err != nil {
			return false
		}
		addresses, err := netlink.AddrList(link, unix.AF_INET)
		if err != nil {
			return false
		}
		for _, a := range addresses {
			if a.IP.String() == "192.0.2.254" {
				return true
			}
		}
		return false
	}
	var readinessListener net.Listener
	if graceful {
		until(t, 8*time.Second, func() bool {
			s := snapshot()
			return s.State == vrrp.StateActive && !s.Active && s.ActivationState == "ReadinessTimeout"
		})
		if hasVIP() {
			t.Fatal("graceful timeout force-published actual VIP")
		}
		if _, err := netlink.LinkByName("ha4"); err == nil {
			t.Fatal("unready MASTER acquired VMAC link")
		}
		var state control.State
		if err := f.control.Do(ctx, "GET", "/v1/config", nil, &state); err != nil || state.Healthy {
			t.Fatal("unpublished MASTER reported healthy", err)
		}
		capture.mu.Lock()
		electionPackets := 0
		earlyARP := false
		for _, packet := range capture.packets {
			b := packet.data
			if len(b) >= 34 && b[12] == 8 && b[13] == 0 && b[23] == 112 {
				electionPackets++
			}
			if len(b) >= 42 && b[12] == 8 && b[13] == 6 && bytes.Equal(b[28:32], net.ParseIP("192.0.2.254").To4()) {
				earlyARP = true
			}
		}
		capture.mu.Unlock()
		if electionPackets == 0 || earlyARP {
			t.Fatal("graceful election did not retain advertisements or announced VIP early", electionPackets, earlyARP)
		}
		readinessListener = healthPBRListener(t, peerNS, "192.0.2.1:18097", "H")
		t.Log("elected MASTER remained unpublished/unhealthy after timeout, kept election advertisements and emitted no VIP ARP")
	}
	until(t, 8*time.Second, func() bool {
		var state control.State
		return f.control.Do(ctx, "GET", "/v1/config", nil, &state) == nil && state.Healthy && hasVIP()
	})
	link, err := netlink.LinkByName("ha4")
	if err != nil || link.Type() != "macvlan" || !bytes.Equal(link.Attrs().HardwareAddr, vrrp.VirtualMAC("ipv4", 42)) {
		t.Fatal("actual VMAC link missing", err)
	}
	tcp, err := net.Listen("tcp4", "192.0.2.254:18095")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	go func() {
		for {
			conn, err := tcp.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("ha"))
			_ = conn.Close()
		}
	}()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.254"), Port: 18096})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		buffer := make([]byte, 64)
		for {
			_, addr, err := udp.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = udp.WriteToUDP([]byte("ha"), addr)
		}
	}()
	traffic := func(want bool) {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			port := "18095"
			if protocol == "udp4" {
				port = "18096"
			}
			succeeded := false
			err := fixtureInsideNamespace(peerNS, func() error {
				conn, err := net.DialTimeout(protocol, "192.0.2.254:"+port, 400*time.Millisecond)
				if err != nil {
					return nil
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
				if protocol == "udp4" {
					if _, err = conn.Write([]byte("hi")); err != nil {
						return nil
					}
				}
				body := make([]byte, 2)
				_, err = io.ReadFull(conn, body)
				succeeded = err == nil && string(body) == "ha"
				return nil
			})
			if err != nil || succeeded != want {
				t.Fatalf("peer to VIP %s want=%t got=%t error=%v", protocol, want, succeeded, err)
			}
		}
	}
	traffic(true)
	if graceful {
		_ = readinessListener.Close()
		until(t, 5*time.Second, func() bool {
			var observation runtimeplan.InstanceStatus
			if err := f.control.Do(ctx, "GET", "/v1/services/service.probe", nil, &observation); err != nil {
				return false
			}
			var sample health.Snapshot
			return json.Unmarshal(observation.Snapshot, &sample) == nil && !sample.Active
		})
		if !hasVIP() {
			t.Fatal("readiness degradation withdrew admitted MASTER VIP")
		}
		traffic(true)
		readinessListener = healthPBRListener(t, peerNS, "192.0.2.1:18097", "H")
		until(t, 5*time.Second, func() bool {
			var observation runtimeplan.InstanceStatus
			if err := f.control.Do(ctx, "GET", "/v1/services/service.probe", nil, &observation); err != nil {
				return false
			}
			var sample health.Snapshot
			return json.Unmarshal(observation.Snapshot, &sample) == nil && sample.Active
		})
		t.Log("late readiness admitted VIP, and later readiness loss preserved actual TCP/UDP until election withdrawal")
	}
	oldWorker := module("service.ha")
	if oldWorker.PID <= 1 {
		t.Fatal("actual VRRP worker missing")
	}
	if err := syscall.Kill(oldWorker.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(f.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	paused := true
	defer func() {
		if paused {
			_ = syscall.Kill(oldWorker.PID, syscall.SIGKILL)
			_ = syscall.Kill(f.pid, syscall.SIGCONT)
		}
	}()
	first := snapshot()
	beforeInstance := instance()
	projection, err := nc.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, iface := range projection.Desired.Interfaces {
		if iface.Name == "ha4" {
			o := iface.AddressOptions["192.0.2.254/24"]
			found = o.BootLifetime.BootID == first.BootID && o.BootLifetime.ValidNanos == first.ExpiresNanos && o.ValidUntil.IsZero()
		}
	}
	if !found {
		t.Fatal("writer did not retain exact original boot deadline")
	}
	offset := time.Duration(0)
	if clockClient != nil {
		for _, step := range []struct {
			action string
			offset time.Duration
		}{{"forward", 24 * time.Hour}, {"backward", -24 * time.Hour}} {
			before, err := boottime.Read()
			if err != nil {
				t.Fatal(err)
			}
			result, err := clockClient.Step(step.action)
			if err != nil {
				t.Fatal(err)
			}
			after, err := boottime.Read()
			if err != nil {
				t.Fatal(err)
			}
			delta := after.WallTime.Sub(before.WallTime) - time.Duration(after.BoottimeNanoseconds-before.BoottimeNanoseconds)
			want := step.offset - offset
			if after.BootID != before.BootID || result.Clock.BootID != after.BootID || delta < want-time.Second || delta > want+time.Second {
				t.Fatal("guest clock did not perform fixed actual correction", step.action, delta, want)
			}
			offset = step.offset
			if !vrrp.CurrentOwnership(first, after) {
				t.Fatal("clock test overran original ownership before reapply")
			}
			if err := nc.Apply(ctx, projection.Revision, projection.Desired); err != nil {
				t.Fatal("same original BOOT projection reapply failed", err)
			}
			if !hasVIP() {
				t.Fatal("UTC correction prematurely revoked VIP")
			}
			traffic(true)
			t.Logf("actual VRRP CLOCK_REALTIME action=%s wall_delta=%s original_BOOT_deadline_unchanged=true", step.action, delta)
		}
	} else if err := nc.Apply(ctx, projection.Revision, projection.Desired); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		clock, err := boottime.Read()
		return err == nil && uint64(clock.BoottimeNanoseconds) >= first.ExpiresNanos && !hasVIP()
	})
	if err := nc.Apply(ctx, projection.Revision, projection.Desired); err != nil {
		t.Log("expired projection safely refused:", err)
	}
	if hasVIP() {
		t.Fatal("expired original projection recreated VIP")
	}
	traffic(false)
	if err := syscall.Kill(oldWorker.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(f.pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	paused = false
	if clockClient != nil {
		if _, err := clockClient.Step("restore"); err != nil {
			t.Fatal(err)
		}
	}
	until(t, 10*time.Second, func() bool {
		m := module("service.ha")
		return m.Ready && m.Generation > oldWorker.Generation && hasVIP()
	})
	traffic(true)
	stale := serviceconfig.Update{SupervisorInstance: beforeInstance.SupervisorInstance, Name: beforeInstance.Name, Role: beforeInstance.Role, ConfigDigest: beforeInstance.ConfigDigest, Generation: beforeInstance.Generation, ProducerEpoch: beforeInstance.ProducerEpoch, Sequence: beforeInstance.Sequence + 100, Snapshot: beforeInstance.Snapshot}
	if err := f.control.Do(ctx, "POST", "/v1/services/effects", stale, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("retired VRRP worker accepted after replacement", err)
	}
	beforeControl := instance()
	oldControl := module("control")
	if err := syscall.Kill(oldControl.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	until(t, 15*time.Second, func() bool {
		m := module("control")
		return m.Ready && m.Generation > oldControl.Generation && module("service.ha").Generation > beforeControl.Generation && hasVIP()
	})
	stale = serviceconfig.Update{SupervisorInstance: beforeControl.SupervisorInstance, Name: beforeControl.Name, Role: beforeControl.Role, ConfigDigest: beforeControl.ConfigDigest, Generation: beforeControl.Generation, ProducerEpoch: beforeControl.ProducerEpoch, Sequence: beforeControl.Sequence + 100, Snapshot: beforeControl.Snapshot}
	if err := f.control.Do(ctx, "POST", "/v1/services/effects", stale, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("cold control accepted retired generation", err)
	}
	traffic(true)
	if conditional {
		// Add a real protocol worker whose interface is the VRRP-owned macvlan.
		// Its activation must follow kernel ownership; removing both exercises the
		// final VRRP callback while the coordinator is already waiting for exit.
		var current control.State
		if err := f.control.Do(ctx, "GET", "/v1/config", nil, &current); err != nil {
			t.Fatal(err)
		}
		withDependent := current.Config
		withDependent.Resources = append(withDependent.Resources, resourceprogram.Resource{ID: "dependent", DependsOn: []string{"service/ha"}, When: &resourceprogram.FactCondition{Fact: "service.ha.active"}, Service: &serviceconfig.Service{Name: "dependent", DHCP4: &serviceconfig.DHCP4{Interface: "ha4"}}})
		if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: current.Revision, RequestID: "conditional-vrrp-dependent", Config: withDependent}, &current); err != nil {
			t.Fatal(err)
		}
		until(t, 8*time.Second, func() bool { return module("service.dependent").Ready && hasVIP() })
		traffic(true)
		base := config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"192.0.2.2/24"}}}}})
		dependent := module("service.dependent")
		if dependent.PID <= 1 {
			t.Fatal("actual dependent worker missing")
		}
		if err := syscall.Kill(dependent.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		dependentPaused := true
		defer func() {
			if dependentPaused {
				_ = syscall.Kill(dependent.PID, syscall.SIGCONT)
			}
		}()
		withdrawal := make(chan error, 1)
		go func() {
			withdrawal <- f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: current.Revision, RequestID: "conditional-vrrp-withdraw", Config: base}, nil)
		}()
		until(t, time.Second, func() bool { return module("service.dependent").State == "stopping" })
		if !hasVIP() {
			t.Fatal("VIP withdrawn while actual dependent was still stopping")
		}
		select {
		case err := <-withdrawal:
			t.Fatalf("cutover finished before dependent exit: %v", err)
		default:
		}
		if err := syscall.Kill(dependent.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		dependentPaused = false
		select {
		case err := <-withdrawal:
			if err != nil {
				t.Fatal("worker stop/withdrawal ACK deadlocked:", err)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("actual worker withdrawal did not finish")
		}
		until(t, 8*time.Second, func() bool { return module("service.ha").PID == 0 && module("service.dependent").PID == 0 && !hasVIP() })
		traffic(false)
		t.Log("conditional VRRP startup/effect ACK, dependent activation on owned macvlan, VIP retained while dependent stopping, final worker withdrawal and config cutover passed")
	}
	addresses, err := netlink.AddrList(lan, unix.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, a := range addresses {
		kept = kept || a.IP.String() == "192.0.2.99"
	}
	if !kept {
		t.Fatal("foreign address removed")
	}
	until(t, time.Second, func() bool {
		return capture.count("192.0.2.1", "192.0.2.254", unix.IPPROTO_TCP, 18095) > 0 && capture.count("192.0.2.1", "192.0.2.254", unix.IPPROTO_UDP, 18096) > 0
	})
	capture.mu.Lock()
	adverts := 0
	for _, p := range capture.packets {
		b := p.data
		if len(b) >= 42 && b[12] == 8 && b[13] == 0 && b[23] == 112 && bytes.Equal(b[6:12], vrrp.VirtualMAC("ipv4", 42)) {
			adverts++
		}
	}
	capture.mu.Unlock()
	if adverts == 0 {
		t.Fatal("missing actual VMAC VRRP advertisements")
	}
	t.Log(fmt.Sprintf("actual VIP TCP/UDP, %d VMAC advertisements, original BOOT expiry/reapply, worker/control replacement and stale-generation refusal; foreign address retained", adverts))
}
