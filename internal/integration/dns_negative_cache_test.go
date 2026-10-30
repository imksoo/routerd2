//go:build linux && integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestDaemonDNSNegativeCacheExpiresAtConfiguredTTL(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	upstream := fixturePeerNamespace(t, "wan0", "server0")
	client := fixturePeerNamespace(t, "lan0", "client0")
	for _, endpoint := range []struct {
		peer          bool
		name, address string
	}{{true, "server0", "192.0.2.2/24"}, {false, "client0", "198.51.100.2/24"}} {
		ns := client
		if endpoint.peer {
			ns = upstream
		}
		h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		link, err := h.LinkByName(endpoint.name)
		if err != nil {
			t.Fatal(err)
		}
		address, err := netlink.ParseAddr(endpoint.address)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.AddrAdd(link, address); err != nil {
			t.Fatal(err)
		}
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(hosts, nil, 0644); err != nil {
		t.Fatal(err)
	}
	server := startFixtureDNSMasq(t, upstream, "server0", []string{
		"--no-resolv", "--no-hosts", "--log-queries", "--local=/cache.example/",
		"--addn-hosts=" + hosts, "--local-ttl=120", "--address=/dns.example/192.0.2.2",
	})
	healthPBRCapture(t, "wan0")
	healthPBRCapture(t, "lan0")
	resolver := func(name, address, next string) serviceconfig.Service {
		return serviceconfig.Service{Name: name, DNS: &serviceconfig.DNS{RuntimeConfig: dnsconfig.RuntimeConfig{
			Resource: name, Spec: api.DNSResolverSpec{
				Listen:  []api.DNSResolverListenSpec{{Name: "client", Addresses: []string{address}, Port: 53, Sources: []string{"forward"}}},
				Sources: []api.DNSResolverSourceSpec{{Name: "forward", Kind: "upstream", Match: []string{"."}, Upstreams: []string{next}, QueryTimeout: "2s"}},
				Cache:   api.DNSResolverCacheSpec{Enabled: true, MinTTL: "60s", MaxTTL: "24h", NegativeTTL: "30s"},
			},
		}}}
	}
	startFixtureDaemon(t, config.FromNative(config.Native{
		Network: network.Desired{Interfaces: []network.Interface{
			{Name: "wan0", Addresses: []string{"192.0.2.1/24"}},
			{Name: "lan0", Addresses: []string{"198.51.100.1/24"}},
		}},
		Services: []serviceconfig.Service{
			resolver("core", "127.0.0.1", "udp://192.0.2.2:53"),
			resolver("lan", "198.51.100.1", "udp://127.0.0.1:53"),
		},
	}))
	query := func(stage string) (string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		address := map[string]string{"core": "127.0.0.1", "lan": "198.51.100.1", "upstream": "192.0.2.2"}[stage]
		var output []byte
		run := func() error {
			var err error
			output, err = exec.CommandContext(ctx, "dig", "@"+address, "new.cache.example.", "A", "+noedns", "+nocookie", "+time=2", "+tries=1").CombinedOutput()
			return err
		}
		var err error
		switch stage {
		case "lan":
			err = fixtureInsideNamespace(client, run)
		case "upstream":
			err = fixtureInsideNamespace(upstream, run)
		default:
			err = run()
		}
		return string(output), err
	}
	started := make(map[string]time.Time)
	for _, stage := range []string{"core", "lan"} {
		answer, err := query(stage)
		if err != nil || !strings.Contains(answer, "status: NXDOMAIN") {
			t.Fatalf("%s did not initially observe the absent name: %v\n%s", stage, err, answer)
		}
		started[stage] = time.Now()
		t.Logf("%s negative answer accepted at %s", stage, started[stage].UTC().Format(time.RFC3339Nano))
	}
	if err := os.WriteFile(hosts, []byte("203.0.113.88 new.cache.example\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := server.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	until(t, 3*time.Second, func() bool {
		answer, err := query("upstream")
		return err == nil && strings.Contains(answer, "status: NOERROR") && strings.Contains(answer, "203.0.113.88")
	})
	t.Logf("independent upstream serves the newly registered name at %s", time.Now().UTC().Format(time.RFC3339Nano))
	for _, stage := range []string{"core", "lan"} {
		answer, err := query(stage)
		if err != nil || !strings.Contains(answer, "status: NXDOMAIN") {
			t.Fatalf("%s did not retain the unexpired negative answer: %v\n%s", stage, err, answer)
		}
	}
	// Wait for the actual configured negative lifetime, with one second for
	// scheduling. The positive 60-second minimum must not prolong NXDOMAIN.
	timer := time.NewTimer(time.Until(started["lan"].Add(31 * time.Second)))
	defer timer.Stop()
	select {
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	case <-timer.C:
	}
	for _, stage := range []string{"core", "lan"} {
		answer, err := query(stage)
		t.Logf("%s after its original negative answer: elapsed=%s\n%s", stage, time.Since(started[stage]), answer)
		if err != nil || !strings.Contains(answer, "status: NOERROR") || !strings.Contains(answer, "203.0.113.88") {
			t.Errorf("%s retained the absent name beyond negativeTTL30s: %v", stage, err)
		}
	}
}
