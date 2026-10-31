//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/vishvananda/netlink"
)

func TestDaemonSnapshotExportAndApply(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	peer := fixturePeerNamespace(t, "lan0", "client0")
	_ = healthPBRCapture(t, "lan0")
	if err := fixtureInsideNamespace(peer, func() error {
		link, err := netlink.LinkByName("client0")
		if err != nil {
			return err
		}
		address, err := netlink.ParseAddr("198.18.0.2/24")
		if err != nil {
			return err
		}
		return netlink.AddrAdd(link, address)
	}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "r2-export-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	stateDir, runDir := filepath.Join(dir, "state"), filepath.Join(dir, "run")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	clientKey, hostKey := filepath.Join(dir, "client_key"), filepath.Join(stateDir, "ssh_host_ed25519_key")
	for _, path := range []string{clientKey, hostKey} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("generate independent SSH key: %v %s", err, output)
		}
	}
	clientPublic, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	sshPort := port(t)
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, append([]byte(fmt.Sprintf("[198.18.0.1]:%d ", sshPort)), hostPublic...), 0600); err != nil {
		t.Fatal(err)
	}
	document := func(answer string) config.Document {
		return config.FromNative(config.Native{
			Network: network.Desired{Interfaces: []network.Interface{{Name: "lan0", Addresses: []string{"198.18.0.1/24"}}}},
			Services: []serviceconfig.Service{{Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: dnsconfig.RuntimeConfig{
				Resource: "dns",
				Spec: api.DNSResolverSpec{
					Listen:  []api.DNSResolverListenSpec{{Name: "lan", Addresses: []string{"198.18.0.1"}, Port: 53}},
					Sources: []api.DNSResolverSourceSpec{{Name: "zone", Kind: "zone", Match: []string{"export.test"}, ZoneRef: []string{"DNSZone/lan"}}},
				},
				Zones: []dnsconfig.RuntimeZone{{Name: "lan", Spec: api.DNSZoneSpec{Zone: "export.test", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: answer}}}}},
			}}}},
		})
	}
	f := startFixtureDaemonConfigured(t, document("192.0.2.1"), dir, nil, runtimeconfig.SSH{
		Listen: fmt.Sprintf("198.18.0.1:%d", sshPort), User: "admin", AuthorizedKeys: []string{string(clientPublic)},
	})
	readState := func() control.State {
		t.Helper()
		var state control.State
		if err := f.control.Do(t.Context(), "GET", "/v1/config", nil, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	checkDNS := func(want string) {
		t.Helper()
		for _, transport := range []string{"+notcp", "+tcp"} {
			var output []byte
			until(t, 10*time.Second, func() bool {
				err := fixtureInsideNamespace(peer, func() error {
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					var err error
					output, err = exec.CommandContext(ctx, "dig", "@198.18.0.1", "router.export.test", "A", transport, "+short", "+time=1", "+tries=1").CombinedOutput()
					return err
				})
				return err == nil && strings.TrimSpace(string(output)) == want
			})
			t.Logf("independent dig %s returned %s", transport, strings.TrimSpace(string(output)))
		}
	}
	cli := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), args...).CombinedOutput()
		if err != nil {
			t.Fatalf("product CLI %v: %v %s", args, err, output)
		}
		return output
	}
	checkDNS("192.0.2.1")
	until(t, 5*time.Second, func() bool { return readState().Healthy })
	initial := readState()
	reference := cli("config", "snapshot", "--runtime-dir="+runDir, "--expected-revision="+fmt.Sprint(initial.Revision))
	var saved config.SnapshotRef
	if err := json.Unmarshal(reference, &saved); err != nil || saved.Validate() != nil || saved.Revision != initial.Revision {
		t.Fatalf("invalid saved reference: %v %s", err, reference)
	}
	referencePath := filepath.Join(dir, "saved-reference.json")
	if err := os.WriteFile(referencePath, reference, 0600); err != nil {
		t.Fatal(err)
	}
	changedPath := filepath.Join(dir, "changed.json")
	if err := config.AtomicJSON(changedPath, document("192.0.2.2")); err != nil {
		t.Fatal(err)
	}
	cli("config", "apply", "--file="+changedPath, "--runtime-dir="+runDir, "--expected-revision="+fmt.Sprint(initial.Revision), "--request-id=export-change")
	checkDNS("192.0.2.2")
	changed := readState()
	var exported []byte
	var sshErrors bytes.Buffer
	if err := fixtureInsideNamespace(peer, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=3", "-i", clientKey, "-p", fmt.Sprint(sshPort), "admin@198.18.0.1", "snapshot export")
		command.Stdin = bytes.NewReader(reference)
		command.Stderr = &sshErrors
		var err error
		exported, err = command.Output()
		return err
	}); err != nil {
		t.Fatalf("independent OpenSSH snapshot export: %v %s", err, sshErrors.String())
	}
	exportPath := filepath.Join(dir, "saved-config.yaml")
	if err := os.WriteFile(exportPath, exported, 0600); err != nil {
		t.Fatal(err)
	}
	recoveredDocument, err := config.Read(exportPath)
	if err != nil || !reflect.DeepEqual(recoveredDocument, initial.Config) {
		t.Fatalf("export did not contain the original applicable configuration: %v", err)
	}
	var apiDocument config.Document
	if err := f.control.Do(t.Context(), "POST", "/v1/config/snapshots/export", saved, &apiDocument); err != nil || !reflect.DeepEqual(apiDocument, recoveredDocument) {
		t.Fatalf("saved document API differs from SSH export: %v", err)
	}
	localExport := cli("config", "export-snapshot", "--snapshot="+referencePath, "--runtime-dir="+runDir)
	if !bytes.Equal(localExport, exported) {
		t.Fatal("local CLI and independent SSH exported different YAML")
	}
	afterExport := readState()
	if afterExport.Revision != changed.Revision || afterExport.StoreID != changed.StoreID || afterExport.LastRequestID != changed.LastRequestID || !reflect.DeepEqual(afterExport.Config, changed.Config) {
		t.Fatal("read-only export changed the current configuration")
	}
	checkDNS("192.0.2.2")
	cli("config", "apply", "--file="+exportPath, "--runtime-dir="+runDir, "--expected-revision="+fmt.Sprint(changed.Revision), "--request-id=apply-exported-config")
	checkDNS("192.0.2.1")
	restored := readState()
	if restored.Revision != changed.Revision+1 || restored.StoreID != initial.StoreID || restored.Confirmation != nil || !reflect.DeepEqual(restored.Config, initial.Config) {
		t.Fatal("ordinary apply did not restore the exported configuration as a new revision")
	}
	t.Logf("saved revision %d exported over independent OpenSSH, API and local CLI without changing revision %d; ordinary config apply restored independent UDP/TCP DNS at revision %d", saved.Revision, changed.Revision, restored.Revision)
}
