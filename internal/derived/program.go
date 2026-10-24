package derived

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/imksoo/routerd/pkg/api"
)

type Program struct {
	name, digest string
	spec         api.DerivedEventSpec
	hold         uint64
	refs         []string
}

func Compile(name string, spec api.DerivedEventSpec) (Program, error) {
	if !token(name, 128) || !token(spec.Topic, 118) || len(spec.Inputs) > 64 {
		return Program{}, errors.New("invalid derived name, topic or input count")
	}
	if spec.EmitWhen != "" && spec.EmitWhen != "all_true" && spec.EmitWhen != "any_true" || spec.RetractWhen != "" && spec.RetractWhen != "any_false" && spec.RetractWhen != "all_false" {
		return Program{}, errors.New("invalid derived transition policy")
	}
	var hold uint64
	if spec.Hysteresis != "" {
		d, err := time.ParseDuration(spec.Hysteresis)
		if err != nil || d < 0 {
			return Program{}, errors.New("invalid derived hysteresis")
		}
		hold = uint64(d)
	}
	visited := 0
	refs := map[string]bool{}
	validate := func(p api.ReadyWhenPredicateSpec) error {
		visited++
		if visited > 1024 || len(p.Field) > 512 || len(p.Equals) > 4096 || !utf8.ValidString(p.Field) || !utf8.ValidString(p.Equals) {
			return errors.New("derived predicate exceeds bounds")
		}
		field := normalize(p.Field)
		if strings.Contains(field, ".status.") {
			parts := strings.SplitN(field, ".status.", 2)
			left := strings.Split(parts[0], "/")
			if len(left) != 2 || !token(left[0], 128) || !token(left[1], 128) || !token(parts[1], 128) {
				return errors.New("invalid derived status reference")
			}
			refs[field] = true
		}
		return nil
	}
	for _, input := range spec.Inputs {
		if len(input.AnyOf) > 64 {
			return Program{}, errors.New("derived any_of exceeds bounds")
		}
		if len(input.AnyOf) > 0 {
			for _, group := range input.AnyOf {
				if len(group) > 64 {
					return Program{}, errors.New("derived conjunction exceeds bounds")
				}
				for _, pred := range group {
					if err := validate(pred); err != nil {
						return Program{}, err
					}
				}
			}
		} else {
			if err := validate(api.ReadyWhenPredicateSpec{Field: input.Field, Equals: input.Equals, NotEmpty: input.NotEmpty}); err != nil {
				return Program{}, err
			}
		}
	}
	body, err := json.Marshal(struct {
		Name string
		Spec api.DerivedEventSpec
	}{name, spec})
	if err != nil {
		return Program{}, err
	}
	if len(body) > 1<<20 {
		return Program{}, errors.New("derived program exceeds 1 MiB")
	}
	var owned struct {
		Name string
		Spec api.DerivedEventSpec
	}
	if err = json.Unmarshal(body, &owned); err != nil {
		return Program{}, err
	}
	digest := sha256.Sum256(body)
	p := Program{name: name, digest: hex.EncodeToString(digest[:]), spec: owned.Spec, hold: hold}
	for ref := range refs {
		p.refs = append(p.refs, ref)
	}
	sort.Strings(p.refs)
	return p, nil
}

func predicate(input api.ReadyWhenPredicateSpec, s Snapshot) evaluated {
	value, known, until, observed := s.read(input.Field)
	if !known {
		return evaluated{truth: Indeterminate}
	}
	truth := True
	if input.NotEmpty && strings.TrimSpace(value) == "" || input.Equals != "" && value != input.Equals || input.Field != "" && !input.NotEmpty && input.Equals == "" && strings.TrimSpace(value) == "" {
		truth = False
	}
	return evaluated{truth: truth, until: until, proofUntil: until, observed: observed}
}

func (p Program) target(s Snapshot) evaluated {
	values := make([]evaluated, 0, len(p.spec.Inputs))
	for _, input := range p.spec.Inputs {
		if len(input.AnyOf) == 0 {
			values = append(values, predicate(api.ReadyWhenPredicateSpec{Field: input.Field, Equals: input.Equals, NotEmpty: input.NotEmpty}, s))
			continue
		}
		var groups []evaluated
		for _, group := range input.AnyOf {
			var terms []evaluated
			for _, input := range group {
				terms = append(terms, predicate(input, s))
			}
			groups = append(groups, combine(terms, true, s.now))
		}
		values = append(values, combine(groups, false, s.now))
	}
	return combine(values, p.spec.EmitWhen != "any_true", s.now)
}
