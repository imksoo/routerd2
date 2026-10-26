//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package legacyconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"golang.org/x/sys/unix"
)

// This boundary test uses the public runtime API and a private temporary Unix
// supervisor fixture. Network intent is validated and captured in memory. It
// cannot run a process, open a network protocol socket, or mutate host networking.
type migrationNetwork struct {
	mu       sync.Mutex
	desired  network.Desired
	dadReady bool
}

func (n *migrationNetwork) Apply(_ context.Context, _ uint64, desired network.Desired) error {
	if err := network.Validate(desired); err != nil {
		return err
	}
	data, err := json.Marshal(desired)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.desired = network.Desired{}
	return json.Unmarshal(data, &n.desired)
}

func (n *migrationNetwork) state() network.Desired {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.desired // callers only read; Apply replaces rather than mutates it
}

func (n *migrationNetwork) Snapshot(context.Context) (network.Snapshot, error) {
	panic("AFTR source dependency must not retrieve the full network snapshot")
}

func (n *migrationNetwork) SourceAddress(_ context.Context, name, requested string) (network.SourceObservation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	observed := network.SourceObservation{Interface: name, RequestedAddress: requested}
	for index, iface := range n.desired.Interfaces {
		if iface.Name != name {
			continue
		}
		observed.InterfacePresent, observed.Index, observed.Up = true, index+1, true
		for _, address := range iface.Addresses {
			prefix, err := netip.ParsePrefix(address)
			if err != nil || prefix.Addr().String() != requested {
				continue
			}
			flags := 0
			if iface.Name == "eth0" && !n.dadReady {
				flags = unix.IFA_F_TENTATIVE
			}
			observed.Found = true
			observed.Address = &network.AddressState{Address: address, Owned: true, PreferredLifetime: 600, ValidLifetime: 1200, Flags: flags}
			return observed, nil
		}
	}
	return observed, nil
}

type migrationSupervisor struct {
	mu      sync.Mutex
	modules []supervisor.DynamicModule
}

func (m *migrationSupervisor) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch req.Method + " " + req.URL.Path {
	case "POST /v1/modules/reconcile":
		var set supervisor.ReconcileModules
		if err := localapi.Decode(w, req, &set); err != nil {
			localapi.Fail(w, http.StatusBadRequest, err)
			return
		}
		m.modules = append([]supervisor.DynamicModule(nil), set.Modules...)
		localapi.JSON(w, http.StatusOK, struct{}{})
	case "GET /v1/status":
		state := supervisor.Status{InstanceID: "11111111111111111111111111111111", Ready: true}
		for _, module := range m.modules {
			state.Modules = append(state.Modules, supervisor.ModuleStatus{Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest, Ready: true, State: "running", Generation: 1})
		}
		localapi.JSON(w, http.StatusOK, state)
	default:
		http.NotFound(w, req)
	}
}

func runtimeMigrationFixture(t *testing.T, document config.Document) (*runtimeplan.Runtime, *migrationNetwork, *migrationSupervisor) {
	t.Helper()
	// A short path avoids platform Unix-domain pathname limits regardless of
	// the Go test's long descriptive name. MkdirTemp creates private mode 0700.
	dir, err := os.MkdirTemp("", "routerd-migrate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	runtimeDir, stateDir := filepath.Join(dir, "run"), filepath.Join(dir, "state")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(runtimeDir, supervisor.SocketName))
	if err != nil {
		t.Fatal("private Unix supervisor fixture:", err)
	}
	manager := &migrationSupervisor{}
	server := &http.Server{Handler: manager, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error("private fixture server:", err)
		}
	})
	n := &migrationNetwork{}
	r := runtimeplan.New(runtimeDir, stateDir, n, "11111111111111111111111111111111")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.ApplyConfig(ctx, 1, document); err != nil {
		t.Fatal("native runtime rejected migration result:", err)
	}
	monitorCtx, stop := context.WithCancel(context.Background())
	monitorDone := make(chan struct{})
	go func() {
		r.Monitor(monitorCtx)
		close(monitorDone)
	}()
	t.Cleanup(func() { stop(); <-monitorDone })
	return r, n, manager
}

