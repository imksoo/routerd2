package runtimeplan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestPreviousSupervisorCannotSeedEmptyProtocolRuntime(t *testing.T) {
	for _, role := range []string{"dhcp4", "bfd"} {
		t.Run(role, func(t *testing.T) {
			doc := wanDoc()
			name := "service.wan"
			var snapshot any = lease4(time.Now().Add(time.Hour))
			if role == "bfd" {
				doc, name = bfdDocument(), "service.edge"
				clock, err := bfd.ReadClock()
				if err != nil {
					t.Fatal(err)
				}
				snapshot = observedBFD(t, doc.Services[0].BFDConfig(), clock)
			}
			r, _, _ := fixture(t, doc)
			u := update(t, r, name, 1, snapshot)
			b, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			_ = json.Unmarshal(b, &fields)
			fields["supervisor_instance"] = strings.Repeat("f", 32)
			// Higher old counters must not become the new authority after a
			// complete supervisor restart, even before its first fresh update.
			fields["generation"], fields["sequence"] = 9999, 9999
			b, _ = json.Marshal(fields)
			var stale serviceconfig.Update
			_ = json.Unmarshal(b, &stale)
			if err := r.Observe(context.Background(), stale); !errors.Is(err, ErrStale) {
				t.Fatal("previous supervisor seeded new protocol authority", err)
			}
			if err := r.Observe(context.Background(), u); err != nil {
				t.Fatal("old supervisor poisoned the new worker's first update", err)
			}
		})
	}
}
