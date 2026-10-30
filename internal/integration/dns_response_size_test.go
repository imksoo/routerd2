//go:build linux && integration

package integration

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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

func TestDaemonDNSResponseFitsClientUDPSize(t *testing.T) {
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
	args := []string{"--no-resolv", "--no-hosts", "--log-queries"}
	for i := range 6 {
		args = append(args, fmt.Sprintf("--txt-record=large.example,part%d-%s", i, strings.Repeat("x", 200)))
	}
	startFixtureDNSMasq(t, upstream, "server0", args)
	healthPBRCapture(t, "wan0")
	healthPBRCapture(t, "lan0")
	resolver := func(name, address, upstream string) serviceconfig.Service {
		return serviceconfig.Service{Name: name, DNS: &serviceconfig.DNS{RuntimeConfig: dnsconfig.RuntimeConfig{
			Resource: name, Spec: api.DNSResolverSpec{
				Listen:  []api.DNSResolverListenSpec{{Name: "client", Addresses: []string{address}, Port: 53, Sources: []string{"forward"}}},
				Sources: []api.DNSResolverSourceSpec{{Name: "forward", Kind: "upstream", Match: []string{"."}, Upstreams: []string{upstream}, QueryTimeout: "2s"}},
				Cache:   api.DNSResolverCacheSpec{Enabled: true, MinTTL: "60s"},
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
	// The peer's complete TXT answer exceeds 1232 bytes. Check both hops of
	// the product's homert-style LAN-to-core forwarding with independent dig.
	sizePattern := regexp.MustCompile("MSG SIZE  rcvd: ([0-9]+)")
	flagsPattern := regexp.MustCompile("; flags: ([^;]+);")
	for _, check := range []struct {
		name                    string
		options                 []string
		limit                   int
		truncated, automaticTCP bool
	}{
		{"udp-default", []string{"+notcp", "+ignore", "+noedns"}, 512, true, false},
		{"udp-edns", []string{"+notcp", "+ignore", "+bufsize=768"}, 768, true, false},
		{"udp-large", []string{"+notcp", "+ignore", "+bufsize=4096"}, 4096, false, false},
		{"tcp", []string{"+tcp", "+noedns"}, 0, false, false},
		{"automatic-tcp", []string{"+notcp", "+noedns"}, 0, false, true},
	} {
		t.Run(check.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			args := append([]string{"@198.51.100.1", "large.example", "TXT", "+time=4", "+tries=1", "+nocookie"}, check.options...)
			var output []byte
			err := fixtureInsideNamespace(client, func() error {
				var err error
				output, err = exec.CommandContext(ctx, "dig", args...).CombinedOutput()
				return err
			})
			answer := string(output)
			t.Logf("dig %s\n%s", strings.Join(args, " "), answer)
			if err != nil || !strings.Contains(answer, "status: NOERROR") {
				t.Fatalf("independent DNS query failed: %v", err)
			}
			sizes := sizePattern.FindAllStringSubmatch(answer, -1)
			flags := flagsPattern.FindAllStringSubmatch(answer, -1)
			if len(sizes) == 0 || len(flags) == 0 {
				t.Fatal("dig did not report the received message size and flags")
			}
			size, err := strconv.Atoi(sizes[len(sizes)-1][1])
			if err != nil {
				t.Fatal(err)
			}
			if check.limit != 0 && size > check.limit {
				t.Errorf("UDP response %d exceeds client limit %d", size, check.limit)
			}
			truncated := strings.Contains(" "+flags[len(flags)-1][1]+" ", " tc ")
			if truncated != check.truncated {
				t.Errorf("truncated=%t, want %t", truncated, check.truncated)
			}
			if !check.truncated {
				for i := range 6 {
					if !strings.Contains(answer, fmt.Sprintf("part%d-%s", i, strings.Repeat("x", 200))) {
						t.Errorf("complete answer is missing TXT part %d", i)
					}
				}
			}
			if check.automaticTCP && !strings.Contains(answer, "Truncated, retrying in TCP mode") {
				t.Error("dig did not retry the bounded UDP answer over TCP")
			}
		})
	}
}
