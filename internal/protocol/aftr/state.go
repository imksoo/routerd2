// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
)

type instant struct{ wall, boot time.Time }
type resolution struct {
	candidate, resolver int
	answers             []Answer // boot-clock times, not UTC
	warning             string
}

type machine struct {
	config             Config
	input              Input   // exact public input for generation/scope validation
	learned            Input   // input deadlines translated once to boot time
	clock              instant // stable BOOTTIME-to-UTC mapping between clock steps
	cache              *resolution
	nextAttempt        time.Time
	attempts, failures uint64
	lastError          string
}

func cloneInput(in Input) Input {
	in.Candidates = slices.Clone(in.Candidates)
	in.Resolvers = slices.Clone(in.Resolvers)
	return in
}
func rebase(t, from, to time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return to.Add(t.Sub(from)).UTC()
}
func inputToBoot(in Input, now instant) Input {
	in = cloneInput(in)
	in.Source.PreferredUntil = rebase(in.Source.PreferredUntil, now.wall, now.boot)
	in.Source.ValidUntil = rebase(in.Source.ValidUntil, now.wall, now.boot)
	for i := range in.Candidates {
		in.Candidates[i].ValidUntil = rebase(in.Candidates[i].ValidUntil, now.wall, now.boot)
	}
	for i := range in.Resolvers {
		in.Resolvers[i].ValidUntil = rebase(in.Resolvers[i].ValidUntil, now.wall, now.boot)
	}
	return in
}

func clockShift(before, after instant) time.Duration {
	return after.wall.Sub(before.wall) - after.boot.Sub(before.boot)
}

// Consecutive CLOCK_BOOTTIME and wall-clock reads have sampling jitter. Reusing
// the same mapping preserves exact UTC deadlines across retries/status changes,
// so those publications cannot trigger a kernel apply for unchanged lifetimes.
// An actual offset change of at least one second starts a new mapping. UpdatedAt
// uses that mapping too, keeping receipt timestamps and TTL arithmetic coherent.
func (m *machine) stableClock(now instant) instant {
	shift := clockShift(m.clock, now)
	if m.clock.boot.IsZero() || shift >= time.Second || shift <= -time.Second {
		m.clock = now
	}
	now.wall = rebase(now.boot, m.clock.boot, m.clock.wall)
	return now
}

