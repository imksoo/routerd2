package network

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// PolicyRule selects a routing table. This API implements the to-table action;
// goto, suppression, l3mdev, uid/port ranges and other actions are not inferred.
// Family is 4 or 6. Each family/priority slot is exclusive to one rule and must
// not collide with a rule installed by another controller.
type PolicyRule struct {
	Family       int               `json:"family"`
	Priority     int               `json:"priority"`
	Table        int               `json:"table"`
	Source       string            `json:"source,omitempty"`
	Destination  string            `json:"destination,omitempty"`
	InInterface  string            `json:"in_interface,omitempty"`
	OutInterface string            `json:"out_interface,omitempty"`
	Mark         *uint32           `json:"mark,omitempty"`
	Mask         *uint32           `json:"mask,omitempty"`
	Invert       bool              `json:"invert,omitempty"`
	Authority    lifetime.Deadline `json:"authority,omitzero"`
}

type PolicyRuleState struct {
	PolicyRule
	Protocol int  `json:"protocol"`
	Action   int  `json:"action"`
	Advanced bool `json:"advanced,omitempty"`
	Owned    bool `json:"owned"`
}

func policyRuleSlot(r PolicyRule) string { return fmt.Sprintf("%d/%d", r.Family, r.Priority) }
func samePolicyRule(a, b PolicyRule) bool {
	// Authority belongs to the writer; it is not a Linux rule attribute.
	a.Authority, b.Authority = lifetime.Deadline{}, lifetime.Deadline{}
	return reflect.DeepEqual(normalizePolicyRule(a), normalizePolicyRule(b))
}
func policyRuleMatches(live PolicyRuleState, want PolicyRule) bool {
	return live.Protocol == RouteProtocol && live.Action == 1 && !live.Advanced && samePolicyRule(live.PolicyRule, want)
}

func validatePolicyRules(rules []PolicyRule) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seen := map[string]bool{}
	for _, r := range rules {
		if r.Authority != (lifetime.Deadline{}) && (!boottime.ValidBootID(r.Authority.BootID) || r.Authority.Nanos == 0 || r.Authority.Nanos > 1<<63-1 || !r.Authority.UTC.IsZero()) {
			add("policy rule %d: invalid boot authority", r.Priority)
		}
		if r.Family != 4 && r.Family != 6 {
			add("policy rule %d: family must be 4 or 6", r.Priority)
		}
		if r.Priority < 1 || uint64(r.Priority) > uint64(^uint32(0)>>1) || r.Priority == 32766 || r.Priority == 32767 {
			add("policy rule: priority %d is invalid or reserved", r.Priority)
		}
		if r.Table < 1 || uint64(r.Table) > uint64(^uint32(0)) || r.Table == 255 {
			add("policy rule %d: invalid or reserved route table", r.Priority)
		}
		for _, s := range []string{r.Source, r.Destination} {
			if s == "" {
				continue
			}
			p, err := netip.ParsePrefix(s)
			if err != nil || p.Addr().Is4In6() || p != p.Masked() || p.Addr().Is4() != (r.Family == 4) {
				add("policy rule %d: %q must be a same-family network prefix", r.Priority, s)
			}
		}
		for _, name := range []string{r.InInterface, r.OutInterface} {
			if name != "" && !validName(name) {
				add("policy rule %d: invalid interface %q", r.Priority, name)
			}
		}
		if r.Mask != nil && r.Mark == nil {
			add("policy rule %d: mask requires a mark", r.Priority)
		}
		if r.Mask != nil && r.Mark != nil && *r.Mark & ^*r.Mask != 0 {
			add("policy rule %d: mark contains bits outside its mask", r.Priority)
		}
		if seen[policyRuleSlot(r)] {
			add("duplicate policy rule family/priority %s", policyRuleSlot(r))
		}
		seen[policyRuleSlot(r)] = true
	}
	return errors.Join(errs...)
}

