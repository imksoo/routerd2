//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

//go:embed testdata/vrrp-import-*.yaml
var legacyVRRP embed.FS

func TestDaemonVRRPUnicastIPv4(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPUnicastIPv4") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4"})
	}
}

func TestDaemonVRRPRFC9568IPv4(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPRFC9568IPv4") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", checksum: vrrp.ChecksumRFC9568})
	}
}

func TestDaemonVRRPUnicastIPv6(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPUnicastIPv6") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv6", migrate: true})
	}
}

func TestDaemonVRRPMulticastIPv6(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPMulticastIPv6") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv6", multicast: true})
	}
}

func TestDaemonVRRPSlowInitialACK(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPSlowInitialACK") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv6", slowACK: true})
	}
}

func TestDaemonVRRPHealthTracking(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPHealthTracking") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", tracking: true})
	}
}

func TestDaemonVRRPv2(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPv2") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", version: 2, multicast: true})
	}
}

func TestDaemonVRRPv2PASS(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPv2PASS") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", version: 2, password: "labpass", migrate: true})
	}
}

func TestDaemonVRRPAdditionalIdentity(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPAdditionalIdentity") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", wan: true})
	}
}

func TestDaemonVRRPMigratedAdditionalIdentity(t *testing.T) {
	if isolateFixture(t, "TestDaemonVRRPMigratedAdditionalIdentity") {
		runVRRPPeer(t, vrrpPeerTest{family: "ipv4", version: 2, wan: true, migrate: true})
	}
}

type vrrpPeerTest struct {
	migrate   bool
	checksum  vrrp.IPv4Checksum
	wan       bool
	family    string
	version   int
	password  string
	tracking  bool
	multicast bool
	slowACK   bool
}

