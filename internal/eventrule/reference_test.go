package eventrule

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bus"
	"github.com/imksoo/routerd/pkg/daemonapi"
	legacy "github.com/imksoo/routerd/pkg/eventrule"
)

type referenceStatus struct{}

func (referenceStatus) SaveObjectStatus(string, string, string, map[string]any) error { return nil }
func (referenceStatus) ObjectStatus(string, string, string) map[string]any            { return nil }

func TestImmediateOutputsMatchPinnedRouterdReference(t *testing.T) {
	for _, pattern := range []api.EventRulePatternSpec{
		{Operator: "all_of", Topics: []string{"routerd.a", "routerd.b"}},
		{Operator: "any_of", Topic: "routerd.*"},
		{Operator: "sequence", Topics: []string{"routerd.a", "routerd.b"}, Strict: true},
		{Operator: "count", Topic: "routerd.*", Threshold: 2},
		{Operator: "window", Topic: "routerd.*", Threshold: 2, Window: "1m"},
		{Operator: "throttle", Topic: "routerd.*", Interval: "1m"},
		{Operator: "all_of", Topics: []string{"routerd.a", "routerd.b"}, CorrelateBy: "resource.name"},
		{Operator: "all_of", Topics: []string{"routerd.a", "routerd.b"}, CorrelateBy: " resource.name "},
	} {
		t.Run(pattern.Operator+"/"+pattern.CorrelateBy, func(t *testing.T) {
			spec := api.EventRuleSpec{Pattern: pattern, Emit: api.EventRuleEmitSpec{Topic: "routerd.out", Attributes: map[string]string{"type": "${event.type}", "resource": "${resource.name}"}}}
			p, err := Compile("rule", spec)
			if err != nil {
				t.Fatal(err)
			}
			b := bus.New()
			ref := legacy.Controller{Router: &api.Router{Spec: api.RouterSpec{Resources: []api.Resource{{TypeMeta: api.TypeMeta{APIVersion: api.NetAPIVersion, Kind: "EventRule"}, Metadata: api.ObjectMeta{Name: "rule"}, Spec: spec}}}}, Bus: b, Store: referenceStatus{}}
			var state json.RawMessage
			var actual []daemonapi.DaemonEvent
			for _, event := range []Input{input("1", "routerd.a", "", 0), input("2", "routerd.b", "", 0), input("3", "routerd.a", "wan", 0), input("4", "routerd.b", "wan", 0)} {
				result, err := p.Advance(state, event, epoch)
				if err != nil {
					t.Fatal(err)
				}
				state = result.State
				for _, out := range result.Emissions {
					actual = append(actual, out.Event)
				}
				if err = ref.Reconcile(context.Background(), event.Event); err != nil {
					t.Fatal(err)
				}
			}
			want := b.Recent("routerd.out")
			if len(want) != len(actual) {
				t.Fatalf("native fires %d, pinned reference %d", len(actual), len(want))
			}
			for i := range want {
				// Native event time and identity are intentionally tied to the
				// durable input; the reference stamps its processing wall clock.
				actual[i].Time = want[i].Time
				actual[i].Cursor = want[i].Cursor
				if !reflect.DeepEqual(actual[i], want[i]) {
					t.Fatalf("native output differs: %+v, want %+v", actual[i], want[i])
				}
			}
		})
	}
}
