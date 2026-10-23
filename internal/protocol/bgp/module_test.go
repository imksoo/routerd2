package bgp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/localapi"
)

func moduleConfig(t *testing.T) Config {
	t.Helper()
	dir, err := os.MkdirTemp("", "r2bgp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return Normalize(Config{Resource: "BGPRouter/test", SocketPath: filepath.Join(dir, "control.sock"), GRPCSocketPath: filepath.Join(dir, "grpc.sock"), StatePath: filepath.Join(dir, "applied.json"), StatusPath: filepath.Join(dir, "status.json"), EventPath: filepath.Join(dir, "events.jsonl"), Runtime: bgpdaemon.AppliedConfig{Global: bgpdaemon.AppliedGlobal{ASN: 64512, RouterID: "192.0.2.1", ListenPort: -1}, Advertisements: []string{"10.20.0.0/24", "2001:db8:20::/64"}}})
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestJournalRestoresDynamicPathsOnlyForSameCommittedIntent(t *testing.T) {
	cfg := moduleConfig(t)
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	applied, _, err := readApplied(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	applied.Paths = append(applied.Paths, bgpdaemon.AppliedPath{Source: "MobilityPool/lab/node/a", Prefix: "10.99.0.8/32", UUID: "1234"})
	if err := writeApplied(cfg.StatePath, applied); err != nil {
		t.Fatal(err)
	}
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	restored, _, _ := readApplied(cfg.StatePath)
	if len(bgpdaemon.NonStaticPaths(restored.Paths)) != 1 {
		t.Fatal("matching intent did not retain dynamic path")
	}
	record, _, _ := readState(cfg.StatePath)
	if record.Owner == nil || record.Owner.Digest != committedDigest(cfg) {
		t.Fatal("write lost atomic ownership metadata")
	}
	cfg.Runtime.Global.RouterID = "192.0.2.3"
	if err := initializeState(cfg); err != nil {
		t.Fatal(err)
	}
	restored, _, _ = readApplied(cfg.StatePath)
	if len(bgpdaemon.NonStaticPaths(restored.Paths)) != 0 || restored.Global.RouterID != "192.0.2.3" {
		t.Fatal("old dynamic intent leaked into new configuration")
	}
	fi, _ := os.Stat(cfg.StatePath)
	if fi.Mode().Perm() != 0600 {
		t.Fatal("applied state permissions", fi.Mode())
	}
}

func TestStateRefusesSymlinksAndDoesNotTouchTarget(t *testing.T) {
	cfg := moduleConfig(t)
	target := cfg.StatePath + "-target"
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	if err := initializeState(cfg); err == nil {
		t.Fatal("followed state symlink")
	}
	if err := atomicJSON(cfg.StatePath, Snapshot{}); err == nil {
		t.Fatal("replaced state symlink")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "keep" {
		t.Fatal("symlink target changed")
	}
}

func TestRunWaitsForOwnershipAndPublishesShutdownWithdrawal(t *testing.T) {
	cfg := moduleConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	ready := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	var mu sync.Mutex
	var snapshots []Snapshot
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if s.Active {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		mu.Lock()
		snapshots = append(snapshots, s)
		mu.Unlock()
		return nil
	})
	go func() { done <- Run(ctx, cfg, effects, func() { close(ready) }) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial ownership callback did not run")
	}
	select {
	case <-ready:
		t.Fatal("ready before ownership acknowledgment")
	default:
	}
	close(release)
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("readiness did not arrive")
	}
	for _, path := range []string{cfg.SocketPath, cfg.GRPCSocketPath, cfg.StatePath, cfg.StatusPath, cfg.EventPath} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("private path %s: %v %v", path, fi, err)
		}
	}
	client := localapi.NewClient(cfg.SocketPath)
	var status Snapshot
	if err := client.Do(ctx, "GET", "/v1/status", nil, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Active || status.Epoch == "" || status.Sequence == 0 || !time.Now().Before(status.ValidUntil) {
		t.Fatalf("status %#v", status)
	}
	if len(status.Routes) != 0 {
		t.Fatal("locally originated paths leaked into learned FIB routes", status.Routes)
	}
	var paths []bgpdaemon.AppliedPath
	if err := client.Do(ctx, "GET", "/v1/paths", nil, &paths); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0].UUID == "" || paths[1].UUID == "" {
		t.Fatalf("restored IPv4+IPv6 paths %#v", paths)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("shutdown exceeded bound")
	}
	mu.Lock()
	defer mu.Unlock()
	last := snapshots[len(snapshots)-1]
	if last.Active || len(last.Routes) != 0 || !last.ValidUntil.IsZero() || last.Sequence <= status.Sequence {
		t.Fatalf("missing withdrawal %#v", last)
	}
	b, err := os.ReadFile(cfg.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &last); err != nil || last.Active {
		t.Fatalf("atomic final status: %s (%v)", b, err)
	}
}

