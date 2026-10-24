//go:build linux

package ra

import (
	"errors"
	"net"
	"reflect"
	"sort"
	"time"
)

// Three advertisements reduce a single lost multicast's impact. The outer loop
// applies the same minimum multicast spacing to updates and solicitations.
const withdrawalAdvertisements = 3

type withdrawnPrefix struct {
	Prefix    Prefix
	Remaining int
}
type withdrawals struct {
	Prefixes map[string]withdrawnPrefix
	RDNSS    map[string]int
	DNSSL    map[string]int
}

func (w withdrawals) clone() withdrawals {
	c := withdrawals{Prefixes: map[string]withdrawnPrefix{}, RDNSS: map[string]int{}, DNSSL: map[string]int{}}
	for k, v := range w.Prefixes {
		c.Prefixes[k] = v
	}
	for k, v := range w.RDNSS {
		c.RDNSS[k] = v
	}
	for k, v := range w.DNSSL {
		c.DNSSL[k] = v
	}
	return c
}
func (w withdrawals) any() bool { return len(w.Prefixes)+len(w.RDNSS)+len(w.DNSSL) > 0 }
func (w withdrawals) prefixNames() []string {
	var names []string
	for name := range w.Prefixes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (w withdrawals) sent() {
	for name, p := range w.Prefixes {
		p.Remaining--
		if p.Remaining <= 0 {
			delete(w.Prefixes, name)
		} else {
			w.Prefixes[name] = p
		}
	}
	for _, names := range []map[string]int{w.RDNSS, w.DNSSL} {
		for name, n := range names {
			if n <= 1 {
				delete(names, name)
			} else {
				names[name] = n - 1
			}
		}
	}
}

func equivalentAt(a, b Config, now time.Time) bool {
	a, b = normalized(a), normalized(b)
	for _, c := range []*Config{&a, &b} {
		for i := range c.Prefixes {
			p := &c.Prefixes[i]
			p.ValidLifetimeSeconds = lifetime(p.ValidLifetimeSeconds, p.ValidUntil, now)
			p.PreferredLifetimeSeconds = lifetime(p.PreferredLifetimeSeconds, p.PreferredUntil, now)
		}
	}
	return reflect.DeepEqual(a, b)
}

func planUpdate(current, candidate Config, pending withdrawals, hardware net.HardwareAddr, mtu int, now time.Time) (Config, withdrawals, bool, error) {
	candidate = normalized(candidate)
	if err := Validate(candidate); err != nil {
		return current, pending, false, err
	}
	if candidate.Resource != current.Resource || candidate.Interface != current.Interface || candidate.SourceAddress != current.SourceAddress {
		return current, pending, false, errors.New("RA live update cannot change resource, interface or source address")
	}
	if candidate.MTU > mtu {
		return current, pending, false, errors.New("advertised RA MTU exceeds interface MTU")
	}
	if equivalentAt(current, candidate, now) {
		return current, pending, false, nil
	}
	updated := pending.clone()
	nextPrefixes := map[string]bool{}
	for _, p := range candidate.Prefixes {
		nextPrefixes[p.Prefix] = true
		delete(updated.Prefixes, p.Prefix)
	}
	for _, p := range current.Prefixes {
		if !nextPrefixes[p.Prefix] {
			p.ValidLifetimeSeconds, p.PreferredLifetimeSeconds = 0, 0
			p.ValidUntil, p.PreferredUntil = time.Time{}, time.Time{}
			updated.Prefixes[p.Prefix] = withdrawnPrefix{Prefix: p, Remaining: withdrawalAdvertisements}
		}
	}
	updateNames := func(old, new []string, removed map[string]int) {
		next := map[string]bool{}
		for _, name := range new {
			next[name] = true
			delete(removed, name)
		}
		for _, name := range old {
			if !next[name] {
				removed[name] = withdrawalAdvertisements
			}
		}
	}
	currentServers, currentDomains := dnsEntries(current)
	nextServers, nextDomains := dnsEntries(candidate)
	updateNames(dnsNames(currentServers), dnsNames(nextServers), updated.RDNSS)
	updateNames(dnsNames(currentDomains), dnsNames(nextDomains), updated.DNSSL)
	maximum, err := maximumPacketSize(candidate, len(hardware) == 6)
	if err != nil {
		return current, pending, false, err
	}
	removed := Config{RDNSS: make([]string, 0, len(updated.RDNSS)), DNSSL: make([]string, 0, len(updated.DNSSL))}
	for name := range updated.RDNSS {
		removed.RDNSS = append(removed.RDNSS, name)
	}
	for name := range updated.DNSSL {
		removed.DNSSL = append(removed.DNSSL, name)
	}
	withdrawnDNS, err := maximumDNSSize(removed)
	if err != nil {
		return current, pending, false, err
	}
	if maximum+32*len(updated.Prefixes)+withdrawnDNS > 1240 {
		return current, pending, false, errors.New("RA update plus withdrawal options exceed minimum IPv6 MTU; reduce the transition size")
	}
	return candidate, updated, true, nil
}

func encodeWithWithdrawals(cfg Config, pending withdrawals, hardware net.HardwareAddr, now time.Time, stop bool) ([]byte, error) {
	cfg = normalized(cfg)
	for _, name := range pending.prefixNames() {
		cfg.Prefixes = append(cfg.Prefixes, pending.Prefixes[name].Prefix)
	}
	payload, err := encode(cfg, hardware, now, stop)
	if err != nil {
		return nil, err
	}
	removed := Config{}
	for name := range pending.RDNSS {
		removed.RDNSS = append(removed.RDNSS, name)
	}
	for name := range pending.DNSSL {
		removed.DNSSL = append(removed.DNSSL, name)
	}
	sort.Strings(removed.RDNSS)
	sort.Strings(removed.DNSSL)
	if len(removed.RDNSS)+len(removed.DNSSL) > 0 {
		extra, err := encode(removed, nil, now, true)
		if err != nil {
			return nil, err
		}
		payload = append(payload, extra[16:]...)
	}
	return payload, nil
}
