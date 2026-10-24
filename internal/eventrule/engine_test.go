package eventrule

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
)

var epoch = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)

func input(id, topic, iface string, offset time.Duration) Input {
	return Input{ID: id, Event: daemonapi.DaemonEvent{Time: epoch.Add(offset), Type: topic, Attributes: map[string]string{"interface": iface}, Resource: &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "Interface", Name: iface}}}
}

func program(t *testing.T, pattern api.EventRulePatternSpec) Program {
	t.Helper()
	p, err := Compile("rule", api.EventRuleSpec{Pattern: pattern, Emit: api.EventRuleEmitSpec{Topic: "routerd.out", Attributes: map[string]string{"input": "${event.type}", "interface": "${attributes.interface}"}}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinnedImmediateOperatorContracts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern api.EventRulePatternSpec
		events  []Input
		fires   int
	}{
		{"any_of", api.EventRulePatternSpec{Operator: "any_of", Topics: []string{"routerd.*"}}, []Input{input("1", "routerd.a", "wan", 0)}, 1},
		{"all_of", api.EventRulePatternSpec{Operator: "all_of", Topics: []string{"routerd.a", "routerd.b"}}, []Input{input("1", "routerd.a", "wan", 0), input("2", "unrelated", "wan", 0), input("3", "routerd.b", "wan", 0)}, 1},
		{"sequence", api.EventRulePatternSpec{Operator: "sequence", Topics: []string{"routerd.a", "routerd.b"}}, []Input{input("1", "routerd.a", "wan", 0), input("2", "unrelated", "wan", 0), input("3", "routerd.b", "wan", 0)}, 1},
		{"strict_sequence", api.EventRulePatternSpec{Operator: "sequence", Topics: []string{"routerd.a", "routerd.b"}, Strict: true}, []Input{input("1", "routerd.a", "wan", 0), input("2", "routerd.a", "wan", 0), input("3", "routerd.b", "wan", 0)}, 0},
		{"count", api.EventRulePatternSpec{Operator: "count", Topic: "routerd.a", Threshold: 2}, []Input{input("1", "routerd.a", "wan", 0), input("2", "routerd.a", "wan", 0), input("3", "routerd.a", "wan", 0)}, 1},
		{"window_inclusive_boundary", api.EventRulePatternSpec{Operator: "window", Topic: "routerd.a", Window: "60s", Threshold: 3}, []Input{input("1", "routerd.a", "wan", 0), input("2", "routerd.a", "wan", 30*time.Second), input("3", "routerd.a", "wan", 60*time.Second)}, 1},
		{"throttle_inclusive_boundary", api.EventRulePatternSpec{Operator: "throttle", Topic: "routerd.a", Interval: "1s"}, []Input{input("1", "routerd.a", "wan", 0), input("2", "routerd.a", "wan", 100*time.Millisecond), input("3", "routerd.a", "wan", time.Second)}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := program(t, tc.pattern)
			var state json.RawMessage
			var emitted []Emission
			for _, event := range tc.events {
				before := bytes.Clone(state)
				result, err := p.Advance(state, event, epoch.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(state, before) {
					t.Fatal("pure evaluator mutated caller state")
				}
				retry, err := p.Advance(state, event, epoch.Add(2*time.Hour))
				if err != nil || !reflect.DeepEqual(result, retry) {
					t.Fatalf("retry changed stable output/state: %v", err)
				}
				state = result.State
				emitted = append(emitted, result.Emissions...)
			}
			if len(emitted) != tc.fires {
				t.Fatalf("got %d fires, want %d", len(emitted), tc.fires)
			}
			for _, event := range emitted {
				if event.ID == "" || event.Event.Type != "routerd.out" || event.Event.Resource.Name != "rule" || event.Event.Reason != "EventRuleFired" {
					t.Fatalf("lost stable identity or origin: %+v", event)
				}
			}
		})
	}
}

func TestDurableAbsenceDebounceAndSpecReplacement(t *testing.T) {
	for _, operator := range []string{"absence", "debounce"} {
		t.Run(operator, func(t *testing.T) {
			pattern := api.EventRulePatternSpec{Operator: operator, Topic: "routerd.a", Duration: "1s", Expected: "routerd.b"}
			p := program(t, pattern)
			result, err := p.Advance(nil, input("first", "routerd.a", "wan", 0), epoch)
			if err != nil {
				t.Fatal(err)
			}
			if operator == "debounce" {
				result, err = p.Advance(result.State, input("last", "routerd.a", "wan", 500*time.Millisecond), epoch.Add(500*time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
			}
			early, err := p.Tick(result.State, epoch.Add(999*time.Millisecond))
			if err != nil || len(early.Emissions) != 0 {
				t.Fatal("timer fired early", err)
			}
			due := time.Second
			if operator == "debounce" {
				due = 1500 * time.Millisecond
			}
			// State can be reopened after a crash; the due time and output identity
			// do not depend on a surviving goroutine or the time of the retry.
			fired, err := p.Tick(bytes.Clone(result.State), epoch.Add(due))
			if err != nil || len(fired.Emissions) != 1 {
				t.Fatal("persisted timer did not fire", fired, err)
			}
			retry, err := p.Tick(result.State, epoch.Add(due+time.Hour))
			if err != nil || !reflect.DeepEqual(fired, retry) {
				t.Fatal("timer retry changed identity/content", err)
			}
			again, err := p.Tick(fired.State, epoch.Add(2*time.Hour))
			if err != nil || len(again.Emissions) != 0 {
				t.Fatal("timer fired twice", err)
			}
			if operator == "absence" {
				cancelled, err := p.Advance(result.State, input("expected", "routerd.b", "wan", 500*time.Millisecond), epoch.Add(500*time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				cancelled, err = p.Tick(cancelled.State, epoch.Add(2*time.Second))
				if err != nil || len(cancelled.Emissions) != 0 {
					t.Fatal("expected event did not cancel absence", err)
				}
			}
			pattern.Duration = "2s"
			replacement := program(t, pattern)
			reset, err := replacement.Tick(result.State, epoch.Add(time.Hour))
			if err != nil || len(reset.Emissions) != 0 {
				t.Fatal("old spec timer survived replacement", err)
			}
		})
	}
}

func TestCorrelationAndLiteralTemplateExpansion(t *testing.T) {
	p := program(t, api.EventRulePatternSpec{Operator: "all_of", Topics: []string{"routerd.a", "routerd.b"}, CorrelateBy: "attributes.interface"})
	r, err := p.Advance(nil, input("1", "routerd.a", "wan-a", 0), epoch)
	if err != nil {
		t.Fatal(err)
	}
	r, err = p.Advance(r.State, input("2", "routerd.b", "wan-b", 0), epoch)
	if err != nil || len(r.Emissions) != 0 {
		t.Fatal("cross-key correlation", err)
	}
	r, err = p.Advance(r.State, input("3", "routerd.a", "wan-b", 0), epoch)
	if err != nil || len(r.Emissions) != 1 || r.Emissions[0].Event.Attributes["correlation"] != "wan-b" {
		t.Fatal(r, err)
	}
	p = program(t, api.EventRulePatternSpec{Operator: "any_of", Topic: "routerd.a"})
	r, err = p.Advance(nil, input("literal", "routerd.a", "${resource.name}", 0), epoch)
	if err != nil || len(r.Emissions) != 1 || r.Emissions[0].Event.Attributes["interface"] != "${resource.name}" {
		t.Fatal("input was interpreted as another template", r, err)
	}
}

func TestInvalidRulesAndExpiredInputsNeverEmit(t *testing.T) {
	for _, pattern := range []api.EventRulePatternSpec{{Operator: "unknown", Topic: "routerd.a"}, {Operator: "count"}, {Operator: "absence", Topic: "routerd.a"}, {Operator: "window", Topic: "routerd.a", Window: "-1s"}, {Operator: "debounce", Topic: "routerd.a", Quiet: "nonsense"}, {Operator: "count", Topic: "routerd.a", CorrelateBy: "secret.path"}} {
		if _, err := Compile("bad", api.EventRuleSpec{Pattern: pattern, Emit: api.EventRuleEmitSpec{Topic: "routerd.out"}}); err == nil {
			t.Fatal("accepted invalid rule", pattern)
		}
	}
	p := program(t, api.EventRulePatternSpec{Operator: "any_of", Topic: "routerd.a"})
	event := input("expired", "routerd.a", "wan", 0)
	event.ExpiresAt = epoch.Add(time.Second)
	if r, err := p.Advance(nil, event, event.ExpiresAt); err != nil || len(r.Emissions) != 1 {
		t.Fatal("expiry boundary changed", r, err)
	}
	if r, err := p.Advance(nil, event, event.ExpiresAt.Add(time.Nanosecond)); err != nil || len(r.Emissions) != 0 {
		t.Fatal("expired input emitted", r, err)
	}
}
