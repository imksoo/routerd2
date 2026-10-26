// SPDX-License-Identifier: BSD-3-Clause
package aftr

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

func configForTest() Config {
	return Config{Resource: "ds-lite", Interface: "wan0", ConfigDigest: strings.Repeat("a", 64)}
}
func clockForTest(seconds int) boottime.Clock {
	return boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Duration(1000+seconds) * time.Second), WallTime: time.Unix(1700000000+int64(seconds), 0).UTC()}
}
func inputForTest() Input {
	c := clockForTest(0)
	return Input{Resource: "ds-lite", Interface: "wan0", ConfigDigest: configForTest().ConfigDigest, Generation: 1, UpdatedAt: c.WallTime, Active: true,
		Source:     Source{Address: "2001:db8:1::1", PreferredUntil: lifetime.After(c, 100), ValidUntil: lifetime.After(c, 200)},
		Candidates: []Candidate{{Value: "aftr.example", Origin: "dhcp6", ValidUntil: lifetime.After(c, 150)}},
		Resolvers:  []Resolver{{Address: "fe80::53", Interface: "wan0", ValidUntil: lifetime.After(c, 120)}}}
}
func testMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{config: configForTest()}
	if changed, err := m.apply(inputForTest(), clockForTest(0)); err != nil || !changed {
		t.Fatal(changed, err)
	}
	return m
}
func goodResult(c boottime.Clock, ttl uint32) resolution {
	return resolution{candidate: 0, resolver: 0, answers: []Answer{{Address: "2001:db8:ffff::1", TTL: ttl, ReceivedNanos: uint64(c.BoottimeNanoseconds), ValidUntil: lifetime.After(c, ttl)}}}
}
func validView(t *testing.T, m *machine, c boottime.Clock) Snapshot {
	t.Helper()
	s := m.snapshot(c)
	if err := ValidateSnapshot(m.config, m.input, s); err != nil {
		t.Fatal(s, err)
	}
	return s
}
func TestInputRenewalFailureAndUTCStepsCannotRefreshDNS(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	first := validView(t, m, clockForTest(0))
	input := cloneInput(m.input)
	input.Generation++
	input.Source.PreferredUntil = lifetime.After(clockForTest(0), 500)
	input.Source.ValidUntil = lifetime.After(clockForTest(0), 600)
	if _, err := m.apply(input, clockForTest(5)); err != nil {
		t.Fatal(err)
	}
	m.result(resolution{}, errors.New("upstream unavailable"), clockForTest(6))
	for _, offset := range []time.Duration{24 * time.Hour, -24 * time.Hour, 0} {
		clock := clockForTest(10)
		clock.WallTime = clock.WallTime.Add(offset)
		s := validView(t, m, clock)
		if !s.Available || s.ValidUntil != first.ValidUntil || !reflect.DeepEqual(s.Answers, first.Answers) {
			t.Fatal("poll/input renewal/UTC step changed original DNS budget", s)
		}
	}
	if validView(t, m, clockForTest(20)).Available {
		t.Fatal("expired DNS survived")
	}
}
func TestEndpointUsesOriginalShortestDependency(t *testing.T) {
	for _, which := range []string{"source", "candidate", "resolver"} {
		t.Run(which, func(t *testing.T) {
			m := testMachine(t)
			in := cloneInput(m.input)
			in.Generation++
			until := lifetime.After(clockForTest(0), 4)
			switch which {
			case "source":
				in.Source.PreferredUntil = until
			case "candidate":
				in.Candidates[0].ValidUntil = until
			case "resolver":
				in.Resolvers[0].ValidUntil = until
			}
			if _, err := m.apply(in, clockForTest(0)); err != nil {
				t.Fatal(err)
			}
			m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
			if s := validView(t, m, clockForTest(3)); !s.Available || s.ValidUntil != until {
				t.Fatal(s)
			}
			if validView(t, m, clockForTest(4)).Available {
				t.Fatal("dependency expired without withdrawal")
			}
		})
	}
}
func TestInputGenerationAndSnapshotAuthority(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	first := validView(t, m, clockForTest(0))
	if changed, err := m.apply(m.input, clockForTest(1)); err != nil || changed {
		t.Fatal("replay changed input", changed, err)
	}
	input := cloneInput(m.input)
	input.Source.Address = "2001:db8:1::2"
	if _, err := m.apply(input, clockForTest(1)); err == nil {
		t.Fatal("equal-generation mutation accepted")
	}
	input.Generation++
	if _, err := m.apply(input, clockForTest(1)); err != nil {
		t.Fatal(err)
	}
	if m.snapshot(clockForTest(1)).Available || ValidateSnapshot(m.config, m.input, first) == nil {
		t.Fatal("old query result crossed source generation")
	}
	if _, err := m.apply(inputForTest(), clockForTest(2)); err == nil {
		t.Fatal("old generation replayed")
	}
}
func TestSnapshotCannotExceedDNSOrSourceAuthority(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	base := validView(t, m, clockForTest(0))
	for name, change := range map[string]func(*Snapshot){
		"local":          func(s *Snapshot) { s.Local = "2001:db8:1::2" },
		"generation":     func(s *Snapshot) { s.Generation++ },
		"ttl-zero":       func(s *Snapshot) { s.Answers[0].TTL = 0 },
		"unbounded-DNS":  func(s *Snapshot) { s.ValidUntil = lifetime.Deadline{} },
		"late-expiry":    func(s *Snapshot) { s.ValidUntil = lifetime.After(clockForTest(0), 21) },
		"future-answer":  func(s *Snapshot) { s.Answers[0].ReceivedNanos = s.ObservedNanos + 1 },
		"foreign-boot":   func(s *Snapshot) { s.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" },
		"wrong-resolver": func(s *Snapshot) { s.ResolverIndex = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			s.Answers = append([]Answer(nil), base.Answers...)
			change(&s)
			if ValidateSnapshot(m.config, m.input, s) == nil {
				t.Fatal("forged authority accepted")
			}
		})
	}
}
func TestLiteralSelectionAndExplicitUTCCutoff(t *testing.T) {
	m := testMachine(t)
	in := inputForTest()
	in.Generation++
	cutoff := lifetime.Deadline{UTC: clockForTest(30).WallTime}
	in.Source.PreferredUntil = cutoff
	in.Source.ValidUntil = cutoff
	in.Candidates = []Candidate{{Value: "2001:db8:ffff::10", Origin: "configured"}}
	if _, err := m.apply(in, clockForTest(0)); err != nil {
		t.Fatal(err)
	}
	m.result(resolution{candidate: 0, resolver: -1}, nil, clockForTest(0))
	if s := validView(t, m, clockForTest(1)); !s.Available || s.ValidUntil != cutoff || s.ResolverIndex != -1 {
		t.Fatal(s)
	}
	shifted := clockForTest(1)
	shifted.WallTime = shifted.WallTime.Add(24 * time.Hour)
	if m.snapshot(shifted).Available {
		t.Fatal("operator UTC cutoff ignored clock change")
	}
	answers := []Answer{{Address: "2001:db8::2"}, {Address: "2001:db8::1"}}
	for _, c := range []Config{{Selection: "ordinal", Ordinal: 1}, {Selection: "ordinalModulo", Ordinal: 3}} {
		a, err := chooseAnswer(c, answers)
		if err != nil || a.Address != "2001:db8::1" {
			t.Fatal(a, err)
		}
	}
}
