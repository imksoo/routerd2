//go:build linux && integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestDaemonDNSProbeTimeoutIndependentOfQueries(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	upstream := fixturePeerNamespace(t, "wan0", "server0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	peer, err := netlink.NewHandleAt(upstream, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	server, err := peer.LinkByName("server0")
	if err != nil {
		t.Fatal(err)
	}
	serverAddress, err := netlink.ParseAddr("192.0.2.2/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.AddrAdd(server, serverAddress); err != nil {
		t.Fatal(err)
	}
	clientLinks, err := netlink.NewHandleAt(client, unix.NETLINK_ROUTE)
	if err != nil {
		t.Fatal(err)
	}
	defer clientLinks.Close()
	clientLink, err := clientLinks.LinkByName("client0")
	if err != nil {
		t.Fatal(err)
	}
	clientAddress, err := netlink.ParseAddr("198.51.100.2/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := clientLinks.AddrAdd(clientLink, clientAddress); err != nil {
		t.Fatal(err)
	}
	// Static addresses belong to the isolated peer fixture. Delay only DNS
	// replies; ARP must not consume the configured protocol timeout.
	prio := netlink.NewPrio(netlink.QdiscAttrs{LinkIndex: server.Attrs().Index, Parent: netlink.HANDLE_ROOT, Handle: netlink.MakeHandle(1, 0)})
	if err := peer.QdiscAdd(prio); err != nil {
		t.Fatal(err)
	}
	if err := peer.QdiscAdd(netlink.NewNetem(netlink.QdiscAttrs{LinkIndex: server.Attrs().Index, Parent: netlink.MakeHandle(1, 3), Handle: netlink.MakeHandle(30, 0)}, netlink.NetemQdiscAttrs{Latency: 1500000})); err != nil {
		t.Fatal(err)
	}
	udp := nl.IPPROTO_UDP
	if err := peer.FilterAdd(&netlink.Flower{FilterAttrs: netlink.FilterAttrs{LinkIndex: server.Attrs().Index, Parent: netlink.MakeHandle(1, 0), Protocol: unix.ETH_P_IP, Priority: 1}, ClassId: netlink.MakeHandle(1, 3), EthType: unix.ETH_P_IP, IPProto: &udp, SrcPort: 53}); err != nil {
		t.Fatal(err)
	}
	startFixtureDNSMasq(t, upstream, "server0", []string{
		"--no-resolv", "--no-hosts", "--log-queries",
		"--address=/dns.example/192.0.2.90",
		"--address=/delayed.example/203.0.113.9",
		"--address=/recovered.example/203.0.113.9",
	})
	healthPBRCapture(t, "wan0")
	healthPBRCapture(t, "lan0")
	resolver := dnsconfig.RuntimeConfig{Resource: "dns", Spec: api.DNSResolverSpec{
		Listen: []api.DNSResolverListenSpec{{Name: "lan", Addresses: []string{"198.51.100.1"}, Port: 53, Sources: []string{"peer"}}},
		Sources: []api.DNSResolverSourceSpec{{Name: "peer", Kind: "upstream", Match: []string{"."}, Upstreams: []string{"udp://192.0.2.2:53"}, QueryTimeout: "1s",
			Healthcheck: api.DNSResolverHealthcheckSpec{Interval: "1s", Timeout: "3s", FailThreshold: 3, PassThreshold: 2}}},
	}}
	doc := config.FromNative(config.Native{
		Network: network.Desired{Interfaces: []network.Interface{
			{Name: "wan0", Addresses: []string{"192.0.2.1/24"}},
			{Name: "lan0", Addresses: []string{"198.51.100.1/24"}},
		}},
		Services: []serviceconfig.Service{{Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: resolver}}},
	})
	work, err := os.MkdirTemp("", "r2-dns-probe-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	startFixtureDaemonAt(t, doc, work)
	plan, err := config.Compile(doc)
	if err != nil {
		t.Fatal(err)
	}
	var statusClient *localapi.Client
	for _, service := range plan.Native.Services {
		if service.DNS != nil {
			cfg := service.DNSConfig(filepath.Join(work, "run"), filepath.Join(work, "state"))
			statusClient = localapi.NewClient(cfg.SocketPath)
		}
	}
	if statusClient == nil {
		t.Fatal("compiled configuration has no DNS service")
	}
	var observed string
	until(t, 10*time.Second, func() bool {
		var status daemonapi.DaemonStatus
		if err := statusClient.Do(context.Background(), "GET", "/v1/status", nil, &status); err != nil {
			return false
		}
		for _, resource := range status.Resources {
			observed = resource.Observed["upstreams"]
			if strings.HasSuffix(observed, ":Healthy") {
				return true
			}
		}
		return false
	})
	t.Logf("independent dnsmasq: UDP DNS replies delayed 1.5s, health timeout 3s, query timeout 1s; observed %s", observed)
	query := func(name, transport string) (string, time.Duration, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		var output []byte
		started := time.Now()
		err := fixtureInsideNamespace(client, func() error {
			var err error
			output, err = exec.CommandContext(ctx, "dig", "@198.51.100.1", name, "A", "+time=4", "+tries=1", transport).CombinedOutput()
			return err
		})
		return string(output), time.Since(started), err
	}
	answer, elapsed, err := query("delayed.example.", "+notcp")
	if err != nil || !strings.Contains(answer, "status: SERVFAIL") {
		t.Fatalf("ordinary query did not retain its shorter timeout: %v elapsed=%s\n%s", err, elapsed, answer)
	}
	t.Logf("independent LAN query received SERVFAIL with 1s query timeout: elapsed=%s", elapsed)
	if err := peer.QdiscDel(prio); err != nil {
		t.Fatal(err)
	}
	// Use another name so recovery cannot be supplied by a failed query's cache.
	for _, transport := range []string{"+notcp", "+tcp"} {
		answer, elapsed, err := query("recovered.example.", transport)
		if err != nil || !strings.Contains(answer, "status: NOERROR") || !strings.Contains(answer, "203.0.113.9") {
			t.Fatalf("LAN DNS after removing response delay (%s): %v elapsed=%s\n%s", transport, err, elapsed, answer)
		}
		t.Logf("independent LAN DNS %s recovered: address=203.0.113.9 elapsed=%s", transport, elapsed)
	}
}
