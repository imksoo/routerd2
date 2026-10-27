package runtimeplan

import (
	"context"
	"testing"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

// The real supervisor can reap DHCPv6 while the coordinator prepares files,
// between its two status reads. The WAN must still be retired in a later pass.
type dhcpReapManager struct {
	*memoryManager
	reads int
}

func (m *dhcpReapManager) Status(ctx context.Context) (supervisor.Status, error) {
	m.reads++
	if m.reads == 2 {
		for i, module := range m.modules {
			if module.Role == "dhcp6" {
				m.modules = append(m.modules[:i], m.modules[i+1:]...)
				break
			}
		}
	}
	return m.memoryManager.Status(ctx)
}

func TestDHCPExitDuringPreparationDoesNotCompleteWANDrain(t *testing.T) {
	ctx := context.Background()
	r, _, manager := fixture(t, config.Native{Services: []serviceconfig.Service{
		{Name: "wan", DHCP4: &serviceconfig.DHCP4{Interface: "wan0"}},
		{Name: "pd", DHCP6: &serviceconfig.DHCP6{Interface: "wan0", IAID: 1, RequestPrefix: true}},
	}})
	r.manager = &dhcpReapManager{memoryManager: manager}
	ceiling := map[string]supervisor.DynamicModule{}
	if err := r.wait(ctx, ceiling); err != nil {
		t.Fatal(err)
	}
	if len(manager.modules) != 0 {
		t.Fatal("WAN survived a completed dependency drain", manager.modules)
	}
}