func normalizePolicyRule(r PolicyRule) PolicyRule {
	for _, s := range []*string{&r.Source, &r.Destination} {
		if *s != "" {
			if p, err := netip.ParsePrefix(*s); err == nil {
				if p.Bits() == 0 {
					*s = ""
				} else {
					*s = p.Masked().String()
				}
			}
		}
	}
	if r.Mark != nil && r.Mask == nil {
		mask := ^uint32(0)
		r.Mask = &mask
	}
	return r
}

func (e *Engine) ownsPolicyRule(live PolicyRuleState) bool {
	for _, saved := range e.state.PolicyRules {
		if policyRuleMatches(live, saved) {
			return true
		}
	}
	return false
}

func (p *apply) preflightPolicyRules(want []PolicyRule) error {
	if len(want) == 0 {
		return nil
	}
	live, err := p.e.kernel.PolicyRules()
	if err != nil {
		return p.failure("policy rule read", err)
	}
	for _, r := range want {
		for _, v := range live {
			if policyRuleSlot(v.PolicyRule) == policyRuleSlot(r) && !p.e.ownsPolicyRule(v) {
				return p.failure("policy rule ownership", fmt.Errorf("family %d priority %d is occupied by an unowned or changed rule", r.Family, r.Priority))
			}
		}
	}
	return nil
}

func (p *apply) removePolicyRules(want []PolicyRule) error {
	for i := 0; i < len(p.e.state.PolicyRules); {
		r := p.e.state.PolicyRules[i]
		keep := false
		for _, w := range want {
			if samePolicyRule(r, w) {
				p.e.state.PolicyRules[i] = w
				keep = true
				break
			}
		}
		if keep {
			i++
			continue
		}
		if err := p.removePolicyRule(i); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) removePolicyRule(i int) error {
	r := p.e.state.PolicyRules[i]
	live, err := p.e.kernel.PolicyRules()
	if err != nil {
		return p.failure("policy rule remove read", err)
	}
	found := false
	for _, v := range live {
		if policyRuleSlot(v.PolicyRule) != policyRuleSlot(r) {
			continue
		}
		if !policyRuleMatches(v, r) {
			return p.failure("policy rule remove ownership", fmt.Errorf("family %d priority %d changed outside routerd2", r.Family, r.Priority))
		}
		if found {
			return p.failure("policy rule remove ownership", fmt.Errorf("family %d priority %d has duplicate live rules", r.Family, r.Priority))
		}
		found = true
	}
	forget := func() { p.e.state.PolicyRules = append(p.e.state.PolicyRules[:i], p.e.state.PolicyRules[i+1:]...) }
	if found {
		if err := p.change("policy rule remove", func() error { return p.e.kernel.DeletePolicyRule(r) }, forget); err != nil {
			return err
		}
	} else {
		forget()
		if err := p.checkpoint("forget absent policy rule"); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) policyRules(want []PolicyRule) error {
	for _, r := range want {
		live, err := p.e.kernel.PolicyRules()
		if err != nil {
			return p.failure("policy rule read", err)
		}
		found := false
		for _, v := range live {
			if policyRuleSlot(v.PolicyRule) != policyRuleSlot(r) {
				continue
			}
			if !policyRuleMatches(v, r) || !p.e.ownsPolicyRule(v) || found {
				return p.failure("policy rule ownership", fmt.Errorf("family %d priority %d conflicts with an unowned, changed or duplicate rule", r.Family, r.Priority))
			}
			found = true
		}
		if found {
			continue
		}
		for _, name := range []string{r.InInterface, r.OutInterface} {
			if name != "" {
				if _, err := p.e.kernel.Interface(name); err != nil {
					return p.failure("policy rule interface", err)
				}
			}
		}
		if err := p.change("policy rule add", func() error {
			clock, err := boottime.Read()
			if err != nil {
				return err
			}
			if !r.Authority.Live(clock) {
				return errors.New("policy rule authority expired before installation")
			}
			return p.e.kernel.AddPolicyRule(r)
		}, func() {
			for _, old := range p.e.state.PolicyRules {
				if samePolicyRule(old, r) {
					return
				}
			}
			p.e.state.PolicyRules = append(p.e.state.PolicyRules, r)
		}); err != nil {
			return err
		}
	}
	return nil
}
