//go:build linux && integration

// Package integration exercises the real two-binary process tree. Every test
// that changes a kernel interface first reexecs into a new user/network namespace.
package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	mdns "github.com/miekg/dns"
	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

func TestDaemonConfigurationAndRecovery(t *testing.T) {
	if os.Getenv("ROUTERD2_DAEMON_CHILD") != "1" {
		binaryDir := os.Getenv("ROUTERD2_TEST_BIN_DIR")
		if binaryDir == "" {
			if os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") == "1" {
				t.Fatal("ROUTERD2_TEST_BIN_DIR must point to freshly built binaries")
			}
			t.Skip("set ROUTERD2_TEST_BIN_DIR to run the real process tree")
		}
		parent, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestDaemonConfigurationAndRecovery$", "-test.v", "-test.timeout=75s")
		cmd.Env = append(os.Environ(), "ROUTERD2_DAEMON_CHILD=1", "ROUTERD2_DAEMON_PARENT="+parent)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNET, UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}, GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
		out, err := cmd.CombinedOutput()
		if err != nil {
			if (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)) && os.Getenv("ROUTERD2_INTEGRATION_REQUIRED") != "1" {
				t.Skipf("namespace isolation unavailable: %v", err)
			}
			t.Fatalf("isolated daemon fixture: %v\n%s", err, out)
		}
		t.Logf("%s", out)
		return
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil || current == os.Getenv("ROUTERD2_DAEMON_PARENT") || os.Getenv("ROUTERD2_DAEMON_PARENT") == "" {
		t.Fatal("refusing daemon fixture outside isolated network namespace")
	}
	loop, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	if err = netlink.LinkSetUp(loop); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "r2-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	runDir, stateDir := filepath.Join(dir, "run"), filepath.Join(dir, "state")
	dnsPort, sshPort := port(t), port(t)
	dns := dnsconfig.RuntimeConfig{Resource: "dns", Spec: api.DNSResolverSpec{Listen: []api.DNSResolverListenSpec{{Name: "local", Addresses: []string{"127.0.0.1"}, Port: dnsPort}}, Sources: []api.DNSResolverSourceSpec{{Name: "zones", Kind: "zone", Match: []string{"lab.example"}, ZoneRef: []string{"DNSZone/lan-zone"}}}}, Zones: []dnsconfig.RuntimeZone{{Name: "lan-zone", Spec: api.DNSZoneSpec{Zone: "lab.example", Records: []api.DNSZoneRecordSpec{{Hostname: "router", IPv4: "192.0.2.1"}}}}}}
	doc := config.Document{SchemaVersion: 1, Services: []serviceconfig.Service{{Name: "dns", DNS: &dns}}}
	configPath := filepath.Join(dir, "config.json")
	if err = config.AtomicJSON(configPath, doc); err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeconfig.Boot{Platform: "host", Config: configPath, ModulePath: filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd-modules"), RuntimeDir: runDir, StateDir: stateDir, SSH: runtimeconfig.SSH{Listen: fmt.Sprintf("127.0.0.1:%d", sshPort), User: "admin", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}}}
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
	daemon := exec.Command(filepath.Join(os.Getenv("ROUTERD2_TEST_BIN_DIR"), "routerd"), "boot", "--file="+boot)
	daemon.Stdout = log
	daemon.Stderr = log
	if err = daemon.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- daemon.Wait() }()
	defer func() {
		_ = daemon.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(12 * time.Second):
			_ = daemon.Process.Kill()
			<-exited
			t.Error("daemon failed to drain its workers")
		}
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
	answer(t, dnsPort, "192.0.2.1")
	// The SSH channel drives the real candidate editor and control transaction.
	hostBytes, err := os.ReadFile(filepath.Join(stateDir, "ssh_host_ed25519_key"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.ParsePrivateKey(hostBytes)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", b.SSH.Listen, &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
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
	_, err = io.WriteString(input, "configure\rset services[dns].dns.zones[lan-zone].spec.records[0].ipv4 \"192.0.2.2\"\rvalidate\rcommit confirmed 5\rconfirm\rexit\rexit\r")
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
	var state control.State
	state = control.State{}
	if err = ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || state.Confirmation != nil {
		t.Fatalf("candidate not committed and confirmed: %+v; transcript=%s", state, transcript.String())
	}
	answer(t, dnsPort, "192.0.2.2")
	// An unconfirmed record change must roll back durably and restart only DNS.
	changedBytes, _ := json.Marshal(state.Config)
	var changed config.Document
	_ = json.Unmarshal(changedBytes, &changed)
	changed.Services[0].DNS.Zones[0].Spec.Records[0].IPv4 = "192.0.2.3"
	request := control.CommitRequest{ExpectedRevision: 2, RequestID: "fixture-rollback", Config: changed, ConfirmTimeoutSeconds: 5}
	if err = ctl.Do(ctx, "POST", "/v1/config/commit", request, &state); err != nil {
		t.Fatal(err)
	}
	answer(t, dnsPort, "192.0.2.3")
	until(t, 12*time.Second, func() bool {
		state = control.State{}
		_ = ctl.Do(ctx, "GET", "/v1/config", nil, &state)
		return state.Revision == 4 && state.Confirmation == nil && state.Healthy
	})
	answer(t, dnsPort, "192.0.2.2")
	// Killing logging cannot interrupt DNS. Killing control reacquires workers
	// from the committed revision and does not lose the confirmed configuration.
	for _, name := range []string{"logging", "control"} {
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
	}
	// Removal must release the actual bound sockets, not merely hide status.
	state = control.State{}
	if err = ctl.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
		t.Fatal(err)
	}
	request = control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "fixture-remove", Config: config.Document{SchemaVersion: 1}}
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
	q := new(mdns.Msg)
	q.SetQuestion("router.lab.example.", mdns.TypeA)
	for _, transport := range []string{"udp", "tcp"} {
		r, _, e := (&mdns.Client{Net: transport, Timeout: time.Second}).Exchange(q, fmt.Sprintf("127.0.0.1:%d", port))
		if e != nil || r == nil || len(r.Answer) != 1 {
			t.Fatalf("DNS %s answer: %v %v", transport, r, e)
		}
		a, ok := r.Answer[0].(*mdns.A)
		if !ok || a.A.String() != want {
			t.Fatalf("DNS %s got %v want %s", transport, r.Answer, want)
		}
	}
}
