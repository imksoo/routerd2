package derived

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/bus"
	"github.com/imksoo/routerd/pkg/daemonapi"
	legacy "github.com/imksoo/routerd/pkg/derived"
)

type referenceStore map[string]map[string]any

func (s referenceStore) SaveObjectStatus(version, kind, name string, status map[string]any) error {
	s[version+"/"+kind+"/"+name] = status
	return nil
}
func (s referenceStore) ObjectStatus(version, kind, name string) map[string]any {
	return s[version+"/"+kind+"/"+name]
}

func TestKnownImmediateOutputsMatchPinnedReference(t *testing.T) {
	for _, emit := range []string{"all_true", "any_true"} {
		for _, retract := range []string{"any_false", "all_false"} {
			for _, initial := range []bool{false, true} {
				t.Run(emit+"/"+retract+"/"+strconv.FormatBool(initial), func(t *testing.T) {
					spec := api.DerivedEventSpec{Topic: "routerd.derived", Inputs: []api.ReadyWhenSpec{{Field: "${Interface/a.status.phase}", Equals: "Up"}, {Field: "${Interface/b.status.phase}", Equals: "Up"}}, EmitWhen: emit, RetractWhen: retract, EmitInitial: initial}
					p, err := Compile("derived", spec)
					if err != nil {
						t.Fatal(err)
					}
					b := bus.New()
					store := referenceStore{}
					ref := legacy.Controller{Router: &api.Router{Spec: api.RouterSpec{Resources: []api.Resource{{TypeMeta: api.TypeMeta{APIVersion: api.NetAPIVersion, Kind: "DerivedEvent"}, Metadata: api.ObjectMeta{Name: "derived"}, Spec: spec}}}}, Bus: b, Store: store}
					var state json.RawMessage
					var actual []daemonapi.DaemonEvent
					for i, pair := range [][2]string{{"Down", "Down"}, {"Up", "Down"}, {"Up", "Up"}, {"Down", "Up"}, {"Down", "Down"}} {
						now := uint64(i+1) * second
						values := map[string]Value{}
						for j, name := range []string{"a", "b"} {
							store[api.NetAPIVersion+"/Interface/"+name] = map[string]any{"phase": pair[j]}
							values["Interface/"+name+".status.phase"] = Value{Text: pair[j], Known: true, Scope: "boot/config", Origin: "worker-" + name, ObservedNanos: now, ExpiresNanos: 100 * second}
						}
						snapshot, err := NewSnapshot(strconv.Itoa(i), "boot/config", now, wall, values)
						if err != nil {
							t.Fatal(err)
						}
						r, err := p.Advance(state, snapshot, false)
						if err != nil {
							t.Fatal(err)
						}
						state = r.State
						for _, event := range r.Emissions {
							actual = append(actual, event.Event)
						}
						ref.Now = func() time.Time { return wall }
						if err = ref.Reconcile(context.Background(), false); err != nil {
							t.Fatal(err)
						}
						status := store[api.NetAPIVersion+"/DerivedEvent/derived"]
						if r.Status.Asserted == nil || status["asserted"] != *r.Status.Asserted || status["phase"] != r.Status.Phase {
							t.Fatalf("status mismatch: native %+v, reference %+v", r.Status, status)
						}
					}
					for _, suffix := range []string{".asserted", ".retracted"} {
						want := b.Recent(spec.Topic + suffix)
						var got []daemonapi.DaemonEvent
						for _, event := range actual {
							if event.Type == spec.Topic+suffix {
								got = append(got, event)
							}
						}
						if len(got) != len(want) {
							t.Fatal("reference emission count differs", got, want)
						}
						for i := range got {
							got[i].Time = want[i].Time
							got[i].Cursor = want[i].Cursor
							if !reflect.DeepEqual(got[i], want[i]) {
								t.Fatalf("reference emission differs: %+v, want %+v", got[i], want[i])
							}
						}
					}
				})
			}
		}
	}
}
