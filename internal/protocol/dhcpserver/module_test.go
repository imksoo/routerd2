//go:build linux

package dhcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
)

type fakeTransport struct {
	in     chan packet
	out    chan response
	closed atomic.Bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{in: make(chan packet, 16), out: make(chan response, 16)}
}
func (f *fakeTransport) Receive(ctx context.Context) (packet, error) {
	select {
	case p := <-f.in:
		return p, nil
	case <-ctx.Done():
		return packet{}, ctx.Err()
	}
}
func (f *fakeTransport) Send(ctx context.Context, r response) error {
	select {
	case f.out <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fakeTransport) Probe(context.Context, netip.Addr, net.HardwareAddr) (bool, error) {
	return false, nil
}
func (f *fakeTransport) Check() error { return nil }
func (f *fakeTransport) Close() error { f.closed.Store(true); return nil }
func awaitResponse(t *testing.T, f *fakeTransport) response {
	t.Helper()
	select {
	case r := <-f.out:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("DHCP reply timeout")
		return response{}
	}
}
func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("DHCP signal timeout")
	}
}

func TestRuntimeReadinessAndDurableEffectsGate(t *testing.T) {
	c := testConfig(t)
	c.SocketPath = filepath.Join(filepath.Dir(c.StatePath), "control.sock")
	c.StatusPath = filepath.Join(filepath.Dir(c.StatePath), "status.json")
	c.EventPath = filepath.Join(filepath.Dir(c.StatePath), "events.ndjson")
	f := newFakeTransport()
	initialEntered, initialPermit := make(chan struct{}), make(chan struct{})
	leaseEntered, leasePermit := make(chan struct{}), make(chan struct{})
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var initialOnce, leaseOnce sync.Once
	var mu sync.Mutex
	var snapshots []Snapshot
	effects := EffectsFunc(func(ctx context.Context, s Snapshot) error {
		if err := dhcpserverconfig.ValidateSnapshot(c, s); err != nil {
			return err
		}
		mu.Lock()
		snapshots = append(snapshots, s)
		mu.Unlock()
		if s.Active && s.Sequence == 1 {
			initialOnce.Do(func() { close(initialEntered) })
			select {
			case <-initialPermit:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if s.Active && len(s.Leases) > 0 {
			leaseOnce.Do(func() { close(leaseEntered) })
			select {
			case <-leasePermit:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	go func() {
		done <- run(ctx, c, effects, func() { close(ready) }, func(Config) (transport, error) { return f, nil })
	}()
	awaitSignal(t, initialEntered)
	select {
	case <-ready:
		t.Fatal("ready before initial effects ACK")
	default:
	}
	close(initialPermit)
	awaitSignal(t, ready)
	info, err := os.Stat(c.SocketPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private socket: %v %v", info, err)
	}
	f.in <- requestPacket(msgDiscover, 9, testMAC, map[byte][]byte{61: testID}, "")
	offered := awaitResponse(t, f)
	m := responseMessage(t, &offered, msgOffer)
	f.in <- requestPacket(msgRequest, 9, testMAC, map[byte][]byte{61: testID, 50: m.YIAddr.AsSlice(), 54: {192, 0, 2, 1}}, "")
	awaitSignal(t, leaseEntered)
	var persisted journal
	data, err := os.ReadFile(c.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &persisted); err != nil || len(persisted.Leases) != 1 {
		t.Fatalf("lease not durable before effects: %v %s", err, data)
	}
	select {
	case <-f.out:
		t.Fatal("ACK sent before lease effects ACK")
	default:
	}
	close(leasePermit)
	ack := awaitResponse(t, f)
	responseMessage(t, &ack, msgACK)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.SocketPath)
	}}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	r, err := client.Get("http://unix/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	var status Snapshot
	err = json.NewDecoder(r.Body).Decode(&status)
	r.Body.Close()
	if err != nil || len(status.Leases) != 1 || !status.Active {
		t.Fatalf("native status: %+v %v", status, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not terminate")
	}
	mu.Lock()
	last := snapshots[len(snapshots)-1]
	mu.Unlock()
	if last.Active || len(last.Leases) != 0 || last.ExpiresNanos <= last.ObservedNanos || !f.closed.Load() {
		t.Fatalf("shutdown did not withdraw: %+v", last)
	}
	if _, err := os.Lstat(c.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private socket leaked")
	}
	if err := json.Unmarshal(mustRead(t, c.StatePath), &persisted); err != nil || len(persisted.Leases) != 1 {
		t.Fatal("shutdown discarded durable binding", err)
	}
	if _, err := os.Stat(c.EventPath); err != nil {
		t.Fatal(err)
	}
}

func TestEffectsFailureNeverAcknowledgesClient(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "unavailable"
		if late {
			name = "late-success"
		}
		t.Run(name, func(t *testing.T) { testEffectsFailureNeverAcknowledgesClient(t, late) })
	}
}

func testEffectsFailureNeverAcknowledgesClient(t *testing.T, late bool) {
	t.Helper()
	c := testConfig(t)
	f := newFakeTransport()
	ready := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	effects := EffectsFunc(func(_ context.Context, s Snapshot) error {
		if s.Active && len(s.Leases) > 0 {
			if late {
				time.Sleep(dhcpserverconfig.ObservationLifetime + 50*time.Millisecond)
				return nil
			}
			return errors.New("control unavailable")
		}
		return nil
	})
	go func() {
		done <- run(ctx, c, effects, func() { close(ready) }, func(Config) (transport, error) { return f, nil })
	}()
	awaitSignal(t, ready)
	f.in <- requestPacket(msgDiscover, 9, testMAC, map[byte][]byte{61: testID}, "")
	offered := awaitResponse(t, f)
	m := responseMessage(t, &offered, msgOffer)
	f.in <- requestPacket(msgRequest, 9, testMAC, map[byte][]byte{61: testID, 50: m.YIAddr.AsSlice(), 54: {192, 0, 2, 1}}, "")
	select {
	case err := <-done:
		want := "control unavailable"
		if late {
			want = "original observation expired"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("failed effects did not stop worker")
	}
	select {
	case <-f.out:
		t.Fatal("ACK sent for unapplied lease effect")
	default:
	}
	var persisted journal
	if err := json.Unmarshal(mustRead(t, c.StatePath), &persisted); err != nil || len(persisted.Leases) != 1 {
		t.Fatal("failed ACK discarded allocation exclusion")
	}
}
func TestInitialEffectsFailureDoesNotBecomeReady(t *testing.T) {
	c := testConfig(t)
	f := newFakeTransport()
	ready := false
	err := run(context.Background(), c, EffectsFunc(func(_ context.Context, s Snapshot) error {
		if s.Active {
			return errors.New("initial failure")
		}
		return nil
	}), func() { ready = true }, func(Config) (transport, error) { return f, nil })
	if err == nil || ready || !f.closed.Load() {
		t.Fatalf("readiness on failure: %v %v", err, ready)
	}
}
func TestDurableRestartPreservesLeaseAndRejectsCorruptOwnership(t *testing.T) {
	c := testConfig(t)
	now := time.Now().UTC()
	store, j, err := openStore(c, now)
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(c, j, func() time.Time { return now }, store.Save)
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) { return false, nil }
	l, _ := acquire(t, e, testMAC, testID, 7)
	if _, _, err := openStore(c, now); err == nil {
		t.Fatal("duplicate journal ownership accepted")
	}
	store.Close()
	now = now.Add(time.Second)
	store, j, err = openStore(c, now)
	if err != nil {
		t.Fatal(err)
	}
	e = newEngine(c, j, func() time.Time { return now }, store.Save)
	e.probe = func(context.Context, netip.Addr, net.HardwareAddr) (bool, error) {
		t.Fatal("unexpired known lease was reprobed")
		return false, nil
	}
	p := requestPacket(msgRequest, 8, testMAC, map[byte][]byte{61: testID, 50: netip.MustParseAddr(l.IPAddress).AsSlice()}, "")
	r, err := e.handle(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ack := responseMessage(t, r, msgACK)
	if ack.YIAddr.String() != l.IPAddress || !e.leases[leaseKey(l)].AcquiredAt.Equal(l.AcquiredAt) {
		t.Fatal("restart changed allocation")
	}
	store.Close()
	c.Interface = "other0"
	if _, _, err := openStore(c, now); err == nil {
		t.Fatal("journal used on another interface")
	}
	c.Interface = "d4server"
	b := mustRead(t, c.StatePath)
	if err := os.WriteFile(c.StatePath, append(b, []byte("{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStore(c, now); err == nil {
		t.Fatal("trailing journal JSON accepted")
	}
}
func TestJournalAndSocketRefuseSymlinksAndUnsafeParents(t *testing.T) {
	c := testConfig(t)
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, c.StatePath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStore(c, time.Now()); err == nil {
		t.Fatal("journal symlink followed")
	}
	if string(mustRead(t, target)) != "untouched" {
		t.Fatal("target modified")
	}
	if err := os.Remove(c.StatePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(c.StatePath), 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStore(c, time.Now()); err == nil {
		t.Fatal("public state parent accepted")
	}
	if err := os.Chmod(filepath.Dir(c.StatePath), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(filepath.Dir(c.StatePath), "control.sock")
	if err := os.WriteFile(socket, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenPrivate(socket); err == nil {
		t.Fatal("regular file replaced by socket")
	}
}
func TestEventCursorAndDiskRotationAreBounded(t *testing.T) {
	o := &observation{epoch: "current"}
	for i := 0; i < 300; i++ {
		o.addEvent(Event{Type: "bound"})
	}
	for _, query := range []string{"?epoch=old&after=299", "?epoch=current&after=1"} {
		w := httptest.NewRecorder()
		o.handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/events"+query, nil))
		var result struct {
			Events []Event
			Gap    bool
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Gap || len(result.Events) != 256 {
			t.Fatalf("cursor lost gap: %+v", result)
		}
	}
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 1<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := appendEvent(path, Event{Type: "test"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path + ".1"); err != nil || info.Size() != 1<<20 {
		t.Fatal("event archive missing")
	}
	if info, err := os.Stat(path); err != nil || info.Size() > 4096 || info.Mode().Perm() != 0600 {
		t.Fatal("event file did not rotate privately")
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotLimitPreventsReadinessAndPublication(t *testing.T) {
	c := testConfig(t)
	c.MaxLeases = 2048
	c.Primary.Prefix = "10.0.0.0/8"
	c.Primary.Start = "10.0.0.2"
	c.Primary.End = "10.255.255.254"
	c.Primary.Router = "10.0.0.1"
	c.ServerAddress = "10.0.0.1"
	now := time.Now().UTC()
	store, _, err := openStore(c, now)
	if err != nil {
		t.Fatal(err)
	}
	e := newTestEngine(t, c, &now, store.Save)
	hostname := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for i := 0; i < 2048; i++ {
		id := strings.Repeat("ab", 253) + stringHex(uint16(i))
		l := Lease{ClientID: id, MACAddress: "02:00:00:00:00:01", IPAddress: numberIP(0x0a000002 + uint32(i)).String(), Hostname: hostname, Scope: "default", AcquiredAt: now, RenewedAt: now, ExpiresAt: now.Add(time.Minute)}
		e.leases[leaseKey(l)] = storedLease{Lease: l, HeldUntil: l.ExpiresAt}
	}
	// Keep allocation exclusions durable even when a full source publication
	// cannot fit IPC. Run must fail before authorizing any reply or readiness.
	if err := e.save(now); err != nil {
		t.Fatal(err)
	}
	store.Close()
	f := newFakeTransport()
	ready, published := false, false
	err = run(context.Background(), c, EffectsFunc(func(_ context.Context, s Snapshot) error {
		if s.Active {
			published = true
		}
		return nil
	}), func() { ready = true }, func(Config) (transport, error) { return f, nil })
	if err == nil || !strings.Contains(err.Error(), "IPC bound") || ready || published || !f.closed.Load() {
		t.Fatal("oversized snapshot reached publication or readiness", err, ready, published)
	}
}
func stringHex(v uint16) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[v>>12], digits[(v>>8)&15], digits[(v>>4)&15], digits[v&15]})
}
