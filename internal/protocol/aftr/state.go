// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

type resolution struct {
	candidate, resolver int
	answers             []Answer
	warning             string
}
type machine struct {
	config             Config
	input              Input
	cache              *resolution
	nextAttempt        uint64
	attempts, failures uint64
	lastError          string
}

func cloneInput(in Input) Input {
	in.Candidates = slices.Clone(in.Candidates)
	in.Resolvers = slices.Clone(in.Resolvers)
	return in
}

// SameQuery compares endpoint dependencies, independently of renewed lifetimes.
// A lifetime update must not interrupt a still-usable tunnel or DNS exchange.
func SameQuery(a, b Input) bool {
	if a.Resource != b.Resource || a.Interface != b.Interface || a.ConfigDigest != b.ConfigDigest || a.Active != b.Active || a.Source.Address != b.Source.Address || len(a.Candidates) != len(b.Candidates) || len(a.Resolvers) != len(b.Resolvers) {
		return false
	}
	for i, c := range a.Candidates {
		if candidateKey(c) != candidateKey(b.Candidates[i]) {
			return false
		}
	}
	for i, r := range a.Resolvers {
		if resolverKey(r) != resolverKey(b.Resolvers[i]) {
			return false
		}
	}
	return true
}
func (m *machine) apply(in Input, clock boottime.Clock) (bool, error) {
	if err := ValidateInput(m.config, in); err != nil {
		return false, err
	}
	if in.Generation < m.input.Generation {
		return false, errors.New("stale AFTR input generation")
	}
	queryChanged := !SameQuery(in, m.input)
	if in.Generation == m.input.Generation && queryChanged {
		return false, errors.New("AFTR query changed without advancing generation")
	}
	if reflect.DeepEqual(in, m.input) {
		return false, nil
	}
	if queryChanged {
		m.cache = nil
		m.nextAttempt = uint64(clock.BoottimeNanoseconds)
		m.lastError = ""
	}
	m.input = cloneInput(in)
	return queryChanged, nil
}
func (m *machine) dependencyError(clock boottime.Clock) string {
	in := m.input
	if !in.Active {
		return "AFTR dependency input is inactive"
	}
	if in.Source.Address == "" {
		return "waiting for an eligible global IPv6 source"
	}
	if !in.Source.PreferredUntil.Live(clock) || !in.Source.ValidUntil.Live(clock) {
		return "local IPv6 source is no longer preferred and valid"
	}
	for _, c := range in.Candidates {
		if c.ValidUntil.Live(clock) {
			return ""
		}
	}
	return "waiting for a live AFTR candidate"
}
func liveAnswers(answers []Answer, clock boottime.Clock) []Answer {
	out := make([]Answer, 0, len(answers))
	for _, a := range answers {
		if a.ValidUntil.Live(clock) {
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
func (m *machine) snapshot(clock boottime.Clock) Snapshot {
	s := Snapshot{BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), Resource: m.config.Resource, Interface: m.config.Interface, ConfigDigest: m.config.ConfigDigest, Generation: m.input.Generation, Active: true, CandidateIndex: -1, ResolverIndex: -1, UpdatedAt: clock.WallTime, NextAttemptNanos: m.nextAttempt, Attempts: m.attempts, Failures: m.failures, LastError: m.lastError}
	if err := m.dependencyError(clock); err != "" {
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
	candidate := m.input.Candidates[cached.candidate]
	if !candidate.ValidUntil.Live(clock) {
		s.LastError = "AFTR candidate lifetime expired"
		return s
	}
	deadline := lifetime.Minimum(clock, m.input.Source.PreferredUntil, m.input.Source.ValidUntil, candidate.ValidUntil)
	remote := ""
	if a, err := netip.ParseAddr(candidate.Value); err == nil {
		remote = a.String()
	} else {
		resolver := m.input.Resolvers[cached.resolver]
		if !resolver.ValidUntil.Live(clock) {
			s.LastError = "AFTR resolver source lifetime expired"
			return s
		}
		answers := liveAnswers(cached.answers, clock)
		answer, err := chooseAnswer(m.config, answers)
		if err != nil {
			s.LastError = err.Error()
			return s
		}
		remote, deadline = answer.Address, lifetime.Minimum(clock, deadline, resolver.ValidUntil, answer.ValidUntil)
		s.ResolverIndex, s.Answers = cached.resolver, answers
	}
	s.Available, s.Local, s.Remote, s.CandidateIndex, s.ValidUntil = true, m.input.Source.Address, remote, cached.candidate, deadline
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
func (m *machine) result(value resolution, err error, clock boottime.Clock) {
	if err == nil {
		before := m.cache
		value.answers = slices.Clone(value.answers)
		// DNS servers rotate RRset order. Keep the working endpoint when
		// the renewed answer still contains it; a refresh is not failover.
		if before != nil && (m.config.Selection == "" || m.config.Selection == "first") && before.candidate == value.candidate && before.resolver == value.resolver {
			if current, err := chooseAnswer(m.config, liveAnswers(before.answers, clock)); err == nil {
				for i, answer := range value.answers {
					if answer.Address == current.Address {
						value.answers[0], value.answers[i] = value.answers[i], value.answers[0]
						break
					}
				}
			}
		}
		m.cache = &value
		if !m.snapshot(clock).Available {
			err = errors.New("AFTR result expired before it could be applied")
			m.cache = before
		}
	}
	now := uint64(clock.BoottimeNanoseconds)
	if err != nil {
		m.failures++
		m.lastError = errorText(err)
		m.nextAttempt = now + uint64(retry(m.config))
		return
	}
	m.lastError = value.warning
	m.nextAttempt = 0
	if len(value.answers) > 0 {
		answer, err := chooseAnswer(m.config, liveAnswers(value.answers, clock))
		if err != nil {
			m.nextAttempt = now + uint64(retry(m.config))
			return
		}
		delay := max(100*time.Millisecond, min(30*time.Second, answer.ValidUntil.Remaining(clock)/2))
		m.nextAttempt = now + uint64(delay)
	}
	if value.candidate > 0 || value.resolver > 0 || value.warning != "" {
		earlier := now + uint64(retry(m.config))
		if m.nextAttempt == 0 || earlier < m.nextAttempt {
			m.nextAttempt = earlier
		}
	}
}