func runVRRPPeer(t *testing.T, spec vrrpPeerTest) {
	family, tracking := spec.family, spec.tracking
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
	source, peerSource := local, peer
	if family == "ipv6" && spec.multicast {
		source, peerSource = "fe80::2", "fe80::3"
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
	if err := handle.AddrAdd(link, address); err != nil {
		t.Fatal(err)
	}
	if peerSource != peer {
		address, _ := netlink.ParseAddr(peerSource + "/64")
		if err := handle.AddrAdd(link, address); err != nil {
			t.Fatal(err)
		}
	}
	capture := healthPBRCapture(t, "lan0")
	c := vrrp.Config{Version: spec.version, AuthPassword: spec.password, Interface: "lan0", VIPInterface: "ha", Family: family, SourceAddress: local, UnicastPeers: []netip.Addr{netip.MustParseAddr(peer)}, VirtualRouterID: 42, Priority: 150, AdvertIntervalCentiseconds: 20, Addresses: vips, AcceptMode: true}
	if c.Version == 2 {
		c.AdvertIntervalCentiseconds = 100
	}
	if family == "ipv4" && c.Version != 2 {
		c.IPv4Checksum = spec.checksum
		if c.IPv4Checksum == "" {
			c.IPv4Checksum = vrrp.ChecksumRFC5798
		}
	}
	if spec.multicast {
		c.UnicastPeers = nil
		c.SourceAddress = source
	}
	c = vrrp.Normalize(c)
	iface := network.Interface{Name: "lan0", Addresses: []string{local + "/" + strconv.Itoa(bits)}}
	if family == "ipv6" {
		if source != local {
			iface.Addresses = append(iface.Addresses, source+"/64")
		}
	}
	var probe, trackingGate net.Listener
	services := []serviceconfig.Service{{Name: "ha", VRRP: &c}}
	if tracking {
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		trackingGate = healthPBRListener(t, peerNS, peer+":18098", "G")
		c.Track = []vrrp.Track{{Resource: "service/probe", Penalty: 100, HealthyThreshold: 2, UnhealthyThreshold: 3, MaxAgeMillis: 1500}}
		services = append(services, serviceconfig.Service{Name: "probe", Health: &health.Config{Interface: "lan0", SourceAddress: local, Target: peer, Protocol: "tcp", Port: 18097, IntervalMillis: 300, TimeoutMillis: 50, HealthyThreshold: 1, UnhealthyThreshold: 1}})
	}
	program := config.FromNative(config.Native{Network: network.Desired{Interfaces: []network.Interface{iface}}, Services: services})
	haName := "service.ha"
	if spec.migrate {
		var imported serviceconfig.Service
		profile := family
		if spec.wan {
			profile = "wan"
		}
		program, imported = importedVRRPProgram(t, profile)
		c, haName = imported.VRRPConfig(), imported.ModuleName()
	}
	if tracking {
		for i := range program.Resources {
			if program.Resources[i].ID == "service/probe" {
				program.Resources[i].When = &resourceprogram.FactCondition{Fact: "service.track-gate.active"}
			}
		}
		program.Resources = append(program.Resources, resourceprogram.Resource{ID: "track-gate", Service: &serviceconfig.Service{Name: "track-gate", Health: &health.Config{Interface: "lan0", SourceAddress: local, Target: peer, Protocol: "tcp", Port: 18098, IntervalMillis: 200, TimeoutMillis: 50, HealthyThreshold: 1, UnhealthyThreshold: 1}}})
	}

	var wan *vrrpWANTest
	var pdPeer *exec.Cmd
	if spec.wan {
		wan = setupVRRPWAN(t, peerNS)
		if !spec.migrate {
			program.Resources = append(program.Resources,
				resourceprogram.Resource{ID: "wan-identity", When: &resourceprogram.FactCondition{Fact: haName + ".elected"}, Link: &network.Link{Name: "wan-ha", Kind: "macvlan", Parent: "wan0", HardwareAddr: vrrpWANMAC, IPv6AddrGenMode: "none"}},
				resourceprogram.Resource{ID: "wan-interface", Interface: &network.Interface{Name: "wan-ha"}},
				resourceprogram.Resource{ID: "wan-address", Address: &resourceprogram.Address{Interface: "wan-ha", CIDR: vrrpWANVIP + "/24"}},
				resourceprogram.Resource{ID: "wan-linklocal", Address: &resourceprogram.Address{Interface: "wan-ha", CIDR: vrrpWANLL + "/64", Options: &network.AddressOptions{NoDAD: true}}},
			)
		}
		program.Resources = append(program.Resources, resourceprogram.Resource{ID: "wan-ra", When: &resourceprogram.FactCondition{Fact: haName + ".active"}, Service: &serviceconfig.Service{Name: "wan-ra", RA: &ra.Config{Interface: "wan-ha", SourceAddress: vrrpWANLL, MinIntervalSeconds: 3, MaxIntervalSeconds: 4, RouterLifetimeSeconds: 12}}})
		if spec.migrate {
			pdPeer = startVRRPPDPeer(t, wan.client)
			if err := pdPeer.Process.Signal(syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			healthPBRListener(t, wan.client, "198.51.100.10:18097", "H")
			healthPBRListener(t, peerNS, peer+":18098", "B")
			healthPBRListener(t, peerNS, peer+":18099", "E")
		}
	}
	f := startFixtureDaemon(t, program)
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
	snapshot := func() vrrp.Snapshot {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		if err := f.control.Do(context.Background(), "GET", "/v1/services/"+haName, nil, &instance); err != nil {
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
	var pdDUID string
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
		if spec.migrate && wan != nil {
			until(t, 8*time.Second, func() bool {
				var state supervisor.Status
				if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &state); err != nil {
					t.Fatal(err)
				}
				for _, resource := range program.Resources {
					if resource.Service == nil || resource.Service.DHCP6 == nil {
						continue
					}
					name := resource.Service.ModuleName()
					present := false
					for _, m := range state.Modules {
						present = present || m.Name == name
					}
					if !router {
						return !present
					}
					if !present {
						return false
					}
					var instance runtimeplan.InstanceStatus
					if err := f.control.Do(context.Background(), "GET", "/v1/services/"+name, nil, &instance); err != nil {
						t.Fatal(err)
					}
					var pd dhcp6config.Snapshot
					if err := json.Unmarshal(instance.Snapshot, &pd); err != nil || len(pd.Prefixes) != 1 || pd.State != "bound" || len(pd.DNSServers) != 1 || pd.DNSServers[0] != "2001:db8:42::1" {
						return false
					}
					if pdDUID == "" {
						pdDUID = pd.ClientDUID
					} else if pd.ClientDUID != pdDUID {
						t.Fatal("master recovery changed automatic DUID", pdDUID, pd.ClientDUID)
					}
					link, err := netlink.LinkByName("wan-ha")
					if err != nil {
						return false
					}
					addresses, err := netlink.AddrList(link, unix.AF_INET6)
					if errors.Is(err, netlink.ErrDumpInterrupted) {
						return false
					}
					if err != nil {
						t.Fatal(err)
					}
					for _, a := range addresses {
						if a.IP.String() == "2001:db8:80::21" && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
							return pd.IAID == 1
						}
					}
					return false
				}
				t.Fatal("missing imported PD declaration")
				return false
			})
			until(t, 6*time.Second, func() bool {
				link, err := netlink.LinkByName("lan-extra")
				if router {
					if err != nil || link.Attrs().HardwareAddr.String() != "02:00:5e:00:42:02" {
						return false
					}
					addresses, err := netlink.AddrList(link, unix.AF_INET6)
					if err != nil {
						t.Fatal(err)
					}
					ready := false
					for _, address := range addresses {
						ready = ready || address.IP.String() == "fe80::42:2" && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
					}
					if !ready {
						return false
					}
				} else if err == nil {
					return false
				}
				var status supervisor.Status
				if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
					t.Fatal(err)
				}
				for _, resource := range program.Resources {
					if resource.Service == nil || resource.Service.Health == nil {
						continue
					}
					service := resource.Service
					want := service.Health.Port == 18099 || service.Health.Port == 18097 && router || service.Health.Port == 18098 && !router
					present := false
					for _, m := range status.Modules {
						present = present || m.Name == service.ModuleName()
					}
					if present != want {
						return false
					}
					if want {
						var state runtimeplan.InstanceStatus
						if err := f.control.Do(context.Background(), "GET", "/v1/services/"+service.ModuleName(), nil, &state); err != nil {
							t.Fatal(err)
						}
						var probe health.Snapshot
						if json.Unmarshal(state.Snapshot, &probe) != nil || !probe.Active {
							return false
						}
					}
				}
				return true
			})
		}
	}
	targets := []string{vip}
	if wan != nil {
		targets = append(targets, vrrpWANVIP)
	}
	vrrpVIPResponders(t, routerNS, suffix, targets, "R")
	vrrpVIPResponders(t, peerNS, suffix, targets, "K")
	if spec.migrate && wan != nil {
		vrrpVIPResponders(t, routerNS, "6", []string{"2001:db8:80::21"}, "R")
	}
	traffic := func(ns netns.NsHandle, target, want string) {
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
				family := "4"
				if netip.MustParseAddr(target).Is6() {
					family = "6"
				}
				conn, err = net.DialTimeout(protocol+family, net.JoinHostPort(target, port), time.Second)
				return err
			}); err != nil {
				if wan != nil && target == vrrpWANVIP {
					wan.logPath(t)
				}
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
	startPeer := func(priority int, password string) func() {
		t.Helper()
		cfg := fmt.Sprintf("global_defs {\n router_id ROUTERD2_TEST\n}\nvrrp_instance TEST {\n state BACKUP\n interface peer0\n virtual_router_id 42\n priority %d\n advert_int %.2f\n version %d\n accept\n", priority, float64(c.AdvertIntervalCentiseconds)/100, c.Version)
		if !spec.multicast {
			cfg += fmt.Sprintf(" unicast_src_ip %s\n unicast_peer {\n %s\n }\n check_unicast_src\n unicast_ttl 255\n", peer, local)
		} else {
			cfg += " mcast_src_ip " + peerSource + "\n"
		}
		if c.IPv4Checksum == vrrp.ChecksumRFC9568 {
			cfg += " v3_checksum_as_v2\n"
		}
		if c.Version == 2 {
			// RFC 3768 discards inferior adverts. Keepalived's default read path
			// resets its send timer even for wrong-auth packets unless this is set.
			cfg += " lower_prio_no_advert\n"
		}
		if password != "" {
			cfg += " authentication {\n auth_type PASS\n auth_pass " + password + "\n }\n"
		}
		if wan != nil {
			cfg += " garp_extra_if all 1\n"
			cfg += " virtual_ipaddress_excluded {\n " + vrrpWANVIP + "/24 dev peer-ha\n " + vrrpWANLL + "/64 dev peer-ha\n }\n"
		}
		cfg += " virtual_ipaddress {\n"
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
	if spec.slowACK {
		writer, election := module("network"), module(haName)
		if snapshot().State != vrrp.StateBackup {
			t.Fatal("fixture missed initial backup before writer pause")
		}
		if err := syscall.Kill(writer.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(writer.PID, syscall.SIGCONT) })
		var pending vrrp.Snapshot
		until(t, 2*time.Second, func() bool {
			pending = snapshot()
			return pending.State == vrrp.StateActive
		})
		time.Sleep(550 * time.Millisecond)
		clock, err := boottime.Read()
		if err != nil || uint64(clock.BoottimeNanoseconds) >= pending.ExpiresNanos {
			t.Fatal("fixture exhausted the original election before resuming writer", err)
		}
		t.Log("resuming writer", "pending election", pending, "age", time.Duration(uint64(clock.BoottimeNanoseconds)-pending.ObservedNanos), "remaining", time.Duration(pending.ExpiresNanos-uint64(clock.BoottimeNanoseconds)))
		if err := syscall.Kill(writer.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		waitOwner(true)
		until(t, 3*time.Second, func() bool {
			current := module(haName)
			if current.PID != election.PID || current.Generation != election.Generation {
				t.Fatal("delayed initial ACK replaced election worker", election, current)
			}
			state := snapshot()
			return state.Active && state.ObservedNanos > pending.ExpiresNanos && hasVIP()
		})
		t.Log("550 ms writer pause delayed the initial VIP ACK; same election renewed beyond its original deadline without extending authority")
	}
	if pdPeer != nil {
		until(t, 8*time.Second, func() bool {
			s := snapshot()
			return s.State == vrrp.StateActive && !s.Active && s.ActivationState == "ReadinessTimeout"
		})
		if hasVIP() {
			t.Fatal("PD admission timeout published the VIP")
		}
		wan.waitNative(t, true)
		if err := pdPeer.Process.Signal(syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		t.Log("elected WAN remained usable for PD acquisition while admission timeout withheld the LAN VIP")
	}
	waitOwner(true)
	if first := module(haName); first.Generation != 1 {
		t.Fatal("normal address DAD replaced the initial VRRP worker", first)
	}
	if wan != nil {
		wan.waitNative(t, true)
		wan.waitForwarder(t, true)
		until(t, 5*time.Second, func() bool { positive, _ := wan.advertisements(); return positive > 0 })
		traffic(wan.client, vrrpWANVIP, "R")
	}
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
	traffic(peerNS, vip, "R")
	if spec.migrate && wan != nil {
		traffic(wan.client, "2001:db8:80::21", "R")
		paused := module(haName)
		original := snapshot()
		clock, err := boottime.Read()
		if err != nil {
			t.Fatal(err)
		}
		budget := time.Duration(original.ExpiresNanos-uint64(clock.BoottimeNanoseconds)) + time.Second
		var lingering []string
		defer func() {
			if t.Failed() {
				t.Log("role expiry pending modules", lingering, "original", original)
			}
		}()
		if err := syscall.Kill(paused.PID, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(paused.PID, syscall.SIGCONT) })
		until(t, budget, func() bool {
			var status supervisor.Status
			if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
				t.Fatal(err)
			}
			lingering = lingering[:0]
			for _, m := range status.Modules {
				for _, resource := range program.Resources {
					if resource.Service != nil && (resource.Service.Health != nil || resource.Service.DHCP6 != nil) && m.Name == resource.Service.ModuleName() {
						// PD cleanup acknowledges withdrawal and can wait for
						// Release. It must already be stopping when its WAN
						// authority expires; cleanup cannot retain that link.
						if resource.Service.DHCP6 != nil && m.State == "stopping" && !m.Ready {
							continue
						}
						lingering = append(lingering, m.Name+":"+m.State)
					}
				}
			}
			link, err := netlink.LinkByName("wan-ha")
			return len(lingering) == 0 && (err != nil || link.Attrs().Flags&net.FlagUp == 0)
		})
		if current := module(haName); current.PID != paused.PID || current.Generation != paused.Generation {
			t.Fatal("unknown role was caused by replacement rather than original observation expiry", current, paused)
		}
		if err := syscall.Kill(paused.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		until(t, 8*time.Second, func() bool { return module(haName).Generation > paused.Generation })
		waitOwner(true)
		wan.waitNative(t, true)
		traffic(wan.client, vrrpWANVIP, "R")
		traffic(wan.client, "2001:db8:80::21", "R")
		t.Log("expired role stopped PD and role-scoped probes before producer replacement; fresh election restored communication with the same automatic DUID")
	}
	if spec.migrate && wan == nil {
		// An actual listener makes timeout prove the inherited zone's deny,
		// rather than an absent service or a blanket VMAC accept.
		var listener net.Listener
		if err := fixtureInsideNamespace(routerNS, func() error {
			var err error
			listener, err = net.Listen("tcp"+suffix, net.JoinHostPort(vip, "18095"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		if err := fixtureInsideNamespace(peerNS, func() error {
			conn, err := net.DialTimeout("tcp"+suffix, net.JoinHostPort(vip, "18095"), 300*time.Millisecond)
			if err == nil {
				conn.Close()
				t.Fatal("migration lost the original zone deny on the VIP VMAC")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if tracking {

		native, source := module(haName), module("service.probe")
		startPeer(120, c.AuthPassword)
		waitTrack := func(known, penalized bool, priority int) {
			t.Helper()
			until(t, 6*time.Second, func() bool {
				s := snapshot()
				return s.Priority == priority && len(s.Track) == 1 && (s.Track[0].State != vrrp.TrackUnknown) == known && s.Track[0].Penalized == penalized
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
		traffic(routerNS, vip, "K")
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, vip, "R")
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
		traffic(routerNS, vip, "K")
		if err := syscall.Kill(source.PID, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, vip, "R")
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
		traffic(routerNS, vip, "K")
		probe = healthPBRListener(t, peerNS, peer+":18097", "H")
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, vip, "R")
		current := module(haName)
		if current.PID != native.PID || current.Generation != native.Generation {
			t.Fatal("tracking restarted the election worker", native, current)
		}
		trackingGate.Close()
		until(t, 6*time.Second, func() bool {
			s := snapshot()
			return len(s.Track) == 1 && s.Track[0].State == vrrp.TrackNeutral && !s.Track[0].Penalized && s.Priority == 150
		})
		neutral := snapshot().Track[0]
		until(t, 6*time.Second, func() bool {
			var status supervisor.Status
			if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
				t.Fatal(err)
			}
			for _, module := range status.Modules {
				if module.Name == "service.probe" {
					return false
				}
			}
			return true
		})
		time.Sleep(600 * time.Millisecond)
		held := snapshot()
		if held.Track[0].State != vrrp.TrackNeutral || held.Track[0].HealthyCount != neutral.HealthyCount || held.Track[0].UnhealthyCount != neutral.UnhealthyCount || held.Track[0].Penalized || held.Priority != 150 {
			t.Fatal("excluded source changed counters or penalty", held.Track, neutral)
		}
		waitOwner(true)
		traffic(peerNS, vip, "R")
		trackingGate = healthPBRListener(t, peerNS, peer+":18098", "G")
		waitTrack(true, false, 150)
		waitOwner(true)
		traffic(peerNS, vip, "R")
		if current := module(haName); current.PID != native.PID || current.Generation != native.Generation {
			t.Fatal("conditional source exclusion restarted the election", current, native)
		}
		t.Log("Health failure/recovery and source replacement changed priority 150/50; conditional source exclusion preserved the finite neutral decision, counters, same election worker and VIP TCP/UDP")
	} else {
		if c.AuthPassword != "" {
			rejected := snapshot().Rejected
			stopWrong := startPeer(200, "wrongkey")
			tracePeer := func() {
				current := snapshot()
				localLink, _ := netlink.LinkByName("lan0")
				peers, _ := handle.NeighList(0, unix.AF_INET)
				t.Log("VRRP receive state", current.State, current.Received, current.Rejected, "physical MAC", localLink.Attrs().HardwareAddr, "peer neighbors", peers)
				capture.mu.Lock()
				defer capture.mu.Unlock()
				for _, packet := range capture.packets {
					b := packet.data
					if len(b) >= 34 && binary.BigEndian.Uint16(b[12:14]) == 0x0800 && b[23] == 112 && net.IP(b[26:30]).String() == peer {
						t.Logf("peer advertisement %s srcmac=%x dstmac=%x", packet.at.Format(time.RFC3339Nano), b[6:12], b[:6])
					}
				}
			}
			defer func() {
				if t.Failed() {
					tracePeer()
				}
			}()
			until(t, 8*time.Second, func() bool { return snapshot().Rejected >= rejected+2 })
			if s := snapshot(); s.State != vrrp.StateActive || !hasVIP() {
				t.Fatal("wrong-password peer displaced native owner", s)
			}
			traffic(routerNS, vip, "R")
			t.Log("wrong-password keepalived advertisements rejected while native VIP ownership and TCP/UDP remained active")
			stopWrong()
		}
		stopPeer := startPeer(200, c.AuthPassword)
		waitOwner(false)
		traffic(routerNS, vip, "K")
		if wan != nil {
			wan.waitNative(t, false)
			until(t, 2*time.Second, func() bool { _, zero := wan.advertisements(); return zero > 0 })
			wan.waitForwarder(t, false)
			traffic(wan.client, vrrpWANVIP, "K")
		}
		var status supervisor.Status
		if err := f.supervisor.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			t.Fatal(err)
		}
		for _, module := range status.Modules {
			if module.Name == haName {
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
		traffic(routerNS, vip, "K")
		stopPeer()
		waitOwner(true)
		traffic(peerNS, vip, "R")
		if wan != nil {
			wan.waitNative(t, true)
			wan.waitForwarder(t, true)
			traffic(wan.client, vrrpWANVIP, "R")
			if spec.migrate {
				traffic(wan.client, "2001:db8:80::21", "R")
			}
		}
		startPeer(100, c.AuthPassword)
		time.Sleep(4 * time.Duration(c.AdvertIntervalCentiseconds) * 10 * time.Millisecond) // More than a complete peer master-down interval.
		waitOwner(true)
		traffic(peerNS, vip, "R")
		if wan != nil {
			native, control, writer := module(haName), module("control"), module("network")
			original := snapshot()
			clock, err := boottime.Read()
			if err != nil {
				t.Fatal(err)
			}
			// The migrated v2 instance has one-second adverts. Wait for its
			// actual original authority, rather than assuming the 20cs fixture.
			budget := time.Duration(original.ExpiresNanos-uint64(clock.BoottimeNanoseconds)) + time.Second
			for _, process := range []supervisor.ModuleStatus{control, native} {
				if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = syscall.Kill(process.PID, syscall.SIGCONT) })
			}
			until(t, budget, func() bool {
				names := []string{"ha", "wan-ha"}
				if spec.migrate {
					names = append(names, "lan-extra")
				}
				for _, name := range names {
					link, err := netlink.LinkByName(name)
					if err != nil || link.Attrs().Flags&net.FlagUp != 0 {
						return false
					}
					addresses, err := netlink.AddrList(link, unix.AF_UNSPEC)
					if err != nil {
						t.Fatal(err)
					}
					for _, address := range addresses {
						if address.IP.String() == vip || address.IP.String() == vrrpWANVIP || address.IP.String() == vrrpWANLL {
							return false
						}
					}
				}
				return true
			})
			for _, before := range []supervisor.ModuleStatus{native, control, writer} {
				current := module(before.Name)
				if current.PID != before.PID || current.Generation != before.Generation || !current.Ready {
					t.Fatal("expiry was caused by process replacement instead of writer authority", before, current)
				}
			}
			until(t, 2*time.Second, func() bool { return wan.peerOwns(t) })
			wan.waitForwarder(t, false)
			traffic(wan.client, vrrpWANVIP, "K")
			for _, process := range []supervisor.ModuleStatus{control, native} {
				if err := syscall.Kill(process.PID, syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
			}
			until(t, 8*time.Second, func() bool { return module(haName).Generation > native.Generation })
			waitOwner(true)
			wan.waitNative(t, true)
			wan.waitForwarder(t, true)
			traffic(wan.client, vrrpWANVIP, "R")
			positive, zero := wan.advertisements()
			announcements := wan.announcements(t)
			if announcements < 2 {
				t.Fatal("missing finite MAC location announcements", announcements)
			}
			t.Logf("WAN MAC location announcements=%d", announcements)
			t.Logf("conditional WAN MAC/link-local identity, RA positive=%d withdrawal=%d, actual WAN TCP/UDP, control+VRRP pause expiry by surviving writer and fresh-election recovery passed", positive, zero)
		}
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
			payload = b[14+int(b[14]&15)*4 : 14+int(binary.BigEndian.Uint16(b[16:18]))]
		} else if suffix == "6" && len(b) >= 54 && binary.BigEndian.Uint16(b[12:14]) == 0x86dd && b[20] == 112 {
			source, destination, hop = netip.AddrFrom16([16]byte(b[22:38])), netip.AddrFrom16([16]byte(b[38:54])), b[21]
			payload = b[54 : 54+int(binary.BigEndian.Uint16(b[18:20]))]
		} else {
			continue
		}
		if len(payload) < 8 || payload[0] != byte(c.Version<<4)|1 {
			t.Fatal("wrong wire version", payload)
		}
		if hop != 255 || source.String() != c.SourceAddress && source.String() != peerSource {
			t.Fatal("unexpected VRRP source or hop", source, hop)
		}
		if spec.multicast {
			group := "224.0.0.18"
			if family == "ipv6" {
				group = "ff02::12"
			}
			if destination.String() != group {
				t.Fatal("unexpected VRRP multicast destination", destination)
			}
		} else if destination.IsMulticast() || destination.String() != local && destination.String() != peer {
			t.Fatal("unexpected unicast VRRP destination", destination)
		}
		if c.Version == 2 {
			if len(payload) != 16+4*int(payload[3]) || int(payload[5])*100 != c.AdvertIntervalCentiseconds {
				t.Fatal("invalid VRRPv2 capture layout")
			}
			if source.String() == c.SourceAddress {
				var auth [8]byte
				copy(auth[:], c.AuthPassword)
				kind := byte(0)
				if c.AuthPassword != "" {
					kind = 1
				}
				if payload[4] != kind || !bytes.Equal(payload[len(payload)-8:], auth[:]) {
					t.Fatal("native VRRPv2 authentication wire mismatch")
				}
			}
		}
		counts[source.String()]++
		if source.String() == c.SourceAddress && len(payload) >= 3 {
			priorities[payload[2]]++
		}
	}
	if counts[c.SourceAddress] < 2 || counts[peerSource] < 2 || capture.err != nil {
		t.Fatal("bidirectional VRRP packet capture incomplete", counts, capture.err)
	}
	if tracking {
		if priorities[50] == 0 || priorities[150] == 0 || priorities[0] != 0 {
			t.Fatal("tracking wire priorities", priorities)
		}
		t.Logf("Health tracking keepalived interoperability: unicast adverts=%v, native priorities=%v, VIP TCP/UDP on both owners", counts, priorities)
		return
	}
	t.Logf("%s VRRPv%d keepalived interoperability: adverts=%v, VIP TCP/UDP, higher-priority takeover, SIGKILL/rebind, priority-zero recovery and lower-priority preemption passed", family, c.Version, counts)
}

func importedVRRPProgram(t *testing.T, family string) (config.Document, serviceconfig.Service) {
	t.Helper()
	work := t.TempDir()
	for _, suffix := range []string{"", "-policy"} {
		name := "vrrp-import-" + family + suffix + ".yaml"
		data, err := legacyVRRP.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", filepath.Join(work, "vrrp-import-"+family+"-policy.yaml"), filepath.Join(work, "vrrp-import-"+family+".yaml"))
	var review bytes.Buffer
	cmd.Stderr = &review
	data, err := cmd.Output()
	if err != nil {
		t.Fatal(err, review.String())
	}
	program, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range program.Resources {
		if resource.Service != nil && resource.Service.VRRP != nil {
			return program, *resource.Service
		}
	}
	t.Fatal("CLI migration omitted VRRP")
	return config.Document{}, serviceconfig.Service{}
}

func vrrpVIPResponders(t *testing.T, ns netns.NsHandle, suffix string, vips []string, response string) {
	t.Helper()
	var tcp net.Listener
	var udp []net.PacketConn
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
		for _, vip := range vips {
			socket, err := listen.ListenPacket(context.Background(), "udp"+suffix, net.JoinHostPort(vip, "18094"))
			if err != nil {
				return err
			}
			udp = append(udp, socket)
		}
		return nil
	}); err != nil {
		if tcp != nil {
			tcp.Close()
		}
		for _, socket := range udp {
			socket.Close()
		}
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
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
	for _, socket := range udp {
		workers.Add(1)
		go func() {
			defer workers.Done()
			var b [64]byte
			for {
				_, peer, err := socket.ReadFrom(b[:])
				if err != nil {
					return
				}
				_, _ = socket.WriteTo([]byte(response), peer)
			}
		}()
	}
	t.Cleanup(func() {
		tcp.Close()
		for _, socket := range udp {
			socket.Close()
		}
		workers.Wait()
	})
}