// Drive the public observation/owner contract and wait for its applied view.
func observeMigration(ctx context.Context, r *runtimeplan.Runtime, u serviceconfig.Update) error {
	if err := r.Observe(ctx, u); err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := r.CheckReady(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func migrationUpdate(t *testing.T, runtime *runtimeplan.Runtime, service serviceconfig.Service, sequence uint64, snapshot any) serviceconfig.Update {
	t.Helper()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range runtime.State().Instances {
		if instance.Name == service.ModuleName() {
			return serviceconfig.Update{SupervisorInstance: "11111111111111111111111111111111", Name: instance.Name, Role: instance.Role, ConfigDigest: instance.ConfigDigest, Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: sequence, Snapshot: data}
		}
	}
	t.Fatal("native runtime instance missing")
	return serviceconfig.Update{}
}

func migrationInterface(t *testing.T, desired network.Desired, name string) network.Interface {
	t.Helper()
	for _, iface := range desired.Interfaces {
		if iface.Name == name {
			return iface
		}
	}
	t.Fatal("projected interface missing:", name)
	return network.Interface{}
}

func TestMigratedWANGraphThroughPublicRuntimeProjection(t *testing.T) {
	result := compiledPolicy(t, fixture(t, "wan-dynamic"), wanPolicy(t))
	doc := result.Document
	raService, pdService, dsService := service(t, doc, "ra_client"), service(t, doc, "dhcp6"), service(t, doc, "aftr")
	runtime, n, manager := runtimeMigrationFixture(t, doc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager.mu.Lock()
	pdOwners, owners := 0, len(manager.modules)
	for _, module := range manager.modules {
		if module.Role == "dhcp6" {
			pdOwners++
		}
	}
	manager.mu.Unlock()
	if owners != 3 || pdOwners != 1 {
		t.Fatal("compiled aliases did not retain one native UDP 546 owner")
	}
	initial := n.state()
	wan := migrationInterface(t, initial, "eth0")
	if wan.IPv6AcceptRA == nil || *wan.IPv6AcceptRA != 0 || wan.IPv6Autoconf == nil || *wan.IPv6Autoconf || len(initial.DSLite) != 0 {
		t.Fatal("native RA ownership was not acquired before tunnel activation")
	}
	clock, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	now := clock.WallTime
	learned := raclient.Snapshot{Resource: raService.Name, Interface: "eth0", SourceAddress: "fe80::2", InterfaceID: "::2", Active: true, UpdatedAt: now,
		Addresses:  []raclient.Address{{Address: "2001:db8:100::2/128", Prefix: "2001:db8:100::/64", PreferredUntil: now.Add(15 * time.Minute), ValidUntil: now.Add(20 * time.Minute)}},
		Prefixes:   []raclient.Prefix{{Prefix: "2001:db8:100::/64", ValidUntil: now.Add(20 * time.Minute)}},
		Routes:     []raclient.Route{{Destination: "::/0", Gateway: "fe80::1", Preference: 1, ValidUntil: now.Add(90 * time.Second)}},
		DNSServers: []raclient.DNSServer{{Address: "2001:db8::54", Router: "fe80::1", ValidUntil: now.Add(90 * time.Second)}},
	}
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, raService, 1, learned)); err != nil {
		t.Fatal(err)
	}
	pd := dhcp6config.Snapshot{BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), Resource: pdService.Name, Interface: "eth0", IAID: 1, ClientDUID: "00030001020000000001", State: "bound", Prefixes: []dhcp6config.Prefix{{Prefix: "2001:db8:200::/56", PreferredUntil: lifetime.After(clock, 480), ValidUntil: lifetime.After(clock, 600)}}, AFTRName: "dhcp-aftr.example", DNSServers: []string{"2001:db8::55"}, InfoReceivedNanos: uint64(clock.BoottimeNanoseconds), InformationRefreshSeconds: 120, InformationRefreshAt: lifetime.After(clock, 120)}
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, pdService, 1, pd)); err != nil {
		t.Fatal(err)
	}
	input, err := runtime.AFTRInput(ctx, dsService.ModuleName())
	if err != nil || input.Source.Address != "" {
		t.Fatal("tentative source escaped the owned-address/DAD gate", input, err)
	}
	n.mu.Lock()
	n.dadReady = true
	n.mu.Unlock()
	input, err = runtime.AFTRInput(ctx, dsService.ModuleName())
	if err != nil || input.Source.Address != "2001:db8:100::2" || len(input.Candidates) != 4 || input.Candidates[0].Value != "dhcp-aftr.example" || input.Candidates[0].ValidUntil != pd.InformationRefreshAt {
		t.Fatal("typed source or supplementary-information candidate was not projected", input, err)
	}
	if len(input.Resolvers) != 3 || input.Resolvers[0].Address != "2001:db8::53" || input.Resolvers[0].ValidUntil != (lifetime.Deadline{}) || input.Resolvers[1].Address != "2001:db8::55" || input.Resolvers[1].ValidUntil != pd.InformationRefreshAt || input.Resolvers[2].Address != "2001:db8::54" || !input.Resolvers[2].ValidUntil.UTC.Equal(learned.DNSServers[0].ValidUntil) {
		t.Fatal("configured/PD/RA resolver order or absolute lifetimes changed", input.Resolvers)
	}
	projected := n.state()
	lan := migrationInterface(t, projected, "eth1")
	if option, ok := lan.AddressOptions["2001:db8:200:10::1/64"]; !ok || option.BootLifetime.ValidNanos != pd.Prefixes[0].ValidUntil.Nanos {
		t.Fatal("hexadecimal delegation or PD expiry was lost at runtime", lan)
	}
	var raRoute network.Route
	for _, route := range projected.Routes {
		if route.Destination == "::/0" {
			raRoute = route
		}
	}
	if raRoute.Gateway != "fe80::1" || raRoute.Table != 254 || raRoute.Metric != 200 || !raRoute.ValidUntil.Equal(learned.Routes[0].ValidUntil) {
		t.Fatal("explicit native RA routing policy failed projection", raRoute)
	}
	receipt, err := boottime.Read()
	if err != nil {
		t.Fatal(err)
	}
	received := receipt.WallTime
	answerExpiry := lifetime.After(receipt, 60)
	resolved := aftr.Snapshot{BootID: receipt.BootID, ObservedNanos: uint64(receipt.BoottimeNanoseconds), Resource: dsService.Name, Interface: "eth0", ConfigDigest: input.ConfigDigest, Generation: input.Generation, Active: true, Available: true,
		Local: input.Source.Address, Remote: "2001:db8:ffff::10", CandidateIndex: 0, ResolverIndex: 1, UpdatedAt: received, ValidUntil: answerExpiry,
		Answers: []aftr.Answer{{Address: "2001:db8:ffff::10", TTL: 60, ReceivedNanos: uint64(receipt.BoottimeNanoseconds), ValidUntil: answerExpiry}},
	}
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, dsService, 1, resolved)); err != nil {
		t.Fatal(err)
	}
	projected = n.state()
	if len(projected.DSLite) != 1 || projected.DSLite[0].Name != "ds0" || projected.DSLite[0].Local != resolved.Local || projected.DSLite[0].Remote != resolved.Remote || projected.DSLite[0].MTU != 1460 {
		t.Fatal("resolved native tunnel differs from migrated intent", projected.DSLite)
	}
	b4 := migrationInterface(t, projected, "ds0")
	if options := b4.AddressOptions["192.0.0.2/32"]; options.Peer != "192.0.0.1/32" || options.BootLifetime.ValidNanos != answerExpiry.Nanos {
		t.Fatal("B4 peer or endpoint expiry lost at native projection", b4)
	}
	var b4Route network.Route
	for _, route := range projected.Routes {
		if route.Interface == "ds0" {
			b4Route = route
		}
	}
	if b4Route.Destination != "0.0.0.0/0" || b4Route.Gateway != "192.0.0.1" || b4Route.Metric != 50 {
		t.Fatal("legacy default route gateway/metric did not reach the native writer", b4Route)
	}
	// A newly stale information grant invalidates the original AFTR generation
	// immediately, even though its earlier DNS answer has not yet expired.
	pd.InformationRefreshAt = lifetime.After(clock, 0)
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, pdService, 2, pd)); err != nil {
		t.Fatal(err)
	}
	projected = n.state()
	if len(projected.DSLite) != 0 || len(projected.Interfaces) != 2 || len(migrationInterface(t, projected, "eth0").Addresses) != 1 || len(migrationInterface(t, projected, "eth1").Addresses) != 2 {
		t.Fatal("information expiry did not withdraw only the B4 dependency", projected)
	}
	for _, route := range projected.Routes {
		if route.Interface == "ds0" {
			t.Fatal("stale AFTR retained its dependent default route")
		}
	}
	input, err = runtime.AFTRInput(ctx, dsService.ModuleName())
	if err != nil || len(input.Candidates) != 3 || len(input.Resolvers) != 2 {
		t.Fatal("stale information survived the next dependency generation", input, err)
	}
	if err := observeMigration(ctx, runtime, migrationUpdate(t, runtime, dsService, 2, resolved)); !errors.Is(err, runtimeplan.ErrStale) {
		t.Fatal("old AFTR generation was accepted after information expiry", err)
	}
	if err := runtime.ApplyConfig(ctx, 2, config.Document{SchemaVersion: config.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.State().Instances) != 0 || len(n.state().Interfaces) != 0 || len(n.state().Routes) != 0 {
		t.Fatal("removing migrated configuration retained native ownership")
	}
}
