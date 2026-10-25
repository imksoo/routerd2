package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func conditionalVRRPSource() config.Document {
	return config.Document{SchemaVersion: config.SchemaVersion, Resources: []resourceprogram.Resource{
		{ID: "ha", Service: &serviceconfig.Service{Name: "ha", VRRP: &vrrp.Config{Interface: "lan0", Family: "ipv4", VirtualRouterID: 42, AcceptMode: true, Addresses: []string{"192.0.2.254/24"}}}},
		{ID: "unused", When: &resourceprogram.FactCondition{Fact: "service.ha.active"}, Route: &network.Route{Destination: "198.51.100.0/24", Interface: "lan0", Gateway: "192.0.2.1"}},
	}}
}

func TestConditionalVRRPWaitsForNetworkACK(t *testing.T) {
	source := conditionalVRRPSource()
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	r.manager = &memoryManager{}
	if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
		t.Fatal(err)
	}
	c := source.Resources[0].Service.VRRPConfig()
	s := bootVRRPSnapshot(t, c)
	u := update(t, r, "service.ha", 1, s)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.Accept(ctx, u) }()
	// Wait for the accepted observation, without relying on scheduler timing.
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		_, accepted := r.records[u.Name]
		r.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("VRRP publication was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-result:
		t.Fatalf("VRRP callback returned before kernel effects were applied: %v", err)
	default:
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("network ACK did not release VRRP publisher")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	found := false
	for _, iface := range n.applied.Interfaces {
		for address, opts := range iface.AddressOptions {
			if address == c.Addresses[0] && iface.Name == c.VIPInterface {
				found = opts.BootLifetime.BootID == s.BootID && opts.BootLifetime.ValidNanos == s.ExpiresNanos
			}
		}
	}
	if !found {
		t.Fatal("success ACK lacked VIP with original boot expiry")
	}
}

// This manager models the actual worker contract: Ready follows initial effect
// ACK, and process exit follows the final inactive effect ACK.
type vrrpACKManager struct {
	mu                sync.Mutex
	runtime           *Runtime
	modules           map[string]supervisor.ModuleStatus
	stopping          map[string]bool
	haStarted         bool
	initial           chan error
	withdrawal        chan error
	dependentStopping chan struct{}
	releaseDependent  chan struct{}
	snapshot          vrrp.Snapshot
}

func (m *vrrpACKManager) Status(context.Context) (supervisor.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := supervisor.Status{Ready: true, InstanceID: testSupervisorInstance}
	for _, module := range m.modules {
		s.Modules = append(s.Modules, module)
		if !module.Ready {
			s.Ready = false
		}
	}
	return s, nil
}
func (m *vrrpACKManager) Reconcile(ctx context.Context, modules []supervisor.DynamicModule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modules == nil {
		m.modules = make(map[string]supervisor.ModuleStatus)
		m.stopping = make(map[string]bool)
	}
	wanted := make(map[string]bool)
	for _, module := range modules {
		wanted[module.Name] = true
		if _, exists := m.modules[module.Name]; exists {
			continue
		}
		status := supervisor.ModuleStatus{Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest, Generation: 1, State: "running", Ready: true}
		if module.Name == "service.ha" {
			status.Ready = false
			m.haStarted = true
			go func() {
				raw, _ := json.Marshal(m.snapshot)
				err := m.runtime.Accept(ctx, serviceconfig.Update{SupervisorInstance: testSupervisorInstance, Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest, Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: 1, Snapshot: raw})
				m.mu.Lock()
				current := m.modules[module.Name]
				current.Ready = err == nil
				m.modules[module.Name] = current
				m.mu.Unlock()
				m.initial <- err
			}()
		}
		m.modules[module.Name] = status
	}
	for name, module := range m.modules {
		if wanted[name] || m.stopping[name] {
			continue
		}
		m.stopping[name] = true
		switch name {
		case "service.ha":
			go func() {
				s := m.snapshot
				s.Active = false
				s.State = vrrp.StateInitialize
				s.Transition++
				s.ExpiresNanos = 0
				s.ValidUntil = time.Time{}
				raw, _ := json.Marshal(s)
				err := m.runtime.Accept(ctx, serviceconfig.Update{SupervisorInstance: testSupervisorInstance, Name: module.Name, Role: module.Role, ConfigDigest: module.ConfigDigest, Generation: 1, ProducerEpoch: strings.Repeat("a", 32), Sequence: 2, Snapshot: raw})
				m.mu.Lock()
				delete(m.modules, module.Name)
				m.mu.Unlock()
				m.withdrawal <- err
			}()
		case "service.dependent":
			close(m.dependentStopping)
			go func() { <-m.releaseDependent; m.mu.Lock(); delete(m.modules, module.Name); m.mu.Unlock() }()
		default:
			delete(m.modules, name)
		}
	}
	return nil
}