func TestFailedInitialOwnershipNeverBecomesReady(t *testing.T) {
	cfg := moduleConfig(t)
	ready, withdrew := false, false
	err := Run(context.Background(), cfg, EffectsFunc(func(_ context.Context, s Snapshot) error {
		if s.Active {
			return errors.New("writer rejected routes")
		}
		withdrew = true
		return nil
	}), func() { ready = true })
	if err == nil || !strings.Contains(err.Error(), "writer rejected routes") || ready || !withdrew {
		t.Fatalf("err=%v ready=%t withdrew=%t", err, ready, withdrew)
	}
}

func TestStopEngineHasBoundedDeadline(t *testing.T) {
	block, done := make(chan struct{}), make(chan struct{})
	err := stopEngine(func() { <-block }, done, 10*time.Millisecond)
	close(block)
	close(done)
	if err == nil {
		t.Fatal("unbounded shutdown accepted")
	}
}

func TestIPv6PolicyNamesCannotAliasCompressedAddresses(t *testing.T) {
	if policyPeerName("2001:db8::1") == policyPeerName("2001:db8:1::") {
		t.Fatal("distinct IPv6 neighbor policy scopes collide")
	}
}

func TestNativeRestartRestoresDynamicPathsWithFreshUUIDs(t *testing.T) {
	cfg := moduleConfig(t)
	start := func() (func(), *localapi.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() {
			done <- Run(ctx, cfg, EffectsFunc(func(context.Context, Snapshot) error { return nil }), func() { close(ready) })
		}()
		select {
		case <-ready:
		case err := <-done:
			cancel()
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("restart readiness timed out")
		}
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(8 * time.Second):
					t.Error("restart shutdown timed out")
				}
			})
		}
		t.Cleanup(stop)
		return stop, localapi.NewClient(cfg.SocketPath)
	}
	stop, client := start()
	path := bgpdaemon.AppliedPath{Source: "MobilityPool/test/node/a", Prefix: "10.99.0.8/32"}
	var created bgpdaemon.AppliedPath
	if err := client.Do(context.Background(), "POST", "/v1/paths", path, &created); err != nil {
		t.Fatal(err)
	}
	if created.UUID == "" {
		t.Fatal("native add did not return UUID")
	}
	stop()
	stop, client = start()
	var restored []bgpdaemon.AppliedPath
	if err := client.Do(context.Background(), "GET", "/v1/paths?source=MobilityPool/test/node/a", nil, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0].UUID == "" || restored[0].UUID == created.UUID {
		t.Fatalf("native restart did not replace path UUID: %#v", restored)
	}
	stop()
	cfg.Runtime.Global.RouterID = "192.0.2.9"
	stop, client = start()
	if err := client.Do(context.Background(), "GET", "/v1/paths?source=MobilityPool/test/node/a", nil, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatal("changed intent restored old dynamic path", restored)
	}
	stop()
}
