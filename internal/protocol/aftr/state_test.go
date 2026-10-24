// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func configForTest() Config {
	return Config{Resource: "ds-lite", Interface: "wan0", ConfigDigest: strings.Repeat("a", 64)}
}
func clockForTest(seconds int) instant {
	return instant{wall: time.Unix(1700000000+int64(seconds), 0).UTC(), boot: time.Unix(1000+int64(seconds), 0).UTC()}
}
func inputForTest() Input {
	now := clockForTest(0).wall
	return Input{Resource: "ds-lite", Interface: "wan0", ConfigDigest: configForTest().ConfigDigest, Generation: 1, UpdatedAt: now, Active: true,
		Source:     Source{Address: "2001:db8:1::1", PreferredUntil: now.Add(100 * time.Second), ValidUntil: now.Add(200 * time.Second)},
		Candidates: []Candidate{{Value: "aftr.example", Origin: "dhcp6", ValidUntil: now.Add(150 * time.Second)}},
		Resolvers:  []Resolver{{Address: "fe80::53", Interface: "wan0", ValidUntil: now.Add(120 * time.Second)}}}
}
func testMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{config: configForTest()}
	if changed, err := m.apply(inputForTest(), clockForTest(0)); err != nil || !changed {
		t.Fatal(changed, err)
	}
	return m
}
func goodResult(now instant, ttl uint32) resolution {
	return resolution{candidate: 0, resolver: 0, answers: []Answer{{Address: "2001:db8:ffff::1", TTL: ttl, ReceivedAt: now.boot, ValidUntil: now.boot.Add(time.Duration(ttl) * time.Second)}}}
}
func validView(t *testing.T, m *machine, now instant) Snapshot {
	t.Helper()
	s := m.snapshot(now)
	if err := ValidateSnapshot(m.config, m.input, s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInputScopeAndBootstrapRequirements(t *testing.T) {
	c, base := configForTest(), inputForTest()
	if err := ValidateInput(c, base); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Input){
		"digest":            func(v *Input) { v.ConfigDigest = strings.Repeat("b", 64) },
		"generation":        func(v *Input) { v.Generation = 0 },
		"source-link-local": func(v *Input) { v.Source.Address = "fe80::1" },
		"source-lifetimes":  func(v *Input) { v.Source.PreferredUntil = v.Source.ValidUntil.Add(time.Second) },
		"mapped-candidate":  func(v *Input) { v.Candidates[0].Value = "::ffff:192.0.2.1" },
		"unknown-origin":    func(v *Input) { v.Candidates[0].Origin = "kernel-auto" },
		"candidate-limit": func(v *Input) {
			for i := 0; i < MaxCandidates; i++ {
				v.Candidates = append(v.Candidates, Candidate{Value: "another.example", Origin: "configured"})
			}
		},
		"ipv4-dns-cycle":           func(v *Input) { v.Resolvers[0].Address = "192.0.2.53" },
		"implicit-host-resolver":   func(v *Input) { v.Resolvers[0].Address = "resolver.example" },
		"local-recursion":          func(v *Input) { v.Resolvers[0].Address = "::1" },
		"wrong-resolver-interface": func(v *Input) { v.Resolvers[0].Interface = "other0" },
		"zoned-resolver":           func(v *Input) { v.Resolvers[0].Address = "fe80::53%wan0" },
		"duplicate-default-port":   func(v *Input) { other := v.Resolvers[0]; other.Port = 53; v.Resolvers = append(v.Resolvers, other) },
	} {
		t.Run(name, func(t *testing.T) {
			v := cloneInput(base)
			change(&v)
			if ValidateInput(c, v) == nil {
				t.Fatal("out-of-scope dependency accepted")
			}
		})
	}
	c.ConfigDigest = ""
	if Validate(c) != nil || ValidateInput(c, base) == nil {
		t.Fatal("loader-only digest was not enforced at the input boundary")
	}
}

func TestRenewalAndFailedRefreshNeverExtendDNSLifetime(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	initial := validView(t, m, clockForTest(0))
	if !initial.Available || !initial.ValidUntil.Equal(clockForTest(20).wall) {
		t.Fatal("initial DNS deadline lost")
	}
	in := cloneInput(m.input)
	in.Generation++
	in.UpdatedAt = clockForTest(5).wall
	in.Source.PreferredUntil = clockForTest(500).wall
	in.Source.ValidUntil = clockForTest(600).wall
	if _, err := m.apply(in, clockForTest(5)); err != nil {
		t.Fatal(err)
	}
	after := validView(t, m, clockForTest(5))
	if !after.Available || !after.ValidUntil.Equal(initial.ValidUntil) || !after.Answers[0].ReceivedAt.Equal(initial.Answers[0].ReceivedAt) {
		t.Fatal("input renewal silently renewed a DNS answer")
	}
	m.result(resolution{}, errors.New("upstream unavailable"), clockForTest(19))
	after = validView(t, m, clockForTest(19))
	if !after.Available || after.LastError == "" || !after.ValidUntil.Equal(initial.ValidUntil) {
		t.Fatal("valid last-good answer was discarded or its TTL extended")
	}
	after = validView(t, m, clockForTest(20))
	if after.Available || after.Remote != "" {
		t.Fatal("expired last-good endpoint survived a DNS failure")
	}
}

func TestEndpointLifetimeIsMinimumOfEveryDependency(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"source-preferred", func(in *Input) { in.Source.PreferredUntil = clockForTest(4).wall }},
		{"source-valid", func(in *Input) {
			in.Source.PreferredUntil = clockForTest(4).wall
			in.Source.ValidUntil = clockForTest(4).wall
		}},
		{"dhcp6-name", func(in *Input) { in.Candidates[0].ValidUntil = clockForTest(4).wall }},
		{"learned-resolver", func(in *Input) { in.Resolvers[0].ValidUntil = clockForTest(4).wall }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &machine{config: configForTest()}
			in := inputForTest()
			tc.change(&in)
			if _, err := m.apply(in, clockForTest(0)); err != nil {
				t.Fatal(err)
			}
			m.result(goodResult(clockForTest(0), 30), nil, clockForTest(0))
			s := validView(t, m, clockForTest(3))
			if !s.Available || !s.ValidUntil.Equal(clockForTest(4).wall) {
				t.Fatal("endpoint outlived dependency")
			}
			if validView(t, m, clockForTest(4)).Available {
				t.Fatal("dependency expiry did not withdraw endpoint")
			}
		})
	}
}

