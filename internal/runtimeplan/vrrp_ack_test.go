package runtimeplan

import (
	"context"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
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
