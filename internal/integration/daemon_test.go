//go:build linux && integration

// Package integration exercises the real two-binary process tree. Every test
// that changes a kernel interface first reexecs into a new user/network namespace.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/derivedcontroller"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/legacyconfig"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	mdns "github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/ssh"
)

func TestDaemonConfigurationAndRecovery(t *testing.T) {
	if !isolateFixture(t, t.Name()) {
		return
	}
	dir, err := os.MkdirTemp("", "r2-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	runDir, stateDir := filepath.Join(dir, "run"), filepath.Join(dir, "state")
	dnsPort, sshPort := port(t), port(t)
	dns := dnsconfig.RuntimeConfig{Resource: "dns", Spec: api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1", "198.18.0.1"}, Port: dnsPort}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"DNSZone/lan-zone"}}}}, Zones: []dnsconfig.RuntimeZone{{Name: "lan-zone", Spec: api.DNSZoneSpec{Zone: "lab.example", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: "192.0.2.1"}}}}}}
	leases := serviceconfig.DHCPServer{Interface: "mgmt0", ServerAddress: "198.18.0.1", Authoritative: true,
		Primary: dhcpserverconfig.Scope{Prefix: "198.18.0.0/24", Start: "198.18.0.100", End: "198.18.0.110", Router: "198.18.0.1", LeaseSeconds: 120}}
	dns.Spec.QueryLog = api.DNSResolverQueryLogSpec{Enabled: true}
	doc := config.FromNative(config.Native{Services: []serviceconfig.Service{{Name: "dns", DNS: &serviceconfig.DNS{RuntimeConfig: dns}}, {Name: "leases", DHCPServer: &leases}}})
	// Exercise the imported management zone through a separate client namespace.
	// ManagementAccess is an admission check, not a source-prefix firewall rule.
	peer := fixturePeerNamespace(t, "mgmt0", "client0")
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
	management, err := legacyconfig.Compile([]byte(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: management}
spec:
  resources:
    - apiVersion: net.routerd.net/v1alpha1
      kind: Interface
      metadata: {name: management}
      spec: {ifname: mgmt0, managed: true, adminUp: true}
    - apiVersion: net.routerd.net/v1alpha1
      kind: IPv4StaticAddress
      metadata: {name: management}
      spec: {interface: management, address: 198.18.0.1/24}
    - apiVersion: firewall.routerd.net/v1alpha1
      kind: FirewallZone
      metadata: {name: management}
      spec: {role: mgmt, interfaces: [Interface/management]}
    - apiVersion: net.routerd.net/v1alpha1
      kind: ManagementAccess
      metadata: {name: management}
      spec: {interfaces: [Interface/management], allowSourceCIDRs: [198.18.0.0/24]}
    - apiVersion: system.routerd.net/v1alpha1
      kind: WebConsole
      metadata: {name: management}
      spec:
        enabled: true
        listenAddress: 198.18.0.1
        port: 8080
        basePath: /ops
        title: "routerd2 <lab>"
        links: [{label: documentation, url: https://example.net/routerd2}]
`))
	if err != nil {
		t.Fatal(err)
	}
	consoleModule := ""
	for _, resource := range management.Document.Resources {
		if resource.Service != nil && resource.Service.WebConsole != nil {
			consoleModule = resource.Service.ModuleName()
		}
	}
	if consoleModule == "" {
		t.Fatal("imported console service missing")
	}
	doc.Resources = append(doc.Resources, management.Document.Resources...)
	_ = healthPBRCapture(t, "mgmt0")
	configPath := filepath.Join(dir, "config.json")
	if err = config.AtomicJSON(configPath, doc); err != nil {
		t.Fatal(err)
	}
	// The source identity exists before routerd2 starts. Pin its public key,
	// never a key learned from the new server, in the independent client.
	legacyKey, clientKey := filepath.Join(dir, "legacy_host_key"), filepath.Join(dir, "client_key")
	for _, path := range []string{legacyKey, clientKey} {
		if output, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("generate independent OpenSSH key: %v %s", err, output)
		}
	}
	hostBytes, err := os.ReadFile(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, err := os.ReadFile(legacyKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	host, _, _, _, err := ssh.ParseAuthorizedKey(hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(stateDir, "ssh_host_ed25519_key")
	if err := os.WriteFile(hostPath, hostBytes, 0600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHosts, append([]byte(fmt.Sprintf("[198.18.0.1]:%d ", sshPort)), hostPublic...), 0600); err != nil {
		t.Fatal(err)
	}
	private, err := os.ReadFile(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(private)
	clear(private)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeconfig.Boot{Platform: "host", Config: configPath, Workers: runtimeconfig.NativeWorkers(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd-modules")), RuntimeDir: runDir, StateDir: stateDir, SSH: runtimeconfig.SSH{Listen: fmt.Sprintf("0.0.0.0:%d", sshPort), User: "admin", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}}}
	b.Workers["dhcp_server"] = filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd-modules")
	b.Engines = map[string]string{"dhcp_server": filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "dnsmasq-engine.tar")}
	boot := filepath.Join(dir, "boot.json")
	if err = config.AtomicJSON(boot, b); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "console.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	startDaemon := func() (*exec.Cmd, chan error) {
		t.Helper()
		daemon := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "boot", "--file="+boot)
		daemon.Stdout = log
		daemon.Stderr = log
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- daemon.Wait() }()
		return daemon, exited
	}
	daemon, exited := startDaemon()
	stopDaemon := func() {
		t.Helper()
		if daemon == nil {
			return
		}
		process := daemon.Process
		daemon = nil
		_ = process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("daemon exit: %v", err)
			}
		case <-time.After(12 * time.Second):
			_ = process.Kill()
			<-exited
			t.Fatal("daemon failed to drain its workers")
		}
	}
	defer func() {
		if t.Failed() && daemon != nil {
			t.Logf("executable FD diagnostics before shutdown:\n%s", executableFDReport(map[string]string{"modules": b.Workers["network"], "logging": b.Workers["logging"]}, daemon.Process.Pid))
		}
		stopDaemon()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("console: %s", data)
			data, _ = os.ReadFile(filepath.Join(stateDir, "logs", "routerd.jsonl"))
			t.Logf("module log: %s", data)
		}
	}()
	super := localapi.NewClient(filepath.Join(runDir, supervisor.SocketName))
	ctl := localapi.NewClient(filepath.Join(runDir, control.SocketName))
	ctx := context.Background()
	status := func() supervisor.Status {
		var s supervisor.Status
		_ = super.Do(ctx, "GET", "/v1/status", nil, &s)
		return s
	}
	until(t, 15*time.Second, func() bool { return status().Ready })
	console := func(method, path string, want int, contains string) string {
		var content string
		t.Helper()
		err := fixtureInsideNamespace(peer, func() error {
			connection, err := net.DialTimeout("tcp", "198.18.0.1:8080", 3*time.Second)
			if err != nil {
				return err
			}
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			request, err := http.NewRequest(method, "http://198.18.0.1:8080/ops/"+path, nil)
			if err != nil {
				return err
			}
			if err := request.Write(connection); err != nil {
				return err
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			if err != nil {
				return err
			}
			content = string(body)
			if response.StatusCode != want || !bytes.Contains(body, []byte(contains)) {
				return fmt.Errorf("console %s %s: HTTP %d: %s", method, path, response.StatusCode, body)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	until(t, 5*time.Second, func() bool {
		var health control.Health
		return ctl.Do(ctx, "GET", "/v1/health", nil, &health) == nil && health.Healthy
	})
	console("GET", "", 200, "routerd2 &lt;lab&gt;")
	console("GET", "network", 200, "198.18.0.1/24")
	console("GET", "resources", 200, "service/console-")
	console("GET", "configuration", 200, "設定のSHA-256")
	console("POST", "configuration", 405, "read-only console")
	console("GET", "v1/config/commit", 404, "404")
	t.Log("imported WebConsole: independent HTTP client read live health, kernel address/routes, resource conditions and confirmation metadata under /ops/; escaped title, read-only methods and private API isolation checked")
	console("GET", "clients", 200, "観測された貸出はありません。")
	console("GET", "clients?service=service.dns", 404, "指定したDHCPサービスはありません")
	// A second interface starts without an address; its actual DHCP lease is
	// compared with the independently fetched console, never a fixed pool slot.
	const clientMAC = "02:bb:00:00:02:01"
	for _, args := range [][]string{
		{"link", "add", "lease0", "link", "client0", "address", clientMAC, "type", "macvlan", "mode", "bridge"},
		{"link", "set", "lease0", "up"},
	} {
		cmd := exec.Command("ip", args...)
		if err := fixtureInsideNamespace(peer, cmd.Run); err != nil {
			t.Fatal(err)
		}
	}
	report, hook := filepath.Join(dir, "client-lease.txt"), filepath.Join(dir, "client-lease.sh")
	if err := os.WriteFile(hook, []byte(fmt.Sprintf(`#!/bin/sh
case "$1" in
bound|renew)
  busybox ifconfig "$interface" "$ip" netmask "$subnet"
  printf '%%s %%s\n' "$1" "$ip" >> %s
  ;;
esac
`, report)), 0700); err != nil {
		t.Fatal(err)
	}
	dhcp := exec.Command("busybox", "udhcpc", "-f", "-i", "lease0", "-x", "hostname:console-client", "-s", hook, "-p", filepath.Join(dir, "client.pid"))
	output, err := os.Create(filepath.Join(dir, "client.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	dhcp.Stdout, dhcp.Stderr = output, output
	if err := fixtureInsideNamespace(peer, dhcp.Start); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dhcp.Process.Kill(); _ = dhcp.Wait() }()
	var address string
	until(t, 10*time.Second, func() bool {
		data, _ := os.ReadFile(report)
		fields := strings.Fields(string(data))
		if len(fields) != 2 || fields[0] != "bound" {
			return false
		}
		address = fields[1]
		return true
	})
	observeLease := func() dhcpserverconfig.Snapshot {
		t.Helper()
		var instance runtimeplan.InstanceStatus
		if err := ctl.Do(ctx, "GET", "/v1/services/service.leases", nil, &instance); err != nil {
			t.Fatal(err)
		}
		var snapshot dhcpserverconfig.Snapshot
		if err := json.Unmarshal(instance.Snapshot, &snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	var expiry time.Time
	until(t, 5*time.Second, func() bool {
		for _, lease := range observeLease().Leases {
			if lease.MACAddress == clientMAC && lease.IPAddress == address && lease.Hostname == "console-client" {
				expiry = lease.ExpiresAt
				return true
			}
		}
		return false
	})
	body := console("GET", "clients?service=service.leases", 200, address)
	for _, value := range []string{clientMAC, "console-client", expiry.String(), "貸出期限内"} {
		if !strings.Contains(html.UnescapeString(body), value) {
			t.Fatalf("console missing acquired lease value %q: %s", value, body)
		}
	}
	// dnsmasq lease files have whole-second precision: wait for the next
	// second before requesting renewal so the displayed deadline must change.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if err := dhcp.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(report)
		if !strings.Contains(string(data), "renew "+address+"\n") {
			return false
		}
		for _, lease := range observeLease().Leases {
			if lease.MACAddress == clientMAC && lease.ExpiresAt.After(expiry) {
				expiry = lease.ExpiresAt
				return true
			}
		}
		return false
	})
	body = console("GET", "clients?service=service.leases", 200, address)
	if !strings.Contains(html.UnescapeString(body), expiry.String()) {
		t.Fatal("renewed deadline missing from console", body)
	}
	if err := dhcp.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool { return len(observeLease().Leases) == 0 })
	body = console("GET", "clients?service=service.leases", 200, "観測された貸出はありません。")
	if strings.Contains(body, clientMAC) || strings.Contains(body, "console-client") {
		t.Fatal("released client remains in console", body)
	}
	t.Logf("independent BusyBox acquired %s (%s); independent HTTP client saw hostname, MAC, actual IP and deadline, renewed deadline and removed released lease", address, clientMAC)
	answer(t, dnsPort, "192.0.2.1")
	// The SSH channel drives the real candidate editor and control transaction.
	openSSHState := func() control.State {
		t.Helper()
		var output []byte
		err := fixtureInsideNamespace(peer, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			output, err = exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UpdateHostKeys=no", "-o", "IdentityAgent=none", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=3", "-i", clientKey, "-p", fmt.Sprint(sshPort), "admin@198.18.0.1", "show config").CombinedOutput()
			return err
		})
		var state control.State
		if err != nil || json.Unmarshal(output, &state) != nil || state.Revision == 0 {
			t.Fatalf("independent OpenSSH pinned to the source identity: %v %s", err, output)
		}
		current, err := os.ReadFile(hostPath)
		if err != nil || !bytes.Equal(current, hostBytes) {
			t.Fatal("migration changed the source host key encoding", err)
		}
		info, err := os.Stat(hostPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("migration changed private host key permissions", err)
		}
		return state
	}
	_ = openSSHState()
	t.Logf("independent OpenSSH authenticated with the source host identity %s after initial provisioning; original OpenSSH bytes and mode 0600 retained", ssh.FingerprintSHA256(host))
	var client *ssh.Client
	err = fixtureInsideNamespace(peer, func() error {
		var dialErr error
		client, dialErr = ssh.Dial("tcp", fmt.Sprintf("198.18.0.1:%d", sshPort), &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host), Timeout: 3 * time.Second})
		return dialErr
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	t.Log("imported ManagementAccess/mgmt FirewallZone: real SSH from isolated client accepted")

	querySince := time.Now().UTC().Format(time.RFC3339Nano)
	if err := fixtureInsideNamespace(peer, func() error {
		answerAddress(t, fmt.Sprintf("198.18.0.1:%d", dnsPort), "192.0.2.1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var originalHistory runtimeplan.DNSHistory
	until(t, 3*time.Second, func() bool {
		return ctl.Do(ctx, "GET", "/v1/services/service.dns/dns-history?client=198.18.0.2&limit=10", nil, &originalHistory) == nil && len(originalHistory.Queries) >= 2
	})
	dnsCommand := "show dns-queries dns client ::ffff:198.18.0.2 name router.lab.example. response NOERROR since " + querySince + " limit 1"
	historySession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	historyOutput, err := historySession.CombinedOutput(dnsCommand)
	_ = historySession.Close()
	var history runtimeplan.DNSHistory
	if err != nil || json.Unmarshal(historyOutput, &history) != nil || history.Service != "service.dns" || history.Limit != 1 || !history.Truncated || len(history.Queries) != 1 {
		t.Fatalf("SSH DNS history lost result metadata: %v %s", err, historyOutput)
	}
	record := history.Queries[0]
	if record.ID != originalHistory.Queries[0].ID || record.Timestamp != originalHistory.Queries[0].Timestamp || record.ClientAddress != "198.18.0.2" || record.QuestionName != originalHistory.Queries[0].QuestionName || strings.TrimSuffix(record.QuestionName, ".") != "router.lab.example" || record.ResponseCode != "NOERROR" {
		t.Fatalf("SSH DNS history changed original record: %+v %+v", record, originalHistory.Queries[0])
	}
	failedHistory, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var failedOutput bytes.Buffer
	failedHistory.Stdout = &failedOutput
	err = failedHistory.Run("show dns-queries dns client invalid-address")
	_ = failedHistory.Close()
	if err == nil || failedOutput.Len() != 0 {
		t.Fatalf("invalid DNS filter became successful history: %v %s", err, failedOutput.String())
	}
	t.Log("independent UDP/TCP DNS generated stored records; real SSH history kept original values/truncation and normalized mapped IPv4; invalid filter failed without successful output")
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var transcript, stderr bytes.Buffer
	session.Stdout = &transcript
	session.Stderr = &stderr
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = session.RequestPty("xterm", 24, 100, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err = session.Shell(); err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(input, "configure\r"+dnsCommand+"\rset resources[service/dns].service.dns.zones[lan-zone].spec.records[0].ipv4 \"192.0.2.2\"\rvalidate\rcommit confirmed 5\rconfirm\rexit\rexit\r")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("SSH editor: %v\n%s", err, transcript.String())
		}
	case <-time.After(15 * time.Second):
		session.Close()
		t.Fatal("SSH editor timed out")
	}
	session.Close()
	if strings.Contains(transcript.String(), "Error:") || stderr.Len() != 0 {
		t.Fatalf("SSH editing failed: %s %s", transcript.String(), stderr.String())
	}
	if !strings.Contains(transcript.String(), `"truncated": true`) || !strings.Contains(transcript.String(), `"questionName": "`+record.QuestionName+`"`) {
		t.Fatal("DNS history unavailable in candidate mode", transcript.String())
	}
	var state control.State
	if err = ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || state.Confirmation != nil {
		t.Fatalf("candidate not committed and confirmed: %+v; transcript=%s", state, transcript.String())
	}
	answer(t, dnsPort, "192.0.2.2")
	// Save through the actual operator command and Unix API. This captures
	// intent only; a later boot selection must explicitly adopt its reference.
	snapshotCommand := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "snapshot", "--runtime-dir="+runDir, "--expected-revision=2")
	snapshotOutput, err := snapshotCommand.CombinedOutput()
	var saved config.SnapshotRef
	if err != nil || json.Unmarshal(snapshotOutput, &saved) != nil || saved.Validate() != nil || saved.StoreID != state.StoreID || saved.Revision != 2 {
		t.Fatalf("actual snapshot CLI did not capture confirmed revision: %v %s", err, snapshotOutput)
	}
	snapshotShell, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var snapshotTranscript, snapshotErrors bytes.Buffer
	snapshotShell.Stdout, snapshotShell.Stderr = &snapshotTranscript, &snapshotErrors
	snapshotShell.Stdin = strings.NewReader("configure\rshow snapshots\rabort\rexit\r")
	if err := snapshotShell.RequestPty("xterm", 24, 100, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := snapshotShell.Shell(); err != nil {
		t.Fatal(err)
	}
	snapshotDone := make(chan error, 1)
	go func() { snapshotDone <- snapshotShell.Wait() }()
	select {
	case err := <-snapshotDone:
		if err != nil {
			t.Fatal(err, snapshotTranscript.String(), snapshotErrors.String())
		}
	case <-time.After(10 * time.Second):
		_ = snapshotShell.Close()
		t.Fatal("snapshot shell timed out")
	}
	_ = snapshotShell.Close()
	if snapshotErrors.Len() != 0 || strings.Contains(snapshotTranscript.String(), "Error:") || !strings.Contains(snapshotTranscript.String(), saved.Digest) {
		t.Fatal("candidate mode did not expose immutable saved references", snapshotTranscript.String(), snapshotErrors.String())
	}
	localHistory := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "cli", "--runtime-dir="+runDir)
	localHistory.Stdin = strings.NewReader(dnsCommand + "\rexit\r")
	localOutput, err := localHistory.CombinedOutput()
	if err != nil || bytes.Contains(localOutput, []byte("Error:")) || !bytes.Contains(localOutput, []byte(`"questionName": "`+record.QuestionName+`"`)) {
		t.Fatalf("local shell DNS history: %v %s", err, localOutput)
	}
	t.Log("candidate-mode DNS history preserved the subsequent configuration commit; candidate-mode snapshot listing and local shell DNS history both succeeded")
	savedPath := filepath.Join(dir, "saved-reference.json")
	if err := os.WriteFile(savedPath, snapshotOutput, 0600); err != nil {
		t.Fatal(err)
	}
	snapshotSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	snapshotBody, _ := json.Marshal(control.SnapshotRequest{ExpectedRevision: 2})
	snapshotSession.Stdin = bytes.NewReader(snapshotBody)
	sshSnapshotOutput, err := snapshotSession.CombinedOutput("snapshot")
	_ = snapshotSession.Close()
	var sshSaved config.SnapshotRef
	if err != nil || json.Unmarshal(sshSnapshotOutput, &sshSaved) != nil || sshSaved != saved {
		t.Fatalf("SSH snapshot differs from local confirmed capture: %v %s", err, sshSnapshotOutput)
	}
	configStore := &config.Store{Dir: stateDir}
	originalSnapshot, err := configStore.ResolveSnapshot(saved)
	if err != nil {
		t.Fatal(err)
	}
	if originalSnapshot.Config.Resources[0].Service.DNS.Zones[0].Spec.Records[0].IPv4 != "192.0.2.2" {
		t.Fatal("snapshot CLI saved a different configuration")
	}
	// An unconfirmed record change must roll back durably and restart only DNS.
	changedBytes, _ := json.Marshal(state.Config)
	var changed config.Document
	_ = json.Unmarshal(changedBytes, &changed)
	changed.Resources[0].Service.DNS.Zones[0].Spec.Records[0].IPv4 = "192.0.2.3"
	request := control.CommitRequest{ExpectedRevision: 2, RequestID: "fixture-rollback", Config: changed, ConfirmTimeoutSeconds: 5}
	if err = ctl.Do(ctx, "POST", "/v1/config/commit", request, &state); err != nil {
		t.Fatal(err)
	}
	answer(t, dnsPort, "192.0.2.3")
	console("GET", "configuration", 200, "確定待ちの期限")
	if err := ctl.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: 3}, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("actual control accepted unconfirmed snapshot", err)
	}
	until(t, 12*time.Second, func() bool {
		state = control.State{}
		_ = ctl.Do(ctx, "GET", "/v1/config", nil, &state)
		return state.Revision == 4 && state.Confirmation == nil && state.Healthy
	})
	answer(t, dnsPort, "192.0.2.2")
	console("GET", "configuration", 200, "<dd>4</dd>")
	// Killing logging cannot interrupt DNS. Killing control reacquires workers
	// from the committed revision and does not lose the confirmed configuration.
	var durableBefore []events.Record
	until(t, 5*time.Second, func() bool {
		var page struct {
			Records []events.Record `json:"records"`
		}
		if ctl.Do(ctx, "GET", "/v1/observations?after_cursor=0&limit=16", nil, &page) != nil || len(page.Records) != 4 {
			return false
		}
		durableBefore = page.Records
		return true
	})
	for i, kind := range []string{"committed", "confirmed", "committed", "rolled_back_timeout"} {
		if durableBefore[i].Event.Topic != "configuration."+kind {
			t.Fatalf("unexpected durable lifecycle: %+v", durableBefore)
		}
	}
	logClient := localapi.NewClient(filepath.Join(runDir, "logging.sock"))
	defer logClient.HTTP.CloseIdleConnections()
	oldLogCursor := checkConsoleLogsBeforeRestart(t, logClient, console)
	for _, name := range []string{"logging", "control", consoleModule} {
		old := status()
		var victim supervisor.ModuleStatus
		for _, m := range old.Modules {
			if m.Name == name {
				victim = m
			}
		}
		if victim.PID == 0 {
			t.Fatal("missing victim module", name)
		}
		if err = syscall.Kill(victim.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if name == "logging" {
			answer(t, dnsPort, "192.0.2.2")
		}
		until(t, 15*time.Second, func() bool {
			s := status()
			if !s.Ready {
				return false
			}
			for _, m := range s.Modules {
				if m.Name == name {
					return m.Ready && m.PID != victim.PID && m.Generation > victim.Generation
				}
			}
			return false
		})
		answer(t, dnsPort, "192.0.2.2")
		until(t, 5*time.Second, func() bool {
			var health control.Health
			return ctl.Do(ctx, "GET", "/v1/health", nil, &health) == nil && health.Healthy
		})
		console("GET", "", 200, "routerd2 &lt;lab&gt;")
		console("GET", "network", 200, "198.18.0.1/24")
		if name == "logging" {
			checkConsoleLogsAfterRestart(t, logClient, console, oldLogCursor)
		}
	}
	var durableAfter struct {
		Records []events.Record `json:"records"`
	}
	if err := ctl.Do(ctx, "GET", "/v1/observations?after_cursor=0&limit=16", nil, &durableAfter); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(durableBefore)
	b2, _ := json.Marshal(durableAfter.Records)
	if !bytes.Equal(a, b2) {
		t.Fatal("control SIGKILL changed or duplicated durable lifecycle history")
	}
	t.Log("durable commit/confirm/rollback observations survived control SIGKILL with stable IDs/cursors")
	var recovered control.State
	if err := ctl.Do(ctx, "GET", "/v1/config", nil, &recovered); err != nil || recovered.StoreID != saved.StoreID {
		t.Fatal("control restart changed store identity", err)
	}
	recoveredSnapshot, err := configStore.ResolveSnapshot(saved)
	aSnapshot, _ := json.Marshal(originalSnapshot)
	bSnapshot, _ := json.Marshal(recoveredSnapshot)
	if err != nil || !bytes.Equal(aSnapshot, bSnapshot) {
		t.Fatal("confirmed rollback or control restart changed snapshot", err)
	}
	if err := ctl.Do(ctx, "POST", "/v1/config/snapshots", control.SnapshotRequest{ExpectedRevision: 2}, nil); !localapi.IsHTTPStatus(err, 409) {
		t.Fatal("stale capture request was accepted after rollback", err)
	}
	t.Log("ROUTERD_CONFIG_SNAPSHOT_DAEMON_TEST_PASS: actual CLI, confirmed-only capture, stable store identity, immutable snapshot after rollback and control SIGKILL")
	observationSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	observationOutput, err := observationSession.CombinedOutput("show observations after 0 limit 16")
	_ = observationSession.Close()
	var operatorHistory control.ObservationPage
	if err != nil || json.Unmarshal(observationOutput, &operatorHistory) != nil {
		t.Fatalf("SSH observation history: %v %s", err, observationOutput)
	}
	c2, _ := json.Marshal(operatorHistory.Records)
	if !bytes.Equal(a, c2) {
		t.Fatal("SSH observations differ from durable API history after control recovery")
	}
	// Discover the saved intent on the router after losing the operator's
	// reference file. The three management entrypoints return the same refs.
	if err := os.Remove(savedPath); err != nil {
		t.Fatal(err)
	}
	listSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	listOutput, err := listSession.CombinedOutput("show snapshots")
	_ = listSession.Close()
	var listed config.SnapshotList
	if err != nil || json.Unmarshal(listOutput, &listed) != nil || len(listed.Unreadable) != 0 || len(listed.Snapshots) != 1 || listed.Snapshots[0] != saved {
		t.Fatalf("SSH saved configuration discovery differs from captured intent: %v %s", err, listOutput)
	}
	discovered := listed.Snapshots[0]
	var apiSnapshots config.SnapshotList
	if err := ctl.Do(ctx, "GET", "/v1/config/snapshots", nil, &apiSnapshots); err != nil || len(apiSnapshots.Unreadable) != 0 || !slices.Equal(apiSnapshots.Snapshots, listed.Snapshots) {
		t.Fatal("snapshot API differs from SSH discovery", err, apiSnapshots)
	}
	listCommand := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "snapshots", "--runtime-dir="+runDir)
	localListOutput, err := listCommand.CombinedOutput()
	var localSnapshots config.SnapshotList
	if err != nil || json.Unmarshal(localListOutput, &localSnapshots) != nil || len(localSnapshots.Unreadable) != 0 || !slices.Equal(localSnapshots.Snapshots, listed.Snapshots) {
		t.Fatalf("local snapshot listing differs from SSH discovery: %v %s", err, localListOutput)
	}
	discoveredJSON, _ := json.Marshal(discovered)
	if err := os.WriteFile(savedPath, discoveredJSON, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("SSH show snapshots, API and local CLI rediscovered saved revision %d after control restart and loss of the local reference file", discovered.Revision)
	// A saved reference remains usable after the real control restart. Restore
	// through the same transaction owner, never by replacing its journal file.
	peerAnswer := func(want string) {
		t.Helper()
		if err := fixtureInsideNamespace(peer, func() error {
			answerAddress(t, fmt.Sprintf("198.18.0.1:%d", dnsPort), want)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctl.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: recovered.Revision, RequestID: "fixture-before-restore", Config: changed}, &state); err != nil {
		t.Fatal(err)
	}
	peerAnswer("192.0.2.3")
	restoreRequest := control.RestoreRequest{Snapshot: discovered, ExpectedRevision: recovered.Revision, RequestID: "fixture-stale-restore"}
	if err := ctl.Do(ctx, "POST", "/v1/config/restore", restoreRequest, nil); !localapi.IsHTTPStatus(err, http.StatusConflict) {
		t.Fatal("restore accepted stale current revision", err)
	}
	peerAnswer("192.0.2.3")
	restoreRequest.ExpectedRevision = state.Revision
	restoreRequest.RequestID = "fixture-restore"
	restoreRequest.ConfirmTimeoutSeconds = 5
	restoreSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	restoreBody, _ := json.Marshal(restoreRequest)
	restoreSession.Stdin = bytes.NewReader(restoreBody)
	restoreOutput, err := restoreSession.CombinedOutput("restore")
	_ = restoreSession.Close()
	var restored control.State
	if err != nil || json.Unmarshal(restoreOutput, &restored) != nil || restored.Revision != state.Revision+1 || restored.Confirmation == nil {
		t.Fatalf("SSH restore did not apply saved configuration as a new confirmed trial: %v %s", err, restoreOutput)
	}
	peerAnswer("192.0.2.2")
	var replayed control.State
	if err := ctl.Do(ctx, "POST", "/v1/config/restore", restoreRequest, &replayed); err != nil || replayed.Revision != restored.Revision || replayed.Confirmation == nil || !replayed.Confirmation.Deadline.Equal(restored.Confirmation.Deadline) {
		t.Fatal("restore replay changed revision or original confirmation deadline", err, replayed.Revision)
	}
	var observedRevision uint64
	until(t, 12*time.Second, func() bool {
		state = control.State{}
		if err := ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
			return false
		}
		if state.Revision != observedRevision {
			var deadline time.Time
			if state.Confirmation != nil {
				deadline = state.Confirmation.Deadline
			}
			t.Logf("restore confirmation transition: revision=%d pending=%v deadline=%s healthy=%v last_error=%q", state.Revision, state.Confirmation != nil, deadline, state.Healthy, state.LastError)
			observedRevision = state.Revision
		}
		return state.Revision == restored.Revision+1 && state.Confirmation == nil && state.Healthy
	})
	peerAnswer("192.0.2.3")
	restoreRevision := state.Revision
	restoreCommand := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "config", "restore", "--snapshot="+savedPath, "--runtime-dir="+runDir, "--expected-revision="+fmt.Sprint(restoreRevision), "--request-id=fixture-restore-confirmed")
	finalRestoreOutput, err := restoreCommand.CombinedOutput()
	if err != nil || json.Unmarshal(finalRestoreOutput, &state) != nil || state.Revision != restoreRevision+1 || state.Confirmation != nil {
		t.Fatalf("local restore CLI could not restore confirmed intent: %v %s", err, finalRestoreOutput)
	}
	peerAnswer("192.0.2.2")
	t.Logf("SSH snapshot/restore, API replay and local restore CLI: saved revision %d restored after control restart as current revision %d; stale revision refused; unchanged five-second confirmation deadline reverted the trial; independent UDP/TCP DNS matched each transition", saved.Revision, state.Revision)
	// The operator reads current finite DerivedEvent state through real SSH;
	// it must not fetch the producer's durable historical asserted value.
	state = control.State{}
	if err = ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	literal := api.DerivedEventSpec{Topic: "routerd.fixture.literal", Inputs: []api.ReadyWhenSpec{{Field: "literal", NotEmpty: true}}, EmitInitial: true}
	state.Config.Resources = append(state.Config.Resources, resourceprogram.Resource{ID: "derived/literal", DerivedEvent: &literal})
	if err = ctl.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "fixture-live-derived", Config: state.Config}, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 10*time.Second, func() bool {
		var view derivedcontroller.Observation
		return ctl.Do(ctx, "GET", "/v1/derived/derived%2Fliteral", nil, &view) == nil && view.State.Asserted != nil && *view.State.Asserted
	})
	derivedSession, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	derivedOutput, err := derivedSession.CombinedOutput("show derived derived/literal")
	_ = derivedSession.Close()
	var derivedView derivedcontroller.Observation
	if err != nil || json.Unmarshal(derivedOutput, &derivedView) != nil || derivedView.ID != "derived/literal" || derivedView.State.Asserted == nil || !*derivedView.State.Asserted {
		t.Fatalf("SSH current derived view: %v %s", err, derivedOutput)
	}
	t.Log("real SSH show derived returned current finite status for a resource ID containing slash")
	beforeRestart := openSSHState()
	previousPID := daemon.Process.Pid
	_ = client.Close()
	stopDaemon()
	daemon, exited = startDaemon()
	until(t, 15*time.Second, func() bool { return status().Ready })
	until(t, 5*time.Second, func() bool {
		var health control.Health
		return ctl.Do(ctx, "GET", "/v1/health", nil, &health) == nil && health.Healthy
	})
	afterRestart := openSSHState()
	if afterRestart.Revision != beforeRestart.Revision {
		t.Fatalf("whole-product restart lost configuration: revision %d -> %d", beforeRestart.Revision, afterRestart.Revision)
	}
	peerAnswer("192.0.2.2")
	t.Logf("whole-product restart %d -> %d: independent OpenSSH reauthenticated against the unchanged source host identity; revision %d and independent UDP/TCP DNS retained", previousPID, daemon.Process.Pid, afterRestart.Revision)
	// Removal must release the actual bound sockets, not merely hide status.
	state = control.State{}
	if err = ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	request = control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "fixture-remove", Config: config.Document{SchemaVersion: config.SchemaVersion}}
	if err = ctl.Do(ctx, "POST", "/v1/config/commit", request, nil); err != nil {
		t.Fatal(err)
	}
	until(t, 5*time.Second, func() bool {
		for _, m := range status().Modules {
			if strings.HasPrefix(m.Name, "service.") {
				return false
			}
		}
		return true
	})
	replacement, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", dnsPort))
	if err != nil {
		t.Fatal("removed DNS retained its port", err)
	}
	replacement.Close()
}
func port(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}
func until(t *testing.T, d time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition timed out after", d)
}
func answer(t *testing.T, port int, want string) {
	t.Helper()
	answerAddress(t, fmt.Sprintf("127.0.0.1:%d", port), want)
}
func answerAddress(t *testing.T, address, want string) {
	t.Helper()
	q := new(mdns.Msg)
	q.SetQuestion("router.lab.example.", mdns.TypeA)
	for _, transport := range []string{"udp", "tcp"} {
		r, _, e := (&mdns.Client{Net: transport, Timeout: time.Second}).Exchange(q, address)
		if e != nil || r == nil || len(r.Answer) != 1 {
			t.Fatalf("DNS %s answer: %v %v", transport, r, e)
		}
		a, ok := r.Answer[0].(*mdns.A)
		if !ok || a.A.String() != want {
			t.Fatalf("DNS %s got %v want %s", transport, r.Answer, want)
		}
	}
}
