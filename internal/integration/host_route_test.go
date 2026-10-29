//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

//go:embed testdata/host-route.yaml
var legacyHostRoute []byte

//go:embed testdata/host-route-options.yaml
var legacyHostRouteOptions []byte

func TestDaemonImportedIPv6HostRoute(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonImportedIPv6HostRoute", 60*time.Second) {
		return
	}
	runHostRouteFixture(t, "static")
}

func TestDaemonRAHostRoute(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonRAHostRoute", 60*time.Second) {
		return
	}
	runHostRouteFixture(t, "learned")
}

func TestDaemonImplicitIPv6HostRoute(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonImplicitIPv6HostRoute", 60*time.Second) {
		return
	}
	runHostRouteFixture(t, "implicit")
}

func runHostRouteFixture(t *testing.T, mode string) {
	peerNS := fixturePeerNamespace(t, "wan0", "provider0")
	if err := fixtureInsideNamespace(peerNS, func() error {
		peer, err := netlink.LinkByName("provider0")
		if err != nil {
			return err
		}
		for _, cidr := range []string{"192.0.2.1/24", "fe80::2/64", "2001:db8:42::1/64", "2001:db8:ffff::9/128"} {
			addr, _ := netlink.ParseAddr(cidr)
			addr.Flags = unix.IFA_F_NODAD
			if err := netlink.AddrAdd(peer, addr); err != nil {
				return err
			}
		}
		return os.WriteFile("/proc/sys/net/ipv6/conf/all/forwarding", []byte("1"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	vrrpVIPResponders(t, peerNS, "6", []string{"2001:db8:ffff::9"}, "R")
	healthPBRListener(t, peerNS, "192.0.2.1:18080", "V")
	capture := healthPBRCapture(t, "wan0")
	work := t.TempDir()
	start := func(name, path string, args ...string) *exec.Cmd {
		t.Helper()
		log, err := os.Create(filepath.Join(work, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		owner, cancel := context.WithCancel(t.Context())
		cmd := exec.CommandContext(owner, path, args...)
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 2 * time.Second
		cmd.Stdout, cmd.Stderr = log, log
		if err := fixtureInsideNamespace(peerNS, cmd.Start); err != nil {
			cancel()
			log.Close()
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() {
			cancel()
			<-done
			log.Close()
			if t.Failed() {
				body, _ := os.ReadFile(filepath.Join(work, name+".log"))
				t.Logf("%s: %s", name, body)
			}
		})
		return cmd
	}
	radvdPath, err := exec.LookPath("radvd")
	if err != nil {
		t.Fatal(err)
	}
	radvdConfig := filepath.Join(work, "radvd.conf")
	advertisement := `interface provider0 {
 AdvSendAdvert on; MinRtrAdvInterval 3; MaxRtrAdvInterval 4; AdvDefaultLifetime 12;
 AdvRASrcAddress { fe80::2; };
 prefix 2001:db8:42::/64 { AdvOnLink on; AdvAutonomous on; AdvValidLifetime 12; AdvPreferredLifetime 4; };
 RDNSS 2001:db8:ffff::9 { AdvRDNSSLifetime 12; };
};`
	if err := os.WriteFile(radvdConfig, []byte(advertisement), 0600); err != nil {
		t.Fatal(err)
	}
	advertiser := start("radvd", radvdPath, "-n", "-m", "stderr", "-u", "root", "-C", radvdConfig, "-p", filepath.Join(work, "radvd.pid"))
	dnsmasqPath, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Fatal(err)
	}
	start("dnsmasq", dnsmasqPath, "--no-daemon", "--conf-file=/dev/null", "--no-hosts", "--no-resolv", "--bind-interfaces", "--listen-address=2001:db8:ffff::9", "--host-record=destination.provider.test,2001:db8:ffff::9", "--log-facility=-")
	input, options := filepath.Join(work, "legacy.yaml"), filepath.Join(work, "options.yaml")
	legacySource, policy := legacyHostRoute, legacyHostRouteOptions
	if mode == "implicit" {
		// This operator declares only the host path, without an RA resource/policy.
		// Its DNS upstream is static; the other modes retain learned RA DNS.
		legacySource = bytes.Replace(legacySource, []byte(`  - apiVersion: net.routerd.net/v1alpha1
    kind: IPv6RAAddress
    metadata:
      name: wan-ra
    spec:
      interface: wan
      managed: true
`), nil, 1)
		legacySource = bytes.Replace(legacySource, []byte(`      addressFrom:
      - resource: IPv6RAAddress/wan-ra
        field: dnsServers`), []byte(`      address: 2001:db8:ffff::9`), 1)
		policy = []byte("schema_version: 1\n")
	}
	if err := os.WriteFile(input, legacySource, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options, policy, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "migrate", "--policy", options, input)
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	body, err := command.Output()
	if err != nil {
		t.Fatal(err, diagnostics.String())
	}
	program, err := config.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "learned" {
		var source string
		for i := range program.Resources {
			if service := program.Resources[i].Service; service != nil && service.RAClient != nil {
				enabled := true
				service.RAClient.DefaultRoute = &enabled
				source = service.ModuleName()
			}
		}
		for i := range program.Resources {
			if host := program.Resources[i].HostRoute; host != nil {
				host.Gateway = ""
				host.RouterFrom = source
			}
		}
	}
	f := startFixtureDaemon(t, program)
	ctx := context.Background()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		var status runtimeplan.Status
		err := f.control.Do(ctx, "GET", "/v1/services", nil, &status)
		body, _ := json.Marshal(status)
		t.Logf("runtime: %s %v", body, err)
		routes, _ := netlink.RouteListFiltered(unix.AF_INET6, &netlink.Route{Table: 120}, netlink.RT_FILTER_TABLE)
		t.Logf("host routes: %+v", routes)
	})
	hostRoute := func() (network.Route, bool) {
		t.Helper()
		routes, err := netlink.RouteListFiltered(unix.AF_INET6, &netlink.Route{Table: 120}, netlink.RT_FILTER_TABLE)
		if err != nil {
			t.Fatal(err)
		}
		rules, err := netlink.RuleList(unix.AF_INET6)
		if err != nil {
			t.Fatal(err)
		}
		local, wan := false, false
		for _, rule := range rules {
			local = local || rule.Priority == 10119 && rule.Table == 254 && rule.IifName == "lo" && rule.SuppressPrefixlen == 0
			wan = wan || rule.Priority == 10120 && rule.Table == 120 && rule.IifName == "lo"
		}
		for _, route := range routes {
			if local && wan && route.Dst != nil && route.Dst.String() == "::/0" && route.Src != nil {
				return network.Route{PreferredSource: route.Src.String()}, true
			}
		}
		return network.Route{}, false
	}
	var source string
	until(t, 12*time.Second, func() bool {
		route, ok := hostRoute()
		if ok {
			source = route.PreferredSource
		}
		return ok
	})
	// The obtained address is observed, never guessed from the peer's prefix/IID.
	wan, err := netlink.LinkByName("wan0")
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := netlink.AddrList(wan, unix.AF_INET6)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, address := range addresses {
		if address.IP.String() == source && address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED|unix.IFA_F_DEPRECATED) == 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("host route did not use a DAD-complete obtained address", source, addresses)
	}
	traffic4 := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp4", "192.0.2.1:18080", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		var reply [1]byte
		if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 'V' {
			t.Fatal("IPv4 interrupted", reply, err)
		}
	}
	traffic6 := func() {
		t.Helper()
		for _, protocol := range []string{"tcp6", "udp6"} {
			port := "18093"
			if protocol == "udp6" {
				port = "18094"
			}
			conn, err := net.DialTimeout(protocol, "[2001:db8:ffff::9]:"+port, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			if conn.LocalAddr().String() == "" {
				t.Fatal("missing actual local source")
			}
			if protocol == "udp6" {
				_, err = conn.Write([]byte("host"))
			}
			var reply [1]byte
			if err == nil {
				_, err = io.ReadFull(conn, reply[:])
			}
			conn.Close()
			if err != nil || reply[0] != 'R' {
				t.Fatal(protocol, reply, err)
			}
		}
		for _, protocol := range []string{"udp", "tcp"} {
			client := dns.Client{Net: protocol, Timeout: time.Second}
			query := new(dns.Msg)
			query.SetQuestion("destination.provider.test.", dns.TypeAAAA)
			answer, _, err := client.Exchange(query, "127.0.0.1:1053")
			if err != nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
				t.Fatal("DNS through host route", protocol, answer, err)
			}
		}
	}
	traffic4()
	traffic6()
	f.stop()
	f = f.restart()
	until(t, 12*time.Second, func() bool { _, ok := hostRoute(); return ok })
	traffic4()
	traffic6()
	var healthName string
	for _, resource := range program.Resources {
		if resource.Service != nil && resource.Service.Health != nil {
			healthName = resource.Service.ModuleName()
		}
	}
	until(t, 5*time.Second, func() bool {
		var instance runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/"+healthName, nil, &instance); err != nil {
			t.Fatal(err)
		}
		var observed health.Snapshot
		return json.Unmarshal(instance.Snapshot, &observed) == nil && observed.Active
	})
	if err := advertiser.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = advertiser.Process.Signal(syscall.SIGCONT) })
	var processes supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
		t.Fatal(err)
	}
	var frozen []int
	for _, process := range processes.Modules {
		if process.Name == "control" || process.Role == "health" || process.Role == "ra_client" {
			if err := syscall.Kill(process.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			frozen = append(frozen, process.PID)
			t.Cleanup(func() { _ = syscall.Kill(process.PID, syscall.SIGCONT) })
		}
	}
	until(t, 6*time.Second, func() bool {
		_, present := hostRoute()
		rules, err := netlink.RuleList(unix.AF_INET6)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range rules {
			if rule.Priority == 10120 {
				return false
			}
		}
		return !present
	})
	traffic4()
	for _, pid := range frozen {
		if err := syscall.Kill(pid, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
	}
	if err := advertiser.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool { _, ok := hostRoute(); return ok })
	traffic4()
	traffic6()
	if capture.count(source, "2001:db8:ffff::9", 6, 18093) == 0 || capture.count(source, "2001:db8:ffff::9", 17, 18094) == 0 || capture.count(source, "2001:db8:ffff::9", 17, 53) == 0 {
		t.Fatal("missing actual learned-source TCP/UDP/DNS packets")
	}
	t.Logf("independent radvd/dnsmasq, obtained source %s, host table120, DNS/Health/TCP/UDP, frozen-owner expiry and recovery passed", source)
}