func queryIdentity(in Input) string {
	v := struct {
		Active                bool
		Local                 string
		Candidates, Resolvers []string
	}{Active: in.Active, Local: in.Source.Address}
	for _, x := range in.Candidates {
		v.Candidates = append(v.Candidates, candidateKey(x))
	}
	for _, x := range in.Resolvers {
		v.Resolvers = append(v.Resolvers, resolverKey(x))
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (m *machine) apply(in Input, now instant) (bool, error) {
	if err := ValidateInput(m.config, in); err != nil {
		return false, err
	}
	if in.Generation < m.input.Generation {
		return false, errors.New("stale AFTR input generation")
	}
	if in.Generation == m.input.Generation {
		if !reflect.DeepEqual(in, m.input) {
			return false, errors.New("AFTR input changed without advancing generation")
		}
		return false, nil
	}
	if queryIdentity(in) != queryIdentity(m.input) {
		m.cache = nil
	}
	m.input, m.learned = cloneInput(in), inputToBoot(in, now)
	m.nextAttempt = now.boot
	m.lastError = ""
	return true, nil
}

func (m *machine) dependencyError(now time.Time) string {
	in := m.learned
	if !in.Active {
		return "AFTR dependency input is inactive"
	}
	if in.Source.Address == "" {
		return "waiting for an eligible global IPv6 source"
	}
	if !alive(in.Source.PreferredUntil, now) || !alive(in.Source.ValidUntil, now) {
		return "local IPv6 source is no longer preferred and valid"
	}
	for _, c := range in.Candidates {
		if alive(c.ValidUntil, now) {
			return ""
		}
	}
	return "waiting for a live AFTR candidate"
}

func liveAnswers(answers []Answer, now time.Time) []Answer {
	out := make([]Answer, 0, len(answers))
	for _, a := range answers {
		if alive(a.ValidUntil, now) {
			out = append(out, a)
		}
	}
	return out
}

func chooseAnswer(c Config, answers []Answer) (Answer, error) {
	if len(answers) == 0 {
		return Answer{}, errors.New("no usable live AAAA answers")
	}
	if c.Selection == "" || c.Selection == "first" {
		return answers[0], nil
	}
	values := slices.Clone(answers)
	sort.Slice(values, func(i, j int) bool { return values[i].Address < values[j].Address })
	ordinal := c.Ordinal
	if ordinal == 0 {
		ordinal = 1
	}
	if c.Selection == "ordinalModulo" {
		return values[(ordinal-1)%len(values)], nil
	}
	if ordinal > len(values) {
		return Answer{}, fmt.Errorf("AAAA ordinal %d exceeds %d live answers", ordinal, len(values))
	}
	return values[ordinal-1], nil
}

func (m *machine) view(now instant) Snapshot {
	s := Snapshot{Resource: m.config.Resource, Interface: m.config.Interface, ConfigDigest: m.config.ConfigDigest, Generation: m.input.Generation, Active: true, CandidateIndex: -1, ResolverIndex: -1, UpdatedAt: now.boot, NextAttemptAt: m.nextAttempt, Attempts: m.attempts, Failures: m.failures, LastError: m.lastError}
	if err := m.dependencyError(now.boot); err != "" {
		s.LastError = err
		return s
	}
	if m.cache == nil {
		if s.LastError == "" {
			s.LastError = "waiting for AFTR resolution"
		}
		return s
	}
	cached := m.cache
	if cached.candidate < 0 || cached.candidate >= len(m.learned.Candidates) {
		s.LastError = "AFTR candidate is outside current input"
		return s
	}
	candidate := m.learned.Candidates[cached.candidate]
	if !alive(candidate.ValidUntil, now.boot) {
		s.LastError = "AFTR candidate lifetime expired"
		return s
	}
	deadline := minimum(m.learned.Source.PreferredUntil, m.learned.Source.ValidUntil, candidate.ValidUntil)
	remote := ""
	if a, err := netip.ParseAddr(candidate.Value); err == nil {
		remote = a.String()
	} else {
		if cached.resolver < 0 || cached.resolver >= len(m.learned.Resolvers) {
			s.LastError = "AFTR resolver is outside current input"
			return s
		}
		resolver := m.learned.Resolvers[cached.resolver]
		if !alive(resolver.ValidUntil, now.boot) {
			s.LastError = "AFTR resolver source lifetime expired"
			return s
		}
		answers := liveAnswers(cached.answers, now.boot)
		answer, err := chooseAnswer(m.config, answers)
		if err != nil {
			s.LastError = err.Error()
			return s
		}
		remote, deadline = answer.Address, minimum(deadline, resolver.ValidUntil, answer.ValidUntil)
		s.ResolverIndex, s.Answers = cached.resolver, answers
	}
	s.Available, s.Local, s.Remote, s.CandidateIndex, s.ValidUntil = true, m.learned.Source.Address, remote, cached.candidate, deadline
	return s
}

func (m *machine) snapshot(now instant) Snapshot {
	now = m.stableClock(now)
	s := m.view(now)
	s.UpdatedAt = now.wall.UTC()
	s.NextAttemptAt = rebase(s.NextAttemptAt, now.boot, now.wall)
	s.ValidUntil = rebase(s.ValidUntil, now.boot, now.wall)
	for i := range s.Answers {
		s.Answers[i].ReceivedAt = rebase(s.Answers[i].ReceivedAt, now.boot, now.wall)
		s.Answers[i].ValidUntil = rebase(s.Answers[i].ValidUntil, now.boot, now.wall)
	}
	if s.Available {
		// UTC input bounds belong to the compiler. If the wall clock stepped,
		// never assert an endpoint beyond those unrefreshed bounds.
		limit := minimum(m.input.Source.PreferredUntil, m.input.Source.ValidUntil, m.input.Candidates[s.CandidateIndex].ValidUntil)
		if s.ResolverIndex >= 0 {
			limit = minimum(limit, m.input.Resolvers[s.ResolverIndex].ValidUntil)
		}
		s.ValidUntil = minimum(s.ValidUntil, limit)
		if !alive(s.ValidUntil, now.wall) {
			s.Available, s.Local, s.Remote, s.CandidateIndex, s.ResolverIndex, s.Answers, s.ValidUntil = false, "", "", -1, -1, nil, time.Time{}
			s.LastError = "AFTR input deadlines require refresh after a wall-clock change"
		}
	}
	return s
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	v := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
	if len(v) > 1024 {
		v = v[:1024]
	}
	return v
}
func retry(c Config) time.Duration {
	if c.RetrySeconds == 0 {
		return 5 * time.Second
	}
	return time.Duration(c.RetrySeconds) * time.Second
}
func timeout(c Config) time.Duration {
	if c.TimeoutSeconds == 0 {
		return 3 * time.Second
	}
	return time.Duration(c.TimeoutSeconds) * time.Second
}

func (m *machine) result(value resolution, err error, now instant) {
	if err == nil {
		before := m.cache
		value.answers = slices.Clone(value.answers)
		m.cache = &value
		if !m.view(now).Available {
			err = errors.New("AFTR result expired before it could be applied")
			m.cache = before
		}
	}
	if err != nil {
		m.failures++
		m.lastError = errorText(err)
		m.nextAttempt = now.boot.Add(retry(m.config))
		return
	}
	m.lastError = value.warning
	m.nextAttempt = time.Time{}
	if len(value.answers) > 0 {
		answer, err := chooseAnswer(m.config, liveAnswers(value.answers, now.boot))
		if err != nil {
			m.nextAttempt = now.boot.Add(retry(m.config))
			return
		}
		delay := answer.ValidUntil.Sub(now.boot) / 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		if delay < 100*time.Millisecond {
			delay = 100 * time.Millisecond
		}
		m.nextAttempt = now.boot.Add(delay)
	}
	// Successful fallback must periodically retry earlier priorities, including
	// a literal fallback whose own address has no DNS TTL.
	if value.candidate > 0 || value.resolver > 0 || value.warning != "" {
		m.nextAttempt = minimum(m.nextAttempt, now.boot.Add(retry(m.config)))
	}
}