func TestLiteralAndSelectionPolicies(t *testing.T) {
	m := testMachine(t)
	in := inputForTest()
	in.Generation++
	in.Candidates = []Candidate{{Value: "2001:db8:ffff::10", Origin: "configured"}}
	in.Resolvers = nil
	in.Source.PreferredUntil = time.Time{}
	in.Source.ValidUntil = time.Time{}
	if _, err := m.apply(in, clockForTest(0)); err != nil {
		t.Fatal(err)
	}
	m.result(resolution{candidate: 0, resolver: -1}, nil, clockForTest(0))
	s := validView(t, m, clockForTest(1))
	if !s.Available || s.Remote != "2001:db8:ffff::10" || !s.ValidUntil.IsZero() || s.ResolverIndex != -1 {
		t.Fatal("literal unexpectedly required DNS or acquired a fake expiry")
	}
	answers := []Answer{{Address: "2001:db8::3"}, {Address: "2001:db8::1"}, {Address: "2001:db8::2"}}
	for _, tc := range []struct {
		mode    string
		ordinal int
		want    string
		bad     bool
	}{
		{"", 0, "2001:db8::3", false}, {"first", 1, "2001:db8::3", false}, {"ordinal", 2, "2001:db8::2", false}, {"ordinalModulo", 5, "2001:db8::2", false}, {"ordinal", 4, "", true},
	} {
		c := configForTest()
		c.Selection, c.Ordinal = tc.mode, tc.ordinal
		a, err := chooseAnswer(c, answers)
		if (err != nil) != tc.bad || a.Address != tc.want {
			t.Fatalf("%s/%d: %+v %v", tc.mode, tc.ordinal, a, err)
		}
	}
}

func TestGenerationAndSnapshotAuthority(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	before := m.snapshot(clockForTest(0))
	if changed, err := m.apply(m.input, clockForTest(1)); err != nil || changed {
		t.Fatal("identical repeated generation changed state")
	}
	in := cloneInput(m.input)
	in.Source.Address = "2001:db8:1::2"
	if _, err := m.apply(in, clockForTest(1)); err == nil {
		t.Fatal("changed equal-generation input accepted")
	}
	in.Generation++
	if _, err := m.apply(in, clockForTest(1)); err != nil {
		t.Fatal(err)
	}
	if m.snapshot(clockForTest(1)).Available {
		t.Fatal("changed source reused a DNS lookup from its predecessor")
	}
	if ValidateSnapshot(m.config, m.input, before) == nil {
		t.Fatal("prior generation snapshot accepted against current input")
	}
	if _, err := m.apply(inputForTest(), clockForTest(2)); err == nil {
		t.Fatal("replayed older generation accepted")
	}
}

