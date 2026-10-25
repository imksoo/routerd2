//go:build linux && integration

package integration

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonVRRPUnicastIPv4(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPUnicastIPv4") {
		runVRRPUnicast(t, "ipv4", false)
	}
}

func TestDaemonVRRPUnicastIPv6(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPUnicastIPv6") {
		runVRRPUnicast(t, "ipv6", false)
	}
}

func TestDaemonVRRPHealthTracking(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPHealthTracking") {
		runVRRPUnicast(t, "ipv4", true)
	}
}

func runVRRPUnicast(t *testing.T, family string, tracking bool) {
	keepalived, err := exec.LookPath("keepalived")
	if err != nil {
		t.Fatal("independent keepalived peer is required", err)
	}
	peerNS := fixturePeerNamespace(t, "lan0", "peer0")
	routerNS, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer routerNS.Close()
	local, peer, vip, suffix := "192.0.2.2", "192.0.2.3", "192.0.2.254", "4"
	bits := 24
	vips := []string{vip + "/32"}
	if family == "ipv6" {
		local, peer, vip, suffix, bits = "2001:db8:42::2", "2001:db8:42::3", "2001:db8:42::254", "6", 64
		vips = []string{"fe80::42/64", vip + "/128"}
	}
	handle, err := netlink.NewHandleAt(peerNS, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	link, err := handle.LinkByName("peer0")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := netlink.ParseAddr(peer + "/" + strconv.Itoa(bits))
	address.Flags = unix.IFA_F_NODAD
	if err := handle.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
	capture := healthPBRCapture(t, "lan0")
	c := vrrp.Config{Interface: "lan0", VIPInterface: "ha", Family: family, SourceAddress: local, UnicastPeers: []netip.Addr{netip.MustParseAddr(peer)}, VirtualRouterID: 42, Priority: 150, AdvertIntervalCentiseconds: 20, Addresses: vips, AcceptMode: true}
	iface := network.Interface{Name: "lan0", Addresses: []string{local + "/" + strconv.Itoa(bits)}}
	if family == "ipv6" {
		iface.AddressOptions = map[string]network.AddressOptions{iface.Addresses[0]: {NoDAD: true}}
	}
	var probe net.Listener
	services := []serviceconfig.Service{{Name: "ha", VRRP: &c}}
	if tracking {
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		c.Track = []vrrp.Track{{Service: "probe", Penalty: 100, HealthyThreshold: 2, UnhealthyThreshold: 3, MaxAgeMillis: 1500}}
		services = append(services, serviceconfig.Service{Name: "probe", Health: &health.Config{Interface: "lan0", SourceAddress: local, Target: peer, Protocol: "tcp", Port: 18097, IntervalMillis: 300, TimeoutMillis: 50, HealthyThreshold: 1, UnhealthyThreshold: 1}})
	}
	f := startFixtureDaemon(t, config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{iface}}, Services: services}))
	snapshot := func() vrrp.Snapshot {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/service.ha", nil, &instance); err != nil {
			t.Fatal(err)
		}
		var s vrrp.Snapshot
		if err := json.Unmarshal(instance.Snapshot, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	hasVIP := func() bool {
		link, err := netlink.LinkByName("ha")
		if err != nil {
			return false
		}
		addresses, err := netlink.AddrList(link, unix.AF_UNSPEC)
		if err != nil {
			t.Fatal(err)
		}
		for _, address := range addresses {
			if address.IP.String() == vip {
				return true
			}
		}
		return false
	}
	waitOwner := func(router bool) {
		t.Helper()
		until(t, 8*time.Second, func() bool {
			s := snapshot()
			addresses, err := handle.AddrList(link, unix.AF_UNSPEC)
			if err != nil {
				t.Fatal(err)
			}
			peerOwns := false
			for _, address := range addresses {
				peerOwns = peerOwns || address.IP.String() == vip
			}
			if router {
				return s.State == vrrp.StateActive && s.Active && hasVIP() && !peerOwns
			}
			return s.State == vrrp.StateBackup && !s.Active && !hasVIP() && peerOwns
		})
	}
	vrrpUnicastResponders(t, routerNS, suffix, vip, "R")
	vrrpUnicastResponders(t, peerNS, suffix, vip, "K")
	traffic := func(ns netns.NsHandle, want string) {
		t.Helper()
		for _, protocol := range []string{"tcp", "udp"} {
			var conn net.Conn
			var err error
			port := "18093"
			if protocol == "udp" {
				port = "18094"
			}
			if err := fixtureInsideNamespace(ns, func() error {
				var err error
				conn, err = net.DialTimeout(protocol+suffix, net.JoinHostPort(vip, port), time.Second)
				return err
			}); err != nil {
				neighbors, _ := netlink.NeighList(0, unix.AF_UNSPEC)
				peerNeighbors, _ := handle.NeighList(0, unix.AF_UNSPEC)
				t.Log("neighbor state", neighbors, peerNeighbors)
				capture.mu.Lock()
				for _, packet := range capture.packets {
					b := packet.data
					if len(b) >= 55 && b[12] == 0x86 && b[13] == 0xdd && b[20] == 58 {
						t.Logf("ICMPv6 frame %x", b)
					}
				}
				capture.mu.Unlock()
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if protocol == "udp" {
				_, err = conn.Write([]byte("path"))
			}
			var response [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, response[:])
			}
			conn.Close()
			if err != nil || string(response[:]) != want {
				t.Fatalf("%s VIP got %q, want %s: %v", protocol, response, want, err)
			}
		}
	}
	dir := t.TempDir()
	peerLog := filepath.Join(dir, "keepalived.log")
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(peerLog)
			t.Logf("keepalived: %s", b)
		}
	})
	startPeer := func(priority int) func() {
		t.Helper()
		cfg := fmt.Sprintf("global_defs {\n router_id ROUTERD2_TEST\n}\nvrrp_instance TEST {\n state BACKUP\n interface peer0\n virtual_router_id 42\n priority %d\n advert_int 0.2\n version 3\n v3_checksum_as_v2\n unicast_src_ip %s\n unicast_peer {\n %s\n }\n check_unicast_src\n unicast_ttl 255\n accept\n virtual_ipaddress {\n", priority, peer, local)
		for _, address := range vips {
			cfg += " " + address + " dev peer0\n"
		}
		cfg += " }\n}\n"
		path := filepath.Join(dir, "keepalived.conf")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		log, err := os.OpenFile(peerLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(keepalived, "--dont-fork", "--dont-respawn", "--log-console", "--no-syslog", "--vrrp", "--use-file="+path, "--pid="+filepath.Join(dir, "parent.pid"), "--vrrp_pid="+filepath.Join(dir, "vrrp.pid"))
		cmd.Stdout, cmd.Stderr = log, log
		if err := fixtureInsideNamespace(peerNS, cmd.Start); err != nil {
			log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					t.Error("keepalived exit", err)
				}
			case <-time.After(4 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("keepalived failed to stop")
			}
			log.Close()
		}
		t.Cleanup(stop)
		return stop
	}
	waitOwner(true)
	if suffix == "6" {
		// Counterpart and router IPv6 link-local DAD must complete before a
		// one-second client deadline can measure steady-state VIP forwarding.
		until(t, 4*time.Second, func() bool {
			localLink, err := netlink.LinkByName("lan0")
			if err != nil {
				t.Fatal(err)
			}
			localAddresses, err := netlink.AddrList(localLink, unix.AF_INET6)
			if err != nil {
				t.Fatal(err)
			}
			peerAddresses, err := handle.AddrList(link, unix.AF_INET6)
			if err != nil {
				t.Fatal(err)
			}
			ready := func(addresses []netlink.Addr) bool {
				for _, address := range addresses {
					if address.IP.IsLinkLocalUnicast() && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
						return true
					}
				}
				return false
			}
			return ready(localAddresses) && ready(peerAddresses)
		})
	}
	traffic(peerNS, "R")
	if tracking {
		module := func(name string) supervisor.ModuleStatus {
			t.Helper()
			var status supervisor.Status
			if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
				t.Fatal(err)
			}
			for _, module := range status.Modules {
				if module.Name == name {
					return module
				}
			}
			t.Fatal("missing module", name)
			return supervisor.ModuleStatus{}
		}
		native, source := module("service.ha"), module("service.probe")
		startPeer(120)
		waitTrack := func(known, penalized bool, priority int) {
			t.Helper()
			until(t, 6*time.Second, func() bool {
				s := snapshot()
				return s.Priority == priority && len(s.Track) == 1 && s.Track[0].Known == known && s.Track[0].Penalized == penalized
			})
		}
		waitTrack(true, false, 150)
		waitOwner(true)
		probe.Close()
		waitTrack(true, true, 50)
		waitOwner(false)
		if s := snapshot(); s.Track[0].UnhealthyCount < 3 {
			t.Fatal("penalty before three failed probes", s.Track)
		}
		traffic(routerNS, "K")
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, "R")
		if err := syscall.Kill(source.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(source.PID, syscall.SIGCONT) })
		waitTrack(false, true, 50)
		paused := module("service.probe")
		if paused.Generation != source.Generation || paused.PID != source.PID || !paused.Ready {
			t.Fatal("test did not expire the sample before supervisor replacement", paused)
		}
		waitOwner(false)
		traffic(routerNS, "K")
		if err := syscall.Kill(source.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, "R")
		probe.Close()
		if err := syscall.Kill(source.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		until(t, 6*time.Second, func() bool {
			replacement := module("service.probe")
			return replacement.Ready && replacement.Generation > source.Generation
		})
		waitTrack(true, true, 50)
		waitOwner(false)
		traffic(routerNS, "K")
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, "R")
		current := module("service.ha")
		if current.PID != native.PID || current.Generation != native.Generation {
			t.Fatal("tracking restarted the election worker", native, current)
		}
		t.Log("Health failure/recovery, original sample expiry and Health SIGKILL/rebind changed priority 150/50 without restarting VRRP")
	} else {
		stopPeer := startPeer(200)
		waitOwner(false)
		traffic(routerNS, "K")
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == "service.ha" {
				if err := syscall.Kill(module.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				until(t, 6*time.Second, func() bool {
					var current supervisor.Status
					if f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &current) != nil {
						return false
					}
					for _, replacement := range current.Modules {
						if replacement.Name == module.Name {
							return replacement.Ready && replacement.Generation > module.Generation
						}
					}
					return false
				})
			}
		}
		waitOwner(false)
		traffic(routerNS, "K")
		stopPeer()
		waitOwner(true)
		traffic(peerNS, "R")
		startPeer(100)
		time.Sleep(time.Second) // More than a complete peer master-down interval.
		waitOwner(true)
		traffic(peerNS, "R")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	counts := map[string]int{}
	priorities := map[byte]int{}
	for _, packet := range capture.packets {
		b := packet.data
		var source, destination netip.Addr
		var hop byte
		var payload []byte
		if suffix == "4" && len(b) >= 34 && binary.BigEndian.Uint16(b[12:14]) == 0x0800 && b[23] == 112 {
			source, destination, hop = netip.AddrFrom4([4]byte(b[26:30])), netip.AddrFrom4([4]byte(b[30:34])), b[22]
			payload = b[14+int(b[14]&15)*4:]
		} else if suffix == "6" && len(b) >= 54 && binary.BigEndian.Uint16(b[12:14]) == 0x86dd && b[20] == 112 {
			source, destination, hop = netip.AddrFrom16([16]byte(b[22:38])), netip.AddrFrom16([16]byte(b[38:54])), b[21]
			payload = b[54:]
		} else {
			continue
		}
		if destination.IsMulticast() || hop != 255 || source.String() != local && source.String() != peer || destination.String() != local && destination.String() != peer {
			t.Fatal("unexpected unicast VRRP packet", source, destination, hop)
		}
		counts[source.String()]++
		if source.String() == local && len(payload) >= 3 {
			priorities[payload[2]]++
		}
	}
	if counts[local] < 2 || counts[peer] < 2 || capture.err != nil {
		t.Fatal("bidirectional unicast packet capture incomplete", counts, capture.err)
	}
	if tracking {
		if priorities[50] == 0 || priorities[150] == 0 || priorities[0] != 0 {
			t.Fatal("tracking wire priorities", priorities)
		}
		t.Logf("Health tracking keepalived interoperability: unicast adverts=%v, native priorities=%v, VIP TCP/UDP on both owners", counts, priorities)
		return
	}
	t.Logf("%s keepalived interoperability: unicast adverts=%v, VIP TCP/UDP, higher-priority takeover, SIGKILL/rebind, priority-zero recovery and lower-priority preemption passed", family, counts)
}

