// Package eventrule evaluates historical event patterns without goroutines,
// external effects or a wall-clock dependency. The caller must commit the
// returned state and emissions with its durable consumer checkpoint.
package eventrule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
)

const maxStateBytes = 64 << 10
const maxKeys = 4096
const maxOutputs = 64

var ErrCapacity = errors.New("event rule state or output capacity exhausted")

type Input struct {
	ID        string                `json:"id"`
	Event     daemonapi.DaemonEvent `json:"event"`
	ExpiresAt time.Time             `json:"expires_at,omitempty"`
}
type Emission struct {
	ID    string                `json:"id"`
	Event daemonapi.DaemonEvent `json:"event"`
}
type Result struct {
	State     json.RawMessage `json:"state"`
	Emissions []Emission      `json:"emissions"`
}
type Program struct {
	name, digest string
	spec         api.EventRuleSpec
	topics       []string
	delay        time.Duration
	threshold    int
}

func token(s string, maximum int) bool {
	return s != "" && len(s) <= maximum && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func Compile(name string, spec api.EventRuleSpec) (Program, error) {
	if !token(name, 128) || !token(spec.Emit.Topic, 128) || len(spec.Emit.Attributes) > 64 || spec.Pattern.Threshold < 0 || spec.Pattern.Rate < 0 {
		return Program{}, errors.New("invalid event rule name, output or threshold")
	}
	if len(spec.Pattern.Topics) > 64 || len(spec.Pattern.Operator) > 32 {
		return Program{}, errors.New("event rule input pattern exceeds bounds")
	}
	for key, value := range spec.Emit.Attributes {
		if !token(key, 128) || len(value) > 4096 || !utf8.ValidString(value) {
			return Program{}, errors.New("invalid event rule attribute template")
		}
	}
	correlate := strings.TrimSpace(spec.Pattern.CorrelateBy)
	switch correlate {
	case "", "resource.name", "resource.kind", "resource.apiVersion", "daemon.instance", "daemon.kind":
	default:
		if !strings.HasPrefix(correlate, "attributes.") || !token(strings.TrimPrefix(correlate, "attributes."), 128) {
			return Program{}, errors.New("unsupported event rule correlation")
		}
	}
	body, err := json.Marshal(struct {
		Name string
		Spec api.EventRuleSpec
	}{name, spec})
	if err != nil {
		return Program{}, err
	}
	// Own the maps/slices before the caller can change its configuration.
	var owned struct {
		Name string
		Spec api.EventRuleSpec
	}
	if err = json.Unmarshal(body, &owned); err != nil {
		return Program{}, err
	}
	digest := sha256.Sum256(body)
	p := Program{name: name, spec: owned.Spec, digest: hex.EncodeToString(digest[:]), threshold: spec.Pattern.Threshold}
	p.spec.Pattern.CorrelateBy = correlate
	if p.threshold == 0 {
		p.threshold = 1
	}
	p.topics = append(p.topics, p.spec.Pattern.Topics...)
	if p.spec.Pattern.Topic != "" {
		p.topics = append(p.topics, p.spec.Pattern.Topic)
	}
	if p.spec.Pattern.Trigger != "" {
		p.topics = append(p.topics, p.spec.Pattern.Trigger)
	}
	if len(p.topics) == 0 || len(p.topics) > 64 {
		return Program{}, errors.New("event rule needs 1..64 input topics")
	}
	for _, topic := range p.topics {
		if !token(topic, 128) {
			return Program{}, errors.New("invalid event rule topic")
		}
	}
	// Validate every supplied duration, including a field masked by another
	// alias. Invalid input must not silently select a default.
	for _, raw := range []string{p.spec.Pattern.Duration, p.spec.Pattern.Window, p.spec.Pattern.Interval, p.spec.Pattern.Quiet} {
		if raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return Program{}, fmt.Errorf("invalid positive event rule duration %q", raw)
			}
		}
	}
	fallback := func(values ...string) time.Duration {
		for _, v := range values {
			if v != "" {
				d, _ := time.ParseDuration(v)
				return d
			}
		}
		return time.Minute
	}
	switch p.spec.Pattern.Operator {
	case "all_of", "any_of", "sequence", "count":
	case "window":
		p.delay = fallback(p.spec.Pattern.Window, p.spec.Pattern.Duration)
	case "throttle":
		p.delay = fallback(p.spec.Pattern.Interval, p.spec.Pattern.Duration)
	case "absence":
		if !token(p.spec.Pattern.Expected, 128) || p.spec.Pattern.Trigger == "" && p.spec.Pattern.Topic == "" {
			return Program{}, errors.New("absence needs an expected topic")
		}
		p.delay = fallback(p.spec.Pattern.Duration)
	case "debounce":
		if p.spec.Pattern.Quiet == "" && p.spec.Pattern.Duration == "" {
			p.delay = time.Second
		} else {
			p.delay = fallback(p.spec.Pattern.Quiet, p.spec.Pattern.Duration)
		}
	default:
		return Program{}, errors.New("unsupported event rule operator")
	}
	return p, nil
}