func TestConditionalVRRPStartupAndStopACKCannotDeadlock(t *testing.T) {
	source := conditionalVRRPSource()
	source.Resources = append(source.Resources, resourceprogram.Resource{ID: "dependent", When: &resourceprogram.FactCondition{Fact: "service.ha.active"}, Service: &serviceconfig.Service{Name: "dependent", DHCP4: &serviceconfig.DHCP4{Interface: "other0"}}})
	n := &memoryNetwork{}
	r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
	m := &vrrpACKManager{runtime: r, initial: make(chan error, 1), withdrawal: make(chan error, 1), dependentStopping: make(chan struct{}), releaseDependent: make(chan struct{}), snapshot: bootVRRPSnapshot(t, source.Resources[0].Service.VRRPConfig())}
	r.manager = m
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.ApplyConfig(ctx, 1, source); err != nil {
		t.Fatal("startup waited for Ready without servicing ownership effects:", err)
	}
	if err := <-m.initial; err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileDynamic(ctx); err != nil {
		t.Fatal(err)
	}
	status, _ := m.Status(ctx)
	dependent := false
	for _, module := range status.Modules {
		dependent = dependent || module.Name == "service.dependent"
	}
	if !dependent {
		t.Fatal("active VRRP did not start dependent")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- r.ApplyConfig(ctx, 2, config.FromNative(config.Native{})) }()
	select {
	case <-m.dependentStopping:
	case <-ctx.Done():
		t.Fatal("dependent was not asked to stop")
	}
	// A stop request alone is not process exit. Keep the prerequisite until the
	// dependent is actually gone, even if HA has queued its final withdrawal.
	time.Sleep(75 * time.Millisecond)
	n.mu.Lock()
	hasVIP := len(n.applied.Links) != 0
	n.mu.Unlock()
	if !hasVIP {
		t.Fatal("VIP withdrawn while dependent worker still existed")
	}
	select {
	case err := <-m.withdrawal:
		t.Fatalf("withdrawal ACK preceded dependent exit: %v", err)
	default:
	}
	close(m.releaseDependent)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal("final withdrawal blocked configuration cutover:", err)
		}
	case <-ctx.Done():
		t.Fatal("stop did not drain VRRP withdrawal")
	}
	if err := <-m.withdrawal; err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.applied.Links) != 0 {
		t.Fatal("VIP remained after withdrawal ACK")
	}
}

func TestConditionalVRRPEffectFailureFences(t *testing.T) {
	for _, mode := range []string{"network-error", "retired-generation", "expired-active", "cancelled-caller", "superseded"} {
		t.Run(mode, func(t *testing.T) {
			source := conditionalVRRPSource()
			n := &memoryNetwork{}
			m := &memoryManager{}
			r := New(t.TempDir(), t.TempDir(), n, testSupervisorInstance)
			r.manager = m
			if err := r.ApplyConfig(context.Background(), 1, source); err != nil {
				t.Fatal(err)
			}
			s := bootVRRPSnapshot(t, source.Resources[0].Service.VRRPConfig())
			if mode == "expired-active" {
				s.ExpiresNanos = s.ObservedNanos + uint64(100*time.Millisecond)
			}
			u := update(t, r, "service.ha", 1, s)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- r.Accept(ctx, u) }()
			deadline := time.Now().Add(time.Second)
			for {
				r.mu.Lock()
				_, accepted := r.records[u.Name]
				r.mu.Unlock()
				if accepted {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("publication not accepted")
				}
				time.Sleep(time.Millisecond)
			}
			switch mode {
			case "network-error":
				n.mu.Lock()
				n.fail = errors.New("injected network apply failure")
				n.mu.Unlock()
			case "retired-generation":
				m.vrrpGeneration = 2
			case "expired-active":
				time.Sleep(110 * time.Millisecond)
			case "cancelled-caller":
				cancel()
			case "superseded":
				stopped := s
				stopped.Active = false
				stopped.State = vrrp.StateInitialize
				stopped.ExpiresNanos = 0
				stopped.ValidUntil = time.Time{}
				stopped.Transition++
				newer := update(t, r, u.Name, 2, stopped)
				next := make(chan error, 1)
				go func() { next <- r.Accept(ctx, newer) }()
				for {
					r.mu.Lock()
					sequence := r.records[u.Name].Sequence
					r.mu.Unlock()
					if sequence == 2 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("new withdrawal not accepted")
					}
					time.Sleep(time.Millisecond)
				}
				if err := r.reconcileDynamic(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-next:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("new withdrawal not acknowledged")
				}
			}
			if mode != "superseded" {
				err := r.reconcileDynamic(context.Background())
				if mode == "network-error" && err == nil {
					t.Fatal("network failure hidden")
				}
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("invalid effect was acknowledged")
				}
			case <-time.After(time.Second):
				t.Fatal("failed effect did not complete")
			}
			if mode == "retired-generation" || mode == "expired-active" || mode == "superseded" {
				n.mu.Lock()
				defer n.mu.Unlock()
				if len(n.applied.Links) != 0 {
					t.Fatal("invalid pending producer acquired kernel VIP")
				}
			}
		})
	}
}
