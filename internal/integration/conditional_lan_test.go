//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/supervisor"
	mdns "github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

//go:embed testdata/conditional-lan.yaml
var conditionalLAN []byte

//go:embed testdata/conditional-lan-policy.yaml
var conditionalLANPolicy []byte

func TestDaemonImportedConditionalLAN(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonImportedConditionalLAN", 90*time.Second) {
		return
	}
	provider := fixturePeerNamespace(t, "wan0", "server0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	if err := fixtureInsideNamespace(provider, func() error {
		for name, addresses := range map[string][]string{"server0": {"2001:db8:ffff::1/64", "fe80::2/64"}, "lo": {"2001:db8:ffff::9/128"}} {
			link, err := netlink.LinkByName(name)
			if err != nil {
				return err
			}
			for _, value := range addresses {
				a, _ := netlink.ParseAddr(value)
				if err := netlink.AddrAdd(link, a); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixtureInsideNamespace(client, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		a, _ := netlink.ParseAddr("192.0.2.3/24")
		if err := netlink.AddrAdd(link, a); err != nil {
			return err
		}
		for name, value := range map[string]string{"accept_ra": "2", "autoconf": "1", "forwarding": "0"} {
			if err := os.WriteFile("/proc/sys/net/ipv6/conf/client0/"+name, []byte(value), 0600); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, provider, "6", []string{"2001:db8:ffff::9"}, "A")
	capture := healthPBRCapture(t, "lan0")
	_ = healthPBRCapture(t, "wan0")
	work := t.TempDir()
	source, policy := filepath.Join(work, "legacy.yaml"), filepath.Join(work, "policy.yaml")
	if err := os.WriteFile(source, conditionalLAN, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, conditionalLANPolicy, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", policy, source)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	migrated, err := command.Output()
	if err != nil {
		t.Fatal(err, diagnostics.String())
	}
	var document config.Document
	if err := config.Decode(bytes.NewReader(migrated), &document); err != nil {
		t.Fatal(err)
	}
	document.Resources = append(document.Resources,
		resourceprogram.Resource{ID: "server-route", Route: &network.Route{Destination: "2001:db8:ffff::9/128", Interface: "wan0", Gateway: "fe80::2"}},
	)
	var election, advertisement, pd string
	for _, resource := range document.Resources {
		if resource.Service == nil {
			continue
		}
		s := resource.Service
		switch s.Role() {
		case "vrrp":
			election = s.ModuleName()
		case "ra":
			advertisement = s.ModuleName()
		case "dhcp6":
			pd = s.ModuleName()
		}
	}
	f := startFixtureDaemon(t, document)
	// Configure the independent ISP route from the product's actual link-local.
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	var gateway net.IP
	until(t, 3*time.Second, func() bool {
		addresses, err := netlink.AddrList(wan, netlink.FAMILY_V6)
		if err != nil {
			return false
		}
		for _, a := range addresses {
			if a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 {
				gateway = a.IP
				return true
			}
		}
		return false
	})
	if err := fixtureInsideNamespace(provider, func() error {
		link, err := netlink.LinkByName("server0")
		if err != nil {
			return err
		}
		_, prefix, _ := net.ParseCIDR("2001:db8:80::/56")
		return netlink.RouteAdd(&netlink.Route{LinkIndex: link.Attrs().Index, Dst: prefix, Gw: gateway})
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	start := func(namespace netns.NsHandle, name, path string, args ...string) func() {
		t.Helper()
		logPath := filepath.Join(work, name+".log")
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		owner, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(owner, path, args...)
		// Peer daemons must remove their own VIP/sockets on normal termination.
		cmd.Cancel = func() error { return cmd.Process.Signal(unix.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second
		cmd.Stdout, cmd.Stderr = log, log
		if err := fixtureInsideNamespace(namespace, cmd.Start); err != nil {
			cancel()
			log.Close()
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		stop := func() { cancel(); <-done }
		t.Cleanup(func() {
			stop()
			log.Close()
			if t.Failed() {
				b, _ := os.ReadFile(logPath)
				t.Logf("%s: %s", name, b)
			}
		})
		return stop
	}
	cf, leases := filepath.Join(work, "dhcp6.conf"), filepath.Join(work, "dhcp6.leases")
	if err := os.WriteFile(cf, []byte("default-lease-time 60; preferred-lifetime 40; option dhcp-renewal-time 20; option dhcp-rebinding-time 40; option dhcp6.name-servers 2001:db8:ffff::9; subnet6 2001:db8:ffff::/64 { prefix6 2001:db8:80:: 2001:db8:80:: /60; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leases, nil, 0600); err != nil {
		t.Fatal(err)
	}
	dnsmasq, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Fatal(err)
	}
	start(provider, "dnsmasq", dnsmasq, "--no-daemon", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-interfaces", "--listen-address=2001:db8:ffff::9", "--log-queries", "--log-facility=-", "--host-record=peer.provider.test,2001:db8:ffff::9", "--host-record=destination.learned.test,2001:db8:ffff::9", "--host-record=destination.boot.test,2001:db8:ffff::9")
	iscArgs := []string{"-6", "-f", "-d", "-cf", cf, "-lf", leases, "-pf", filepath.Join(work, "dhcp6.pid"), "server0"}
	stopISC := start(provider, "isc", os.Getenv("ROUTERD2_DHCP6_SERVER"), iscArgs...)
	role := func() string {
		var s runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+election, nil, &s); err != nil {
			t.Fatal(err)
		}
		var value vrrp.Snapshot
		_ = json.Unmarshal(s.Snapshot, &value)
		return value.State
	}
	communication := func() {
		t.Helper()
		var source string
		until(t, 10*time.Second, func() bool {
			// Election and kernel VIP publication precede DNS subscription delivery.
			// Observe the actual bound UDP socket before issuing the client query.
			bound, err := exec.Command("ss", "-H", "-lnu", "src 192.0.2.254 and sport = :53").CombinedOutput()
			if err != nil {
				t.Fatalf("inspect DNS sockets: %v: %s", err, bound)
			}
			if len(bytes.TrimSpace(bound)) == 0 {
				return false
			}
			var routes []netlink.Route
			var addresses []netlink.Addr
			if err = fixtureInsideNamespace(client, func() error {
				routes, err = netlink.RouteList(nil, netlink.FAMILY_V6)
				if err != nil {
					return err
				}
				addresses, err = netlink.AddrList(nil, netlink.FAMILY_V6)
				return err
			}); err != nil {
				if errors.Is(err, netlink.ErrDumpInterrupted) {
					return false
				}
				t.Fatal(err)
			}
			for _, a := range addresses {
				if a.IP.IsGlobalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0 {
					source = a.IP.String()
					break
				}
			}
			for _, route := range routes {
				if route.Gw != nil && (route.Dst == nil || route.Dst.String() == "::/0") && source != "" {
					return true
				}
			}
			return false
		})
		got := fixtureDHCP6Information(t, client, "client0")
		if len(got.dns) != 1 {
			t.Fatal("client did not acquire the router DNS", got)
		}
		if err := fixtureInsideNamespace(client, func() error {
			for _, transport := range []string{"+notcp", "+tcp"} {
				for _, domain := range []string{"home.test", "learned.test", "boot.test"} {
					out, err := exec.Command("dig", "-6", "@"+got.dns[0].String(), "destination."+domain, "AAAA", "+short", "+time=2", "+tries=1", transport).CombinedOutput()
					if err != nil || !strings.Contains(string(out), "2001:db8:ffff::9") {
						return fmt.Errorf("acquired DNS %s: %v: %s", domain, err, out)
					}
				}
			}
			out, err := exec.Command("dig", "-4", "@192.0.2.254", "destination.home.test", "AAAA", "+short", "+time=2", "+tries=1").CombinedOutput()
			if err != nil || !strings.Contains(string(out), "2001:db8:ffff::9") {
				return fmt.Errorf("virtual DNS listener: %v: %s", err, out)
			}
			fixtureDHCP6Traffic(t, source)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	until(t, 8*time.Second, func() bool { return role() == vrrp.StateActive })
	communication()
	var input ra.Config
	if err := f.control.Do(ctx, "GET", "/v1/services/"+advertisement+"/ra", nil, &input); err != nil {
		t.Fatal(err)
	}
	if input.RouterLifetimeSeconds != 7200 || input.RouterPreference != "high" || input.MTU != 1500 || input.ActiveUntil.Nanos == 0 {
		t.Fatal("lost advertisement intent or condition authority", input)
	}
	capture.mu.Lock()
	positiveLifetime := uint16(0)
	for _, p := range capture.packets {
		if b := p.data; len(b) >= 70 && b[54] == 134 {
			positiveLifetime = max(positiveLifetime, binary.BigEndian.Uint16(b[60:]))
		}
	}
	capture.mu.Unlock()
	if positiveLifetime <= 1 {
		t.Fatal("activation budget leaked into advertised router lifetime", positiveLifetime)
	}
	// An actual higher-priority implementation changes the role while PD remains bound.
	keepalived, err := exec.LookPath("keepalived")
	if err != nil {
		t.Fatal(err)
	}
	keepConfig := filepath.Join(work, "keepalived.conf")
	if err := os.WriteFile(keepConfig, []byte("global_defs {\n router_id LAN_PEER\n}\nvrrp_instance LAN {\n state BACKUP\n interface client0\n virtual_router_id 42\n priority 200\n advert_int 1\n version 2\n virtual_ipaddress {\n 192.0.2.254/24\n }\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stopPeer := start(client, "keepalived", keepalived, "--dont-fork", "--dont-respawn", "--log-console", "--no-syslog", "--vrrp", "--use-file="+keepConfig, "--pid="+filepath.Join(work, "keep.pid"), "--vrrp_pid="+filepath.Join(work, "vrrp.pid"))
	until(t, 8*time.Second, func() bool { return role() == vrrp.StateBackup })
	until(t, 5*time.Second, func() bool {
		var status supervisor.Status
		if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			return false
		}
		for _, m := range status.Modules {
			if m.Name == advertisement || m.Role == "dhcp6_server" {
				return false
			}
		}
		return true
	})
	var delegation runtimeplan.InstanceStatus
	if err := f.control.Do(ctx, "GET", "/v1/services/"+pd, nil, &delegation); err != nil {
		t.Fatal(err)
	}
	var bound dhcp6config.Snapshot
	if err := json.Unmarshal(delegation.Snapshot, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.State != "bound" || len(bound.Prefixes) == 0 {
		t.Fatal("role change lost independent PD", bound)
	}
	capture.mu.Lock()
	withdrawn := false
	for _, packet := range capture.packets {
		b := packet.data
		if len(b) >= 70 && b[54] == 134 && binary.BigEndian.Uint16(b[60:]) == 0 {
			withdrawn = true
		}
	}
	capture.mu.Unlock()
	if !withdrawn {
		t.Fatal("role change did not withdraw RA")
	}
	stopPeer()
	until(t, 8*time.Second, func() bool { return role() == vrrp.StateActive })
	communication()
	// Stop only control. The RA worker must withdraw under its original grant,
	// while the independently acquired PD remains valid much longer.
	var status supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	var controller int
	for _, module := range status.Modules {
		if module.Name == "control" {
			controller = module.PID
		}
	}
	func() {
		if err := unix.Kill(controller, unix.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		defer unix.Kill(controller, unix.SIGCONT)
		stopped := time.Now()
		capture.mu.Lock()
		first := len(capture.packets)
		capture.mu.Unlock()
		until(t, 2*time.Second, func() bool {
			capture.mu.Lock()
			defer capture.mu.Unlock()
			for _, p := range capture.packets[first:] {
				b := p.data
				if len(b) >= 70 && b[54] == 134 && binary.BigEndian.Uint16(b[60:]) == 0 {
					return true
				}
			}
			return false
		})
		time.Sleep(time.Until(stopped.Add(1500 * time.Millisecond)))
		capture.mu.Lock()
		defer capture.mu.Unlock()
		for _, p := range capture.packets[first:] {
			b := p.data
			if len(b) >= 70 && b[54] == 134 && p.at.After(stopped.Add(time.Second)) && binary.BigEndian.Uint16(b[60:]) != 0 {
				t.Fatal("RA advertised after stopped control's authority expired")
			}
		}
	}()
	communication()
	// Keep a real TCP DNS connection to the interface-derived IPv4 listener
	// through PD loss and recovery. The fixed local zone remains useful.
	var connection net.Conn
	if err := fixtureInsideNamespace(client, func() error {
		var err error
		connection, err = net.DialTimeout("tcp4", "192.0.2.2:53", time.Second)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	dnsTCP := &mdns.Conn{Conn: connection}
	localQuery := func() {
		t.Helper()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		question := new(mdns.Msg).SetQuestion("destination.home.test.", mdns.TypeAAAA)
		if err := dnsTCP.WriteMsg(question); err != nil {
			t.Fatal(err)
		}
		reply, err := dnsTCP.ReadMsg()
		if err != nil {
			t.Fatal("fixed DNS TCP connection was lost", err)
		}
		if reply.Id != question.Id || len(reply.Answer) != 1 {
			t.Fatal("fixed local zone failed", reply)
		}
	}
	localQuery()
	stopISC()
	until(t, 65*time.Second, func() bool {
		localQuery()
		var state runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+pd, nil, &state); err != nil {
			t.Fatal(err)
		}
		var current dhcp6config.Snapshot
		if err := json.Unmarshal(state.Snapshot, &current); err != nil {
			t.Fatal(err)
		}
		if len(current.Prefixes) == 0 {
			return true
		}
		// Sample liveness without exhausting the normal128-query TCP budget
		// before this60-second lease expires.
		time.Sleep(500 * time.Millisecond)
		return false
	})
	for _, domain := range []string{"learned.test", "boot.test"} {
		if err := fixtureInsideNamespace(client, func() error {
			out, err := exec.Command("dig", "-4", "@192.0.2.2", "destination."+domain, "AAAA", "+time=2", "+tries=1").CombinedOutput()
			if err != nil || !strings.Contains(string(out), "status: SERVFAIL") {
				return fmt.Errorf("expired learned DNS %s: %v: %s", domain, err, out)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	localQuery()
	start(provider, "isc-recovered", os.Getenv("ROUTERD2_DHCP6_SERVER"), iscArgs...)
	// Await WAN reacquisition, not a client route left from before withdrawal.
	// The one Information-request client then starts while LAN DAD may be pending.
	until(t, 10*time.Second, func() bool {
		localQuery()
		var state runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+pd, nil, &state); err != nil {
			t.Fatal(err)
		}
		var current dhcp6config.Snapshot
		if err := json.Unmarshal(state.Snapshot, &current); err != nil {
			t.Fatal(err)
		}
		if len(current.Prefixes) > 0 {
			return true
		}
		time.Sleep(500 * time.Millisecond)
		return false
	})
	communication()
	localQuery()
	f.stop()
	t.Log("legacy import: independent ISC/keepalived/Linux client acquired SLAAC/default/DNS, used UDP/TCP DNS and IPv6 TCP/UDP, withdrew and restored serving on role change and original control authority expiry; configured7200s router lifetime kept distinct from1s activation; imported dynamic listen addresses, learned forwarding/bootstrap, original PD loss/reacquisition and the same fixed DNS TCP connection")
}