func TestSnapshotRejectsForgedFactsAndTTLZero(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	base := validView(t, m, clockForTest(0))
	for name, change := range map[string]func(*Snapshot){
		"local": func(s *Snapshot) { s.Local = "2001:db8:1::2" }, "remote": func(s *Snapshot) { s.Remote = "2001:db8:ffff::2" },
		"generation": func(s *Snapshot) { s.Generation++ }, "config": func(s *Snapshot) { s.ConfigDigest = strings.Repeat("b", 64) },
		"ttl-zero": func(s *Snapshot) { s.Answers[0].TTL = 0 }, "ttl-high-bit": func(s *Snapshot) { s.Answers[0].TTL = 1 << 31 },
		"unbounded-DNS": func(s *Snapshot) { s.ValidUntil = time.Time{} }, "late-expiry": func(s *Snapshot) { s.ValidUntil = clockForTest(21).wall },
		"answer-lifetime": func(s *Snapshot) { s.Answers[0].ValidUntil = clockForTest(21).wall },
		"future-answer":   func(s *Snapshot) { s.Answers[0].ReceivedAt = s.UpdatedAt.Add(time.Second) },
		"wrong-resolver":  func(s *Snapshot) { s.ResolverIndex = 1 }, "wrong-candidate": func(s *Snapshot) { s.CandidateIndex = 1 },
		"inactive-facts": func(s *Snapshot) { s.Active = false }, "unavailable-facts": func(s *Snapshot) { s.Available = false },
	} {
		t.Run(name, func(t *testing.T) {
			v := base
			v.Answers = append([]Answer(nil), base.Answers...)
			change(&v)
			if ValidateSnapshot(m.config, m.input, v) == nil {
				t.Fatal("forged endpoint accepted")
			}
		})
	}
}

func TestClockStepsDoNotExtendCachedDNS(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	now := clockForTest(10)
	now.wall = now.wall.Add(-time.Hour)
	s := validView(t, m, now)
	if !s.Available || s.ValidUntil.Sub(now.wall) != 10*time.Second {
		t.Fatal("backward clock step extended TTL")
	}
	now.wall = clockForTest(10).wall.Add(time.Hour)
	s = validView(t, m, now)
	if s.Available {
		t.Fatal("forward step exceeded unrefreshed compiler UTC lifetime scope")
	}
	if !reflect.DeepEqual(m.cache.answers, goodResult(clockForTest(0), 20).answers) {
		t.Fatal("clock step changed original cached clock deadlines")
	}
}

func TestSamplingJitterCannotChangeAbsoluteDNSTimes(t *testing.T) {
	m := testMachine(t)
	m.result(goodResult(clockForTest(0), 20), nil, clockForTest(0))
	first := validView(t, m, clockForTest(0))
	for i, jitter := range []time.Duration{7521 * time.Nanosecond, -12 * time.Microsecond, 800 * time.Microsecond, -500 * time.Millisecond} {
		now := clockForTest(i + 1)
		now.wall = now.wall.Add(jitter)
		m.attempts++
		m.result(resolution{}, errors.New("DNS silent"), now)
		s := validView(t, m, now)
		if !s.ValidUntil.Equal(first.ValidUntil) || !reflect.DeepEqual(s.Answers, first.Answers) {
			t.Fatalf("sampling jitter %v changed fixed answer/deadline: first=%+v next=%+v", jitter, first, s)
		}
		if !s.UpdatedAt.Equal(clockForTest(i + 1).wall) {
			t.Fatal("snapshot clock and receipt/deadline clock use different mappings")
		}
	}
	// A true clock step rebases once, retaining the same BOOTTIME expiry.
	now := clockForTest(10)
	now.wall = now.wall.Add(2 * time.Second)
	stepped := validView(t, m, now)
	if !stepped.ValidUntil.Equal(first.ValidUntil.Add(2*time.Second)) || stepped.ValidUntil.Sub(stepped.UpdatedAt) != 10*time.Second {
		t.Fatal("actual clock step was ignored or renewed the DNS TTL")
	}
	now = clockForTest(11)
	now.wall = now.wall.Add(2*time.Second + 7521*time.Nanosecond)
	after := validView(t, m, now)
	if !after.ValidUntil.Equal(stepped.ValidUntil) || !reflect.DeepEqual(after.Answers, stepped.Answers) {
		t.Fatal("jitter after a clock step changed the new fixed anchor")
	}
	now = clockForTest(20)
	now.wall = now.wall.Add(2 * time.Second)
	if s := validView(t, m, now); s.Available {
		t.Fatal("fixed wall mapping prevented BOOTTIME expiry")
	}
}