func vrrpUnicastResponders(t *testing.T, ns netns.NsHandle, suffix, vip, response string) {
	t.Helper()
	var tcp net.Listener
	var udp net.PacketConn
	if err := fixtureInsideNamespace(ns, func() error {
		var err error
		tcp, err = net.Listen("tcp"+suffix, ":18093")
		if err != nil {
			return err
		}
		listen := net.ListenConfig{Control: func(_, _ string, conn syscall.RawConn) error {
			var socketErr error
			if err := conn.Control(func(fd uintptr) {
				if suffix == "4" {
					socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
				} else {
					socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_FREEBIND, 1)
				}
			}); err != nil {
				return err
			}
			return socketErr
		}}
		udp, err = listen.ListenPacket(context.Background(), "udp"+suffix, net.JoinHostPort(vip, "18094"))
		return err
	}); err != nil {
		if tcp != nil {
			tcp.Close()
		}
		t.Fatal(err)
	}
	tcpDone, udpDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(tcpDone)
		for {
			c, err := tcp.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(time.Second))
			_, _ = c.Write([]byte(response))
			c.Close()
		}
	}()
	go func() {
		defer close(udpDone)
		var b [64]byte
		for {
			_, peer, err := udp.ReadFrom(b[:])
			if err != nil {
				return
			}
			_, _ = udp.WriteTo([]byte(response), peer)
		}
	}()
	t.Cleanup(func() { tcp.Close(); udp.Close(); <-tcpDone; <-udpDone })
}
