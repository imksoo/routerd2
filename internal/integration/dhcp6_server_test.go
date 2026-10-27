//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestDaemonDHCP6InformationIndependentClient(t *testing.T) {
	if !isolateFixtureTimeout(t, "TestDaemonDHCP6InformationIndependentClient", 80*time.Second) {
		return
	}
	client := fixturePeerNamespace(t, "lan0", "client0")
	capture := healthPBRCapture(t, "lan0")
	f := startFixtureDaemon(t, config.FromNative(config.Native{
		Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"2001:db8:80::1/64"}}}},
		Services: []serviceconfig.Service{{Name: "lan-info", DHCP6Server: &serviceconfig.DHCP6Server{
			Interface: "lan0", DNSServers: []netip.Addr{netip.MustParseAddr("2001:db8:80::53")}, DomainSearch: []string{"lan.test"}, SNTPServers: []netip.Addr{netip.MustParseAddr("2001:db8:80::123")},
		}}},
	}))
	ctx := context.Background()
	read := func() (runtimeplan.InstanceStatus, dhcp6serverconfig.Snapshot) {
		t.Helper()
		var state runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.lan-info", nil, &state); err != nil {
			t.Fatal(err)
		}
		var s dhcp6serverconfig.Snapshot
		if len(state.Snapshot) > 0 {
			if err := json.Unmarshal(state.Snapshot, &s); err != nil {
				t.Fatal(err)
			}
		}
		return state, s
	}
	var original runtimeplan.InstanceStatus
	var identity string
	until(t, 12*time.Second, func() bool { state, s := read(); original = state; identity = s.ServerDUID; return s.ServerDUID != "" })
	until(t, 5*time.Second, func() bool {
		ready := false
		if err := fixtureInsideNamespace(client, func() error {
			l, err := netlink.LinkByName("client0")
			if err != nil {
				return err
			}
			addresses, err := netlink.AddrList(l, netlink.FAMILY_V6)
			for _, a := range addresses {
				ready = ready || a.IP.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	})
	request := func(domain string) {
		t.Helper()
		got := fixtureDHCP6Information(t, client, "client0")
		if !slices.Equal(got.dns, []netip.Addr{netip.MustParseAddr("2001:db8:80::53")}) || !slices.Equal(got.search, []string{domain}) {
			t.Fatal("client options", got)
		}
	}
	request("lan.test")
	var modules supervisor.Status
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	killed := false
	for _, m := range modules.Modules {
		if m.Name == "service.lan-info" {
			if err := unix.Kill(m.PID, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
			killed = true
		}
	}
	if !killed {
		t.Fatal("missing DHCPv6 server process")
	}
	until(t, 12*time.Second, func() bool {
		state, s := read()
		if state.Generation <= original.Generation || s.ServerDUID == "" {
			return false
		}
		if s.ServerDUID != identity {
			t.Fatal("server restart changed DUID", identity, s.ServerDUID)
		}
		return true
	})
	request("lan.test")
	// Control recovery deliberately quiesces dynamic workers. Recreated
	// serving must retain its durable identity and actual client behavior.
	var before supervisor.ModuleStatus
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	for _, m := range modules.Modules {
		if m.Name == "service.lan-info" {
			before = m
		}
		if m.Name == "control" {
			if err := unix.Kill(m.PID, unix.SIGKILL); err != nil {
				t.Fatal(err)
			}
		}
	}
	until(t, 12*time.Second, func() bool {
		var state runtimeplan.InstanceStatus
		if err := f.control.Do(ctx, "GET", "/v1/services/service.lan-info", nil, &state); err != nil {
			return false
		}
		var s dhcp6serverconfig.Snapshot
		return state.Generation > before.Generation && len(state.Snapshot) > 0 && json.Unmarshal(state.Snapshot, &s) == nil && s.ServerDUID != "" && s.ServerDUID == identity
	})
	if err := f.supervisor.Do(ctx, "GET", "/v1/status", nil, &modules); err != nil {
		t.Fatal(err)
	}
	for _, m := range modules.Modules {
		if m.Name == "service.lan-info" && m.Generation <= before.Generation {
			t.Fatal("control recovery did not recreate serving", before, m)
		}
	}
	request("lan.test")
	f = f.restart()
	until(t, 12*time.Second, func() bool {
		_, s := read()
		if s.ServerDUID == "" {
			return false
		}
		if s.ServerDUID != identity {
			t.Fatal("daemon restart changed DUID", identity, s.ServerDUID)
		}
		return true
	})
	request("lan.test")
	// A different source epoch must apply before it can authorize serving;
	// confirmed rollback must restore actual client options with the same DUID.
	var current control.State

	if err := f.control.Do(ctx, "GET", "/v1/config", nil, &current); err != nil {
		t.Fatal(err)
	}
	trialIdentity := "000400112233445546778899aabbccddeeff"
	candidate := current.Config
	candidate.Resources = slices.Clone(candidate.Resources)
	for i, r := range candidate.Resources {
		if r.Service == nil || r.Service.DHCP6Server == nil {
			continue
		}
		service := *r.Service
		settings := *service.DHCP6Server
		settings.DomainSearch = []string{"candidate.test"}
		settings.ServerDUID = trialIdentity
		service.DHCP6Server = &settings
		candidate.Resources[i].Service = &service
	}
	beforeTrial, _ := read()
	if err := f.control.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: current.Revision, RequestID: "information-trial", Config: candidate, ConfirmTimeoutSeconds: 5}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 4*time.Second, func() bool {
		state, s := read()
		return state.ConfigDigest != beforeTrial.ConfigDigest && s.ServerDUID == trialIdentity
	})
	request("candidate.test")
	until(t, 12*time.Second, func() bool {
		current = control.State{}
		return f.control.Do(ctx, "GET", "/v1/config", nil, &current) == nil && current.Revision == 3 && current.Confirmation == nil && current.Healthy
	})
	request("lan.test")
	_, restored := read()
	if restored.ServerDUID != identity {
		t.Fatal("confirmed rollback changed server identity", identity, restored.ServerDUID)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	serverID, err := hex.DecodeString(identity)
	if err != nil {
		t.Fatal(err)
	}
	trialID, err := hex.DecodeString(trialIdentity)
	if err != nil {
		t.Fatal(err)
	}
	trialReply := false
	replies := 0
	for _, p := range capture.packets {
		b := p.data
		if len(b) < 66 || b[20] != 17 || binary.BigEndian.Uint16(b[54:]) != 547 || binary.BigEndian.Uint16(b[56:]) != 546 || b[62] != 7 {
			continue
		}
		opts, err := dhcp6FixtureOptions(b[66:])
		if err != nil {
			t.Fatal(err)
		}
		duid, ntp, refresh := false, false, false
		for _, o := range opts {
			switch o.code {
			case 2:
				trialReply = trialReply || bytes.Equal(o.value, trialID)
				duid = bytes.Equal(o.value, serverID) || bytes.Equal(o.value, trialID)
			case 31:
				ntp = bytes.Equal(o.value, netip.MustParseAddr("2001:db8:80::123").AsSlice())
			case 32:
				refresh = len(o.value) == 4 && binary.BigEndian.Uint32(o.value) == 86400
			}
		}
		if !duid || !ntp || !refresh {
			t.Fatal("wire server identity/SNTP/refresh", opts)
		}
		replies++
	}
	if replies < 6 || !trialReply {
		t.Fatal("missing real Replies", replies)
	}
	t.Log("BusyBox DNS/search plus wire SNTP/refresh; worker/control/daemon recovery and confirmed rollback retained server DUID", identity)
}

// BusyBox decodes server options for its ordinary script hook. Keep this
// hook private to the fixture; it does not install a resolver or address.
type dhcp6FixtureInformation struct {
	dns    []netip.Addr
	search []string
}

func fixtureDHCP6Information(t *testing.T, client netns.NsHandle, iface string) dhcp6FixtureInformation {
	t.Helper()
	script := filepath.Join(t.TempDir(), "options.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in bound|renew) ;; *) exit 0 ;; esac\nprintf 'dns=%s\\nsearch=%s\\n' \"$dns\" \"$search\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	call, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(call, "busybox", "udhcpc6", "-l", "-f", "-n", "-q", "-i", iface, "-t", "3", "-T", "1", "-O", "23", "-O", "24", "-O", "31", "-s", script)
	var output []byte
	if err := fixtureInsideNamespace(client, func() error { var err error; output, err = cmd.CombinedOutput(); return err }); err != nil {
		t.Fatalf("BusyBox Information-request: %v: %s", err, output)
	}
	var out dhcp6FixtureInformation
	for _, line := range strings.Split(string(output), "\n") {
		if value, ok := strings.CutPrefix(line, "dns="); ok {
			for _, value := range strings.Fields(value) {
				ip, err := netip.ParseAddr(value)
				if err != nil {
					t.Fatal(err)
				}
				out.dns = append(out.dns, ip)
			}
		}
		if value, ok := strings.CutPrefix(line, "search="); ok {
			out.search = strings.Fields(value)
		}
	}
	return out
}
