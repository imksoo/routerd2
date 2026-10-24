package derived

import "testing"

func TestHistoricalUpCannotGrantRetractionGraceUnderNewAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, scope, producer string
		known                 bool
	}{{"same-producer-grace", "boot/config", "worker-1", true}, {"new-producer", "boot/config", "worker-2", false}, {"new-control-scope", "new-control/config", "worker-1", false}} {
		t.Run(tc.name, func(t *testing.T) {
			p := compile(t, "1s", false)
			up, err := p.Advance(nil, snapshot(t, "boot/config", "worker-1", "Up", true, second, 10*second), false)
			if err != nil {
				t.Fatal(err)
			}
			down, err := p.Advance(up.State, snapshot(t, tc.scope, tc.producer, "Pending", true, 2*second, 10*second), false)
			if err != nil || down.Status.PendingTransition != "Retracting" {
				t.Fatal(down, err)
			}
			if tc.known {
				if down.Status.Asserted == nil || !*down.Status.Asserted {
					t.Fatal("same-authority hysteresis grace was lost", down)
				}
			} else if down.Status.Asserted != nil {
				t.Fatal("historical Up was advertised under replacement authority", down)
			}
			finished, err := p.Advance(down.State, snapshot(t, tc.scope, tc.producer, "Pending", true, 3*second, 10*second), false)
			if err != nil || finished.Status.Asserted == nil || *finished.Status.Asserted {
				t.Fatal("fresh stable Down never completed", finished, err)
			}
		})
	}
}
