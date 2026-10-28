// SPDX-License-Identifier: BSD-3-Clause

// Package aftr resolves bounded DS-Lite endpoint facts from explicit WAN inputs.
// It never writes kernel networking or consults the host resolver configuration.
package aftr

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

const (
	MaxCandidates = 4
	MaxResolvers  = 8
	MaxAnswers    = 64
	MaxCNAMEs     = 8
	MaxDNSTTL     = uint32(1<<31 - 1)
)

// Config is immutable. ConfigDigest is attached by the verified module loader
// after decoding the instance; it is not serialized into its own hash input.
// The default first policy preserves Linux routerd's ordered AAAA selection.
// ordinal/ordinalModulo explicitly select from canonical, lexically sorted IPs.
type Config struct {
	Resource       string `json:"resource"`
	Interface      string `json:"interface"`
	Selection      string `json:"selection,omitempty"`
	Ordinal        int    `json:"ordinal,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	RetrySeconds   int    `json:"retry_seconds,omitempty"`
	ConfigDigest   string `json:"-"`
}

type Source struct {
	Address        string            `json:"address"`
	PreferredUntil lifetime.Deadline `json:"preferred_until"`
	ValidUntil     lifetime.Deadline `json:"valid_until"`
}

type Candidate struct {
	Value      string            `json:"value"`
	Origin     string            `json:"origin"` // configured or dhcp6
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

type Resolver struct {
	Address    string            `json:"address"`        // IPv6 literal; no hostname or host DNS fallback
	Port       uint16            `json:"port,omitempty"` // zero means 53
	Interface  string            `json:"interface"`
	ValidUntil lifetime.Deadline `json:"valid_until"`
}

// Input is one complete, compiler-authorized dependency snapshot. Generation
// increases when endpoint dependencies change. Renewed lifetimes keep the same
// generation and update the budget without changing the DNS question.
// The selected Source must be installed and DAD-usable with a preferred grant.
// A dedicated PD /128 can be deprecated locally to prevent automatic source
// selection. Empty Source/Candidates/Resolvers express waiting for dependencies.
// Zero deadlines are explicit infinite/static lifetimes, never a TTL default.
type Input struct {
	Resource     string      `json:"resource"`
	Interface    string      `json:"interface"`
	ConfigDigest string      `json:"config_digest"`
	Generation   uint64      `json:"generation"`
	UpdatedAt    time.Time   `json:"updated_at"`
	Active       bool        `json:"active"`
	Source       Source      `json:"source"`
	Candidates   []Candidate `json:"candidates,omitempty"`
	Resolvers    []Resolver  `json:"resolvers,omitempty"`
}

// Answer.ValidUntil includes the minimum lifetime of its CNAME chain. TTL is
// the AAAA RRset's minimum TTL at ReceivedNanos; no input/status poll renews it.
type Answer struct {
	Address       string            `json:"address"`
	TTL           uint32            `json:"ttl"`
	ReceivedNanos uint64            `json:"received_nanos"`
	ValidUntil    lifetime.Deadline `json:"valid_until"`
}

type Snapshot struct {
	BootID           string            `json:"boot_id"`
	ObservedNanos    uint64            `json:"observed_nanos"`
	Resource         string            `json:"resource"`
	Interface        string            `json:"interface"`
	ConfigDigest     string            `json:"config_digest"`
	Generation       uint64            `json:"generation"`
	Active           bool              `json:"active"`
	Available        bool              `json:"available"`
	Local            string            `json:"local,omitempty"`
	Remote           string            `json:"remote,omitempty"`
	CandidateIndex   int               `json:"candidate_index"` // -1 when unavailable
	ResolverIndex    int               `json:"resolver_index"`  // -1 for a literal or unavailable
	Answers          []Answer          `json:"answers,omitempty"`
	ValidUntil       lifetime.Deadline `json:"valid_until"`
	UpdatedAt        time.Time         `json:"updated_at"`
	NextAttemptNanos uint64            `json:"next_attempt_nanos,omitempty"`
	Attempts         uint64            `json:"attempts"`
	Failures         uint64            `json:"failures"`
	LastError        string            `json:"last_error,omitempty"`
}

type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

func Validate(c Config) error {
	if strings.TrimSpace(c.Resource) == "" || len(c.Resource) > 128 || strings.ContainsAny(c.Resource, "\x00\r\n") {
		return errors.New("AFTR resource must be bounded and nonempty")
	}
	if !validInterface(c.Interface) {
		return errors.New("AFTR interface is invalid")
	}
	if c.Selection != "" && c.Selection != "first" && c.Selection != "ordinal" && c.Selection != "ordinalModulo" {
		return errors.New("AFTR selection must be first, ordinal or ordinalModulo")
	}
	if c.Ordinal < 0 || c.Ordinal > 65535 || (c.Selection == "" || c.Selection == "first") && c.Ordinal > 1 {
		return errors.New("AFTR ordinal requires an explicit ordinal selection policy and must be 1..65535")
	}
	if c.TimeoutSeconds < 0 || c.TimeoutSeconds > 30 || c.RetrySeconds < 0 || c.RetrySeconds > 300 {
		return errors.New("AFTR timeout/retry must be 1..30/1..300 seconds, or zero for defaults")
	}
	if c.ConfigDigest != "" && !validDigest(c.ConfigDigest) {
		return errors.New("AFTR config digest must be canonical SHA-256")
	}
	return nil
}

func validInterface(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 15 && !strings.ContainsAny(value, "/\x00 \t\r\n")
}
func validDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && value == strings.ToLower(value)
}
func validTime(t time.Time) bool { return t.IsZero() || t.Year() >= 1970 && t.Year() <= 9999 }
func bounded(value, limit lifetime.Deadline, clock boottime.Clock) bool {
	return value.Remaining(clock) <= limit.Remaining(clock)
}
func global(value string) bool {
	a, err := netip.ParseAddr(value)
	return err == nil && a.Is6() && !a.Is4In6() && a.IsGlobalUnicast() && !a.IsLinkLocalUnicast() && a.Zone() == "" && a.String() == value
}
func hostname(value string) bool {
	value = strings.TrimSuffix(strings.ToLower(value), ".")
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
func candidateKey(c Candidate) string {
	return strings.ToLower(strings.TrimSuffix(c.Value, ".")) + "|" + c.Origin
}
func resolverPort(r Resolver) uint16 {
	if r.Port == 0 {
		return 53
	}
	return r.Port
}
func resolverKey(r Resolver) string {
	return fmt.Sprintf("%s%%%s:%d", r.Address, r.Interface, resolverPort(r))
}

func ValidateInput(c Config, in Input) error {
	if err := Validate(c); err != nil {
		return err
	}
	if !validDigest(c.ConfigDigest) || in.ConfigDigest != c.ConfigDigest || in.Resource != c.Resource || in.Interface != c.Interface || in.Generation == 0 {
		return errors.New("AFTR input config/resource/interface/generation identity mismatch")
	}
	if in.UpdatedAt.IsZero() || !validTime(in.UpdatedAt) {
		return errors.New("AFTR input requires a bounded timestamp")
	}
	if in.Source.Address == "" {
		if in.Source.PreferredUntil != (lifetime.Deadline{}) || in.Source.ValidUntil != (lifetime.Deadline{}) {
			return errors.New("AFTR empty source cannot carry lifetimes")
		}
	} else if !global(in.Source.Address) || in.Source.ValidUntil.Validate() != nil || in.Source.PreferredUntil.Validate() != nil || !lifetime.Ordered(in.Source.PreferredUntil, in.Source.ValidUntil) {
		return errors.New("AFTR source must be canonical global IPv6 with ordered lifetimes")
	}
	if len(in.Candidates) > MaxCandidates || len(in.Resolvers) > MaxResolvers {
		return errors.New("AFTR input exceeds bounded candidate/resolver limits")
	}
	seen := map[string]bool{}
	for _, v := range in.Candidates {
		_, ipErr := netip.ParseAddr(v.Value)
		if len(v.Value) > 254 || v.Origin != "configured" && v.Origin != "dhcp6" || ipErr == nil && !global(v.Value) || ipErr != nil && !hostname(v.Value) || v.ValidUntil.Validate() != nil || seen[candidateKey(v)] {
			return errors.New("AFTR input has an invalid or duplicate candidate")
		}
		seen[candidateKey(v)] = true
	}
	seen = map[string]bool{}
	for _, v := range in.Resolvers {
		a, err := netip.ParseAddr(v.Address)
		if err != nil || !a.Is6() || a.Is4In6() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.Zone() != "" || a.String() != v.Address || v.Interface != c.Interface || v.ValidUntil.Validate() != nil || seen[resolverKey(v)] {
			return errors.New("AFTR resolver must be a unique literal IPv6 server scoped to the underlay; implicit/loopback/IPv4 DNS is forbidden")
		}
		seen[resolverKey(v)] = true
	}
	return nil
}

// ValidateSnapshot checks endpoint identity and the original DNS answer budget.
// Consumers intersect its deadline with current source/candidate/resolver budgets;
// those can be renewed or shortened while this snapshot is being delivered.
// It cannot establish authenticity of DNS itself; the transport checks source,
// ID, question and chain integrity before creating these bounded facts.
func ValidateSnapshot(c Config, in Input, s Snapshot) error {
	if err := ValidateInput(c, in); err != nil {
		return err
	}
	if s.Resource != c.Resource || s.Interface != c.Interface || s.ConfigDigest != c.ConfigDigest || s.Generation != in.Generation || s.UpdatedAt.IsZero() || !validTime(s.UpdatedAt) || !boottime.ValidBootID(s.BootID) || s.ObservedNanos == 0 || s.ObservedNanos > 1<<63-1 || len(s.LastError) > 1024 || len(s.Answers) > MaxAnswers {
		return errors.New("AFTR snapshot identity or bounded metadata is invalid")
	}
	if !s.Available {
		if s.Local != "" || s.Remote != "" || s.CandidateIndex != -1 || s.ResolverIndex != -1 || len(s.Answers) != 0 || s.ValidUntil != (lifetime.Deadline{}) {
			return errors.New("unavailable AFTR snapshot contains installable endpoint facts")
		}
		return nil
	}
	clock := boottime.Clock{BootID: s.BootID, BoottimeNanoseconds: int64(s.ObservedNanos), WallTime: s.UpdatedAt}
	if !s.Active || !in.Active || s.Local != in.Source.Address || !global(s.Local) || !global(s.Remote) || !in.Source.PreferredUntil.Live(clock) || !in.Source.ValidUntil.Live(clock) || s.ValidUntil.Validate() != nil || !s.ValidUntil.Live(clock) {
		return errors.New("AFTR snapshot uses an unavailable source or outlives it")
	}
	if s.CandidateIndex < 0 || s.CandidateIndex >= len(in.Candidates) {
		return errors.New("AFTR snapshot candidate is outside current input")
	}
	candidate := in.Candidates[s.CandidateIndex]
	if !candidate.ValidUntil.Live(clock) {
		return errors.New("AFTR snapshot outlives its candidate")
	}
	if ip, err := netip.ParseAddr(candidate.Value); err == nil {
		if s.Remote != ip.String() || s.ResolverIndex != -1 || len(s.Answers) != 0 {
			return errors.New("literal AFTR snapshot differs from its configured candidate")
		}
		return nil
	}
	if s.ResolverIndex < 0 || s.ResolverIndex >= len(in.Resolvers) || len(s.Answers) == 0 || s.ValidUntil == (lifetime.Deadline{}) {
		return errors.New("DNS AFTR snapshot lacks scoped resolver/finite answer lifetime")
	}
	resolver := in.Resolvers[s.ResolverIndex]
	if !resolver.ValidUntil.Live(clock) {
		return errors.New("AFTR snapshot outlives its resolver source")
	}
	seen, selected := map[string]bool{}, false
	for _, a := range s.Answers {
		if !global(a.Address) || a.TTL == 0 || a.TTL > MaxDNSTTL || a.ReceivedNanos == 0 || a.ReceivedNanos > s.ObservedNanos || a.ValidUntil.BootID != s.BootID || a.ValidUntil.Nanos == 0 || a.ValidUntil.Validate() != nil || !a.ValidUntil.Live(clock) || a.ValidUntil.Nanos > a.ReceivedNanos+uint64(a.TTL)*uint64(time.Second) || seen[a.Address] {
			return errors.New("AFTR snapshot contains invalid, expired or duplicate DNS answers")
		}
		seen[a.Address] = true
		if a.Address == s.Remote {
			selected = true
			if !bounded(s.ValidUntil, a.ValidUntil, clock) {
				return errors.New("AFTR endpoint outlives its DNS answer")
			}
		}
	}
	if !selected {
		return errors.New("AFTR endpoint is absent from its DNS answers")
	}
	chosen, err := chooseAnswer(c, s.Answers)
	if err != nil || chosen.Address != s.Remote {
		return errors.New("AFTR endpoint violates the configured AAAA selection policy")
	}
	return nil
}
