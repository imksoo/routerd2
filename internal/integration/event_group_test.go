//go:build linux && integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/eventd"
	"github.com/imksoo/routerd/pkg/federation"
	routerstate "github.com/imksoo/routerd/pkg/state"
	"github.com/imksoo/routerd2/internal/buildinfo"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// The product is PID 1 in a private mount/PID namespace inside the existing
// isolated network fixture. Signed selection, all workers and the old peer run
// as actual processes. No in-process control, transport or network fake is used.
func TestDaemonSignedEventGroup(t *testing.T) {
	if !isolateFixtureTimeout(t, t.Name(), 120*time.Second) {
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("signed appliance fixture requires root in its isolated network namespace")
	}
	binaryDir, oldBinary := os.Getenv("ROUTERD2_TEST_BIN_DIR"), os.Getenv("OLD_EVENTD")
	if oldBinary == "" {
		t.Fatal("OLD_EVENTD must name the independent routerd-eventd executable")
	}
	dir, err := os.MkdirTemp("", "r2-event-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0700)
			}
			return nil
		})
		os.RemoveAll(dir)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	wait := func(description string, seconds int, check func() bool) {
		t.Helper()
		until := time.Now().Add(time.Duration(seconds) * time.Second)
		for !check() {
			if time.Now().After(until) {
				t.Fatal(description)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	version, err := exec.Command(filepath.Join(binaryDir, "routerd"), "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal(version, &info); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := map[string]ed25519.PublicKey{release.KeyID(public): public}
	trustPath := filepath.Join(dir, "trust.json")
	if err := config.AtomicJSON(trustPath, trust); err != nil {
		t.Fatal(err)
	}
	releaseDir := filepath.Join(dir, "releases")
	store, err := release.Open(release.Options{Root: releaseDir, TrustedKeys: trust})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files := map[string]*os.File{}
	var artifacts []release.Artifact
	for _, name := range []string{"routerd", "routerd-modules", "routerd-logging"} {
		file, err := os.Open(filepath.Join(binaryDir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		files[name] = file
		artifacts = append(artifacts, release.Artifact{Name: name, Kind: "native", Command: name})
	}
	// Select the real roles needed by this configuration. External engines for
	// unrelated services are not part of this feature's release fixture.
	native := runtimeconfig.NativeWorkers("routerd-modules")
	var roles []release.RoleBinding
	for _, role := range []string{"logging", "network", "control", "management", "event_group"} {
		artifact := native[role]
		if artifact == "" {
			t.Fatal("missing native role", role)
		}
		roles = append(roles, release.RoleBinding{Role: role, Artifact: artifact})
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].Role < roles[j].Role })
	bundle, err := os.Create(filepath.Join(dir, "runtime.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	manifest, err := release.WriteBundle(ctx, bundle, release.BundleOptions{ReleaseID: info.ReleaseID, Sequence: 1, GOOS: info.GOOS, GOARCH: info.GOARCH, BootABI: 1, WorkerABI: 5, Entrypoint: "routerd", Artifacts: artifacts, Roles: roles}, private, files)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range manifest.Artifacts {
		t.Logf("signed product: release=%s artifact=%s sha256=%s", info.ReleaseID, a.Name, a.SHA256)
	}
	if _, err := bundle.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Stage(ctx, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Initialize(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	secretPath := filepath.Join(dir, "secret")
	if err = os.WriteFile(secretPath, []byte("isolated-event-product-key"), 0600); err != nil {
		t.Fatal(err)
	}
	nativePort, oldPort := port(t), port(t)
	nativeAddress, oldAddress := fmt.Sprintf("127.0.0.1:%d", nativePort), fmt.Sprintf("127.0.0.1:%d", oldPort)
	source := fmt.Sprintf(`apiVersion: routerd.net/v1alpha1
kind: Router
metadata: {name: events}
spec:
  resources:
    - apiVersion: mobility.routerd.net/v1alpha1
      kind: SAMNodeSet
      metadata: {name: nodes}
      spec:
        nodes:
          - {nodeRef: native, eventEndpoint: 'http://%s'}
          - {nodeRef: old, eventEndpoint: 'http://%s'}
    - apiVersion: federation.routerd.net/v1alpha1
      kind: EventGroup
      metadata: {name: fixture}
      spec:
        nodeName: native
        peersFrom: [{resource: SAMNodeSet/nodes}]
        auth: {mode: hmac, secretFile: %s}
        listen: {address: 127.0.0.1, port: %d}
        retention: {maxEvents: 1000, maxAge: 24h}
        replayWindow: 5m
`, nativeAddress, oldAddress, secretPath, nativePort)
	sourcePath := filepath.Join(dir, "source.yaml")
	if err = os.WriteFile(sourcePath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	converted, err := exec.Command(filepath.Join(binaryDir, "routerd"), "config", "migrate", sourcePath).Output()
	if err != nil {
		t.Fatalf("product migration: %v", err)
	}
	var migrated config.Document
	if err = config.Decode(bytes.NewReader(converted), &migrated); err != nil {
		t.Fatal(err)
	}
	serviceName := ""
	for _, r := range migrated.Resources {
		if r.Service != nil && r.Service.EventGroup != nil {
			serviceName = r.Service.Name
		}
	}
	if serviceName == "" {
		t.Fatal("product migration did not produce event_group")
	}
	configPath, runDir, stateDir := filepath.Join(dir, "config.json"), filepath.Join(dir, "run"), filepath.Join(dir, "state")
	if err = config.AtomicJSON(configPath, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	bootPath := filepath.Join(dir, "boot.json")
	boot := runtimeconfig.Boot{Platform: "appliance", Config: configPath, RuntimeDir: runDir, StateDir: stateDir, SSH: runtimeconfig.SSH{User: "admin"}, Release: &runtimeconfig.Release{Root: releaseDir, TrustFile: trustPath}}
	if err = config.AtomicJSON(bootPath, boot); err != nil {
		t.Fatal(err)
	}
	moduleName := "service." + serviceName
	logPath := filepath.Join(dir, "product.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	product := exec.CommandContext(ctx, "unshare", "--mount", "--pid", "--fork", "--mount-proc", "--kill-child=SIGKILL", "--propagation", "private", filepath.Join(binaryDir, "routerd"), "boot", "--file", bootPath)
	product.Stdout, product.Stderr = log, log
	if err = product.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if t.Failed() {
			var records logging.QueryResult
			_ = localapi.NewClient(filepath.Join(runDir, "logging.sock")).Do(context.Background(), "GET", "/v1/logs?module="+moduleName+"&level=WARN&limit=30", nil, &records)
			for _, r := range records.Records {
				t.Logf("product warning: %s %s %v", r.Module, r.Message, r.Attributes)
			}
		}
		cancel()
		_ = product.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Log(string(data))
		}
	}()
	local := localapi.NewClient(filepath.Join(runDir, "control.sock"))
	admin := localapi.NewClient(filepath.Join(runDir, "supervisor.sock"))
	wait("signed product did not become ready", 30, func() bool {
		var s supervisor.Status
		return admin.Do(ctx, "GET", "/v1/status", nil, &s) == nil && s.Ready && s.PID == 1 && s.ReleaseID == info.ReleaseID
	})
	var current control.State
	if err = local.Do(ctx, "GET", "/v1/config", nil, &current); err != nil {
		t.Fatal(err)
	}
	request := control.CommitRequest{ExpectedRevision: current.Revision, RequestID: "event-group-product", Config: migrated}
	if err = local.Do(ctx, "POST", "/v1/config/commit", request, nil); err != nil {
		t.Fatal(err)
	}
	module := func() supervisor.ModuleStatus {
		var status supervisor.Status
		if err := admin.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
			t.Fatalf("read supervisor status: %v", err)
		}
		for _, m := range status.Modules {
			if m.Name == moduleName {
				return m
			}
		}
		return supervisor.ModuleStatus{}
	}
	wait("configured product event_group worker did not become ready", 20, func() bool { return module().Ready })
	first := module()
	if first.Role != "event_group" || first.ReleaseID != info.ReleaseID || first.PID <= 1 {
		t.Fatal("unexpected worker", first)
	}
	t.Logf("signed PID 1 adopted migrated EventGroup: worker=%s pid=%d generation=%d", first.Name, first.PID, first.Generation)

	oldState, oldConfig := filepath.Join(dir, "old.db"), filepath.Join(dir, "old.json")
	peerCfg := eventd.Config{NodeName: "old", Group: "fixture", Listen: eventd.Listen{Address: "127.0.0.1", Port: oldPort}, SecretFile: secretPath, StatePath: oldState, Peers: []eventd.PeerConfig{{NodeName: "native", Endpoint: "http://" + nativeAddress}}, PushInterval: 100 * time.Millisecond}
	peerCfg.ApplyDefaults()
	body, err := eventd.MarshalConfigJSON(peerCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(oldConfig, body, 0600); err != nil {
		t.Fatal(err)
	}
	var old *exec.Cmd
	var stopOld context.CancelFunc
	startOld := func() {
		child, stop := context.WithCancel(ctx)
		stopOld = stop
		old = exec.CommandContext(child, oldBinary, "daemon", "--config-file", oldConfig)
		old.Stdout, old.Stderr = log, log
		if err := old.Start(); err != nil {
			t.Fatal(err)
		}
		wait("old eventd did not listen", 5, func() bool {
			response, err := http.Get("http://" + oldAddress + "/v1/status")
			if err != nil {
				return false
			}
			response.Body.Close()
			return true
		})
	}
	haltOld := func() { stopOld(); _ = old.Wait(); old = nil }
	defer func() {
		if old != nil {
			haltOld()
		}
	}()
	startOld()
	emit := func(id string, expiry time.Time) {
		t.Helper()
		wire := federation.Event{ID: id, Type: "fixture.local", Subject: "192.0.2.7/32", Payload: map[string]string{"proof": "product"}, ObservedAt: time.Now().UTC().Truncate(time.Second), ExpiresAt: expiry}
		if err := local.Do(ctx, "POST", "/v1/event-groups/"+moduleName+"/emit", wire, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitOld := func(id string) {
		t.Helper()
		wait("old peer did not receive "+id, 20, func() bool {
			db, err := routerstate.OpenSQLiteReadOnly(oldState)
			if err != nil {
				return false
			}
			defer db.Close()
			records, err := db.ListFederationEvents("fixture", true, time.Now().Unix())
			if err != nil {
				return false
			}
			for _, r := range records {
				if r.ID == id {
					return r.SourceNode == "native" && r.Payload["proof"] == "product"
				}
			}
			return false
		})
	}
	emit("product-first", time.Time{})
	waitOld("product-first")
	observed := time.Now().UTC().Truncate(time.Second)
	expires := observed.Add(60 * time.Second)
	peerState, err := routerstate.OpenSQLite(oldState)
	if err != nil {
		t.Fatal(err)
	}
	err = peerState.RecordFederationEvent(routerstate.EventRecord{ID: "old-first", Group: "fixture", SourceNode: "old", Type: "fixture.remote", Subject: "198.51.100.7/32", Payload: map[string]string{"proof": "old"}, ObservedAt: observed, ExpiresAt: expires})
	peerState.Close()
	if err != nil {
		t.Fatal(err)
	}
	wait("product did not durably accept old signed event", 10, func() bool {
		var cursor uint64
		for {
			var page control.ObservationPage
			if err := local.Do(ctx, "GET", fmt.Sprintf("/v1/observations?after_cursor=%d&limit=16", cursor), nil, &page); err != nil {
				t.Fatalf("read product observations: %v", err)
			}
			for _, r := range page.Records {
				if f := r.Event.Federation; f != nil && f.ID == "old-first" {
					if f.SourceNode != "old" || r.Event.Attributes["proof"] != "old" || !r.Event.ObservedAt.Equal(observed) || !r.Event.ExpiresAt.Equal(expires) {
						t.Fatalf("remote fact changed: %+v", r.Event)
					}
					return true
				}
			}
			if !page.More {
				return false
			}
			cursor = page.NextCursor
		}
	})
	t.Log("PASS actual product worker and independent old eventd exchanged signed events in both directions with original expiry")
	haltOld()
	outage := time.Now()
	emit("product-after-restart", time.Now().UTC().Add(time.Minute))
	logs := localapi.NewClient(filepath.Join(runDir, "logging.sock"))
	wait("product did not attempt the queued event before restart", 15, func() bool {
		var records logging.QueryResult
		if err := logs.Do(ctx, "GET", "/v1/logs?module="+moduleName+"&level=WARN&limit=100", nil, &records); err != nil {
			return false
		}
		for _, record := range records.Records {
			if record.Timestamp.After(outage) && record.Message == "event peer delivery pending" {
				return true
			}
		}
		return false
	})
	if output, err := exec.CommandContext(ctx, filepath.Join(binaryDir, "routerd"), "modules", "restart", moduleName, "--runtime-dir", runDir).CombinedOutput(); err != nil {
		t.Fatalf("product service restart: %v %s", err, output)
	}
	wait("supervised event_group restart did not complete", 15, func() bool { m := module(); return m.Ready && m.Generation > first.Generation && m.PID != first.PID })
	restarted := module()
	startOld()
	waitOld("product-after-restart")
	t.Logf("PASS pending signed event delivered after supervisor restart: worker pid=%d generation=%d", restarted.PID, restarted.Generation)
	if err = local.Do(ctx, "GET", "/v1/config", nil, &current); err != nil {
		t.Fatal(err)
	}
	if err = local.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: current.Revision, RequestID: "event-group-remove", Config: config.Document{SchemaVersion: config.SchemaVersion}}, nil); err != nil {
		t.Fatal(err)
	}
	wait("removed event_group worker remained", 10, func() bool { return module().Name == "" })
	t.Log("PASS configured removal stopped the actual product worker")
}
