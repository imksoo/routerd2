//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	wire "github.com/imksoo/routerd/pkg/dhcpv4client"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/clocktest"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCPv4OriginalLeaseAuthority(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCPv4OriginalLeaseAuthority", 180*time.Second) {
		return
	}
	runDHCPv4Authority(t, nil)
}

// The positive clock mutation is available only through the dedicated guest
// PID 1 broker. This test must refuse before creating namespaces on a host.
func TestDaemonDHCPv4ClockCorrections(t *testing.T) {
	file, err := clocktest.ForwardFileFromEnv()
	if err != nil {
		if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
			t.Fatal(err)
		}
		t.Skip("dedicated no-NIC QEMU clock broker required")
	}
	_ = file.Close()
	if !isolateFixtureTimeout(t, "TestDaemonDHCPv4ClockCorrections", 180*time.Second) {
		return
	}
	client, err := clocktest.ClientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.Step("restore"); err != nil {
			t.Error("restore guest clock", err)
		}
		_ = client.Close()
	})
	runDHCPv4Authority(t, client)
}

func runDHCPv4Authority(t *testing.T, clockClient *clocktest.Client) {
	t.Helper()
	ctx := context.Background()
	peerNS := fixturePeerNamespace(t, "wan0", "peer0")
	peer, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	link, err := peer.LinkByName("peer0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr("192.0.2.1/24")
	if err = peer.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
	var dhcp, data *net.UDPConn
	var tcp net.Listener
	if err = fixtureInsideNamespace(peerNS, func() error {
		lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
			var inner error
			if err := raw.Control(func(fd uintptr) {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, "peer0")
				if inner == nil {
					inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1)
				}
			}); err != nil {
				return err
			}
			return inner
		}}
		packet, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:67")
		if err != nil {
			return err
		}
		dhcp = packet.(*net.UDPConn)
		data, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 18092})
		if err != nil {
			return err
		}
		tcp, err = net.Listen("tcp4", "192.0.2.1:18091")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer dhcp.Close()
	defer data.Close()
	defer tcp.Close()
	var acknowledge atomic.Bool
	acknowledge.Store(true)
	var leaseSeconds atomic.Uint32
	leaseSeconds.Store(3)
	if clockClient != nil {
		leaseSeconds.Store(20)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := dhcp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 240 {
				continue
			}
			m, err := wire.Decode(buf[:n])
			if err != nil {
				continue
			}
			kind := byte(wire.MessageACK)
			if m.MessageType() == wire.MessageDiscover {
				kind = wire.MessageOffer
			} else if m.MessageType() != wire.MessageRequest || !acknowledge.Load() {
				continue
			}
			u32 := func(n uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, n); return b }
			life, t1, t2 := leaseSeconds.Load(), uint32(1), uint32(2)
			if life == 20 {
				t1, t2 = 4, 12
			}
			opts := map[byte][]byte{wire.OptionServerID: {192, 0, 2, 1}, wire.OptionSubnetMask: {255, 255, 255, 0}, wire.OptionRouter: {192, 0, 2, 1}, wire.OptionLeaseTime: u32(life), wire.OptionT1: u32(t1), wire.OptionT2: u32(t2)}
			packet := wire.EncodeRequest(kind, m.XID, m.CHAddr, opts)
			packet[0] = 2
			copy(packet[16:20], []byte{192, 0, 2, 10})
			_, _ = dhcp.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4bcast, Port: 68})
		}
	}()
	go func() {
		buf := make([]byte, 64)
		for {
			_, peer, err := data.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = data.WriteToUDP([]byte("lease"), peer)
		}
	}()
	go func() {
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = c.Write([]byte("lease"))
			_ = c.Close()
		}
	}()
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := netlink.ParseAddr("192.0.2.99/32")
	if err = netlink.AddrAdd(wan, foreign); err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("ROUTERD2_TEST_EVIDENCE_DIR"); dir != "" {
		t.Setenv("ROUTERD2_TEST_EVIDENCE_DIR", filepath.Join(dir, "dhcp4-authority"))
	}
	f := startFixtureDaemon(t, config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{{Name: "wan0"}}}, Services: []serviceconfig.Service{{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}}}}))
	// Stop capture before daemon shutdown can change its interface state.
	capture := healthPBRCapture(t, "wan0")
	instance := func() runtimeplan.InstanceStatus {
		t.Helper()
		var s runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.wan", nil, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	snapshot := func() dhcp4.Snapshot {
		t.Helper()
		var s dhcp4.Snapshot
		b := instance().Snapshot
		if len(b) > 0 {
			if err := json.Unmarshal(b, &s); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	module := func(name string) supervisor.ModuleStatus {
		t.Helper()
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, m := range status.Modules {
			if m.Name == name {
				return m
			}
		}
		t.Fatalf("missing module %s", name)
		return supervisor.ModuleStatus{}
	}
	restart := func(name string) {
		t.Helper()
		old := module(name)
		if err := syscall.Kill(old.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 15*time.Second, func() bool { m := module(name); return m.PID > 0 && m.PID != old.PID && m.Ready })
	}
	kernel := func(want bool) bool {
		t.Helper()
		addresses, err := netlink.AddrList(wan, unix.AF_INET)
		if err != nil {
			t.Fatal(err)
		}
		leased, kept := false, false
		for _, a := range addresses {
			leased = leased || a.IP.String() == "192.0.2.10"
			kept = kept || a.IP.String() == "192.0.2.99"
		}
		if !kept {
			t.Fatal("foreign static address removed")
		}
		routes, err := netlink.RouteList(wan, unix.AF_INET)
		if err != nil {
			t.Fatal(err)
		}
		gateway := false
		for _, r := range routes {
			gateway = gateway || r.Gw.String() == "192.0.2.1" && (r.Dst == nil || r.Dst.String() == "0.0.0.0/0")
		}
		return leased == want && gateway == want
	}
	traffic := func() {
		t.Helper()
		for _, protocol := range []string{"tcp4", "udp4"} {
			port := "18091"
			if protocol == "udp4" {
				port = "18092"
			}
			c, err := net.DialTimeout(protocol, "192.0.2.1:"+port, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			if protocol == "udp4" {
				_, err = c.Write([]byte("probe"))
			}
			body := make([]byte, 5)
			_, readErr := io.ReadFull(c, body)
			local := c.LocalAddr().String()
			_ = c.Close()
			if err != nil || readErr != nil || string(body) != "lease" || len(local) < len("192.0.2.10:") || local[:len("192.0.2.10:")] != "192.0.2.10:" {
				t.Fatal("actual leased TCP/UDP failed", protocol, local, string(body), err, readErr)
			}
		}
	}
	step := func(action string, expectedDelta time.Duration) {
		t.Helper()
		before, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		got, err := clockClient.Step(action)
		if err != nil {
			t.Fatal(action, err)
		}
		after, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		elapsed := time.Duration(after.BoottimeNanoseconds - before.BoottimeNanoseconds)
		delta := after.WallTime.Sub(before.WallTime) - elapsed
		if after.BootID != before.BootID || got.Clock.BootID != after.BootID || elapsed < 0 || elapsed > 3*time.Second || delta < expectedDelta-time.Second || delta > expectedDelta+time.Second {
			t.Fatal("unexpected actual guest clock correction", action, before, got, after)
		}
		t.Logf("actual CLOCK_REALTIME action=%s wall_delta=%s BOOTTIME_elapsed=%s boot_unchanged=true", action, after.WallTime.Sub(before.WallTime), elapsed)
	}
	until(t, 15*time.Second, func() bool { return snapshot().Active })
	first := snapshot()
	if first.BootID == "" || first.LeaseClock.ExpiresNanos == 0 {
		t.Fatal("DHCP worker omitted original BOOTTIME lease authority")
	}
	until(t, 10*time.Second, func() bool { return kernel(true) })
	traffic()
	if clockClient != nil {
		acknowledge.Store(false)
		time.Sleep(100 * time.Millisecond) // drain any already-sent ACK before pinning the grant
		first = snapshot()
		step("forward", 24*time.Hour)
		now, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		if !first.ExpiresAt.Before(now.WallTime) || !dhcp4.CurrentLease(snapshot(), now) || snapshot().LeaseClock != first.LeaseClock || !kernel(true) {
			t.Fatal("forward UTC correction revoked or rewrote original BOOT lease", first, snapshot(), now)
		}
		traffic()
		var state control.State
		if err := f.control.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
			t.Fatal(err)
		}
		if state.ConfigOmitted {
			t.Fatal("configuration omitted")
		}
		changed := false
		for i := range state.Config.Resources {
			r := &state.Config.Resources[i]
			if r.Interface != nil && r.Interface.Name == "wan0" {
				r.Interface.MTU = 1400
				changed = true
			}
		}
		if !changed {
			t.Fatal("missing declared WAN")
		}
		if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "dhcp4-clock-reapply", Config: state.Config}, nil); err != nil {
			t.Fatal(err)
		}
		until(t, 8*time.Second, func() bool {
			link, err := netlink.LinkByName("wan0")
			return err == nil && link.Attrs().MTU == 1400 && kernel(true)
		})
		if snapshot().LeaseClock != first.LeaseClock {
			t.Fatal("UTC reapply replaced original grant", first, snapshot())
		}
		traffic()
		step("restore", -24*time.Hour)
		leaseSeconds.Store(3)
		acknowledge.Store(true)
	}
	until(t, 10*time.Second, func() bool {
		return snapshot().LeaseClock.AcquiredNanos > first.LeaseClock.AcquiredNanos && snapshot().LeaseClock.ExpiresNanos-snapshot().LeaseClock.AcquiredNanos == uint64(3*time.Second)
	})
	traffic()
	paused := 0
	t.Cleanup(func() {
		if paused > 0 {
			_ = syscall.Kill(paused, syscall.SIGCONT)
		}
	})
	pause := func() serviceconfig.Update {
		t.Helper()
		acknowledge.Store(false)
		m := module("service.wan")
		if err := syscall.Kill(m.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		paused = m.PID
		return healthPBRUpdate(instance())
	}
	resume := func() {
		t.Helper()
		acknowledge.Store(true)
		if err := syscall.Kill(paused, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		paused = 0
		until(t, 15*time.Second, func() bool { clock, _ := boottime.Read(); return dhcp4.CurrentLease(snapshot(), clock) && kernel(true) })
		traffic()
	}
	old := pause()
	if clockClient != nil {
		step("backward", -24*time.Hour)
	}
	until(t, 12*time.Second, func() bool { return kernel(false) })
	if m := module("service.wan"); !m.Ready || m.PID != paused {
		t.Fatal("expiry fixture lost the original ready producer", m)
	}
	if !snapshot().Active {
		t.Fatal("raw last active snapshot did not remain independently stale")
	}
	old = healthPBRUpdate(instance()) // all pre-stop RPCs have drained before expiry.
	if err := f.control.Do(ctx, "POST", "/v1/services/state", old, nil); err != nil || !kernel(false) {
		t.Fatal("identical retry renewed expired lease", err)
	}
	if clockClient != nil {
		step("restore", 24*time.Hour)
	}
	resume()
	old = healthPBRUpdate(instance())
	restart("service.wan")
	until(t, 15*time.Second, func() bool { return instance().Generation > old.Generation && snapshot().Active && kernel(true) })
	if err := f.control.Do(ctx, "POST", "/v1/services/state", old, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("old DHCP worker notification accepted", err)
	}
	traffic()
	// Control recovery deliberately quiesces all previous dynamic workers.
	// Withhold ACKs: their cached positive lease must remain only a hint in
	// the replacement worker, even if its original deadline is still future.
	expired := healthPBRUpdate(instance())
	acknowledge.Store(false)
	stableGeneration := module("service.wan").Generation
	restart("control")
	until(t, 10*time.Second, func() bool {
		return module("service.wan").Generation > stableGeneration && !snapshot().Active && kernel(false)
	})
	if err := f.control.Do(ctx, "POST", "/v1/services/state", expired, nil); !localapi.IsHTTPStatus(err, 409) || !kernel(false) {
		t.Fatal("control restart revived an expired original grant", err)
	}
	acknowledge.Store(true)
	until(t, 15*time.Second, func() bool { clock, _ := boottime.Read(); return dhcp4.CurrentLease(snapshot(), clock) && kernel(true) })
	traffic()
	until(t, 5*time.Second, func() bool {
		return capture.count("0.0.0.0", "255.255.255.255", unix.IPPROTO_UDP, 67) > 0 && capture.count("192.0.2.10", "192.0.2.1", unix.IPPROTO_TCP, 18091) > 0 && capture.count("192.0.2.10", "192.0.2.1", unix.IPPROTO_UDP, 18092) > 0
	})
	t.Log("real DHCP worker/independent server -> original BOOT grant -> kernel address/default route -> TCP/UDP/PCAP; renewal, SIGSTOP expiry, duplicate retry, worker/control SIGKILL, stale generation and foreign address preservation passed")
	if clockClient != nil {
		t.Log("ROUTERD_DHCP4_CLOCK_TEST_PASS")
	} else {
		t.Log("ROUTERD_DHCP4_AUTHORITY_TEST_PASS")
	}
}
