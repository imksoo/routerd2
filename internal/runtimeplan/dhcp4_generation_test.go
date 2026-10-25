package runtimeplan

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDHCPv4LiveSupervisorFencesFirstOldGeneration(t *testing.T) {
	r, _, _ := fixture(t, wanDoc())
	r.manager = &observedManager{generation: 2}
	u := update(t, r, "service.wan", 1, lease4(time.Now().Add(time.Hour)))
	if err := r.Observe(context.Background(), u); !errors.Is(err, ErrStale) {
		t.Fatal("old DHCP worker seeded empty runtime despite live supervisor generation2", err)
	}
}
