// SPDX-License-Identifier: BSD-3-Clause

package network

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/miekg/dns"
)

// Resolution and refresh timing follow routerd's IPAddressSet controller.
// Only this worker changes the owned firewall. DNS I/O never holds its mutex.
type resolvedAddressSet struct {
	intent     AddressSet
	addresses  []netip.Addr
	refresh    time.Duration
	next       time.Time
	resolvedAt time.Time
	lastError  string
}

func (e *Engine) AddressSets(ctx context.Context) (AddressSetSnapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return AddressSetSnapshot{}, err
	}
	snapshot := AddressSetSnapshot{Revision: e.state.Revision}
	for name, set := range e.addressSets {
		snapshot.Sets = append(snapshot.Sets, AddressSetStatus{Name: name, Addresses: slices.Clone(set.addresses), ResolvedAt: set.resolvedAt, Error: set.lastError})
	}
	slices.SortFunc(snapshot.Sets, func(a, b AddressSetStatus) int { return strings.Compare(a.Name, b.Name) })
	return snapshot, nil
}

func (e *Engine) prepareAddressSets(f *Firewall) {
	next := make(map[string]resolvedAddressSet)
	if f != nil {
		referenced := map[string]bool{}
		for _, rule := range f.LocalRedirects {
			referenced[rule.DestinationSet] = true
		}
		for _, set := range f.AddressSets {
			if !referenced[set.Name] || len(set.Names) == 0 {
				continue
			}
			current, exists := e.addressSets[set.Name]
			if !exists || !reflect.DeepEqual(current.intent, set) {
				current = resolvedAddressSet{intent: set}
				if set.RefreshInterval != "" {
					current.refresh, _ = time.ParseDuration(set.RefreshInterval)
				}
			}
			next[set.Name] = current
		}
	}
	e.addressSets = next
	select {
	case e.addressSetsWake <- struct{}{}:
	default:
	}
}

func (e *Engine) resolvedFirewall(f *Firewall) *Firewall {
	if f == nil {
		return nil
	}
	out := *f
	out.AddressSets = slices.Clone(f.AddressSets)
	for i := range out.AddressSets {
		set := &out.AddressSets[i]
		if learned, exists := e.addressSets[set.Name]; exists && reflect.DeepEqual(learned.intent, *set) {
			set.Addresses = append(slices.Clone(set.Addresses), learned.addresses...)
			slices.SortFunc(set.Addresses, netip.Addr.Compare)
			set.Addresses = slices.Compact(set.Addresses)
		}
	}
	return &out
}

// MaintainAddressSets has the same explicit process lifetime as authority
// maintenance. Callers cancel and join it before closing the engine.
func (e *Engine) MaintainAddressSets(ctx context.Context) error {
	for {
		e.mu.Lock()
		var due []resolvedAddressSet
		var earliest time.Time
		now := time.Now()
		for _, set := range e.addressSets {
			if !set.next.After(now) {
				due = append(due, set)
			} else if earliest.IsZero() || set.next.Before(earliest) {
				earliest = set.next
			}
		}
		e.mu.Unlock()
		for _, set := range due {
			addresses, ttl, lookupErr := resolveAddressSet(ctx, set.intent)
			if ctx.Err() != nil {
				return nil
			}
			interval := max(time.Minute, ttl/2)
			if set.refresh > 0 {
				interval = min(interval, set.refresh)
			}
			if lookupErr != nil {
				slog.Warn("address set resolution failed", "set", set.intent.Name, "error", lookupErr)
				interval = min(interval, time.Second)
			}
			e.mu.Lock()
			current, exists := e.addressSets[set.intent.Name]
			// Configuration can change while DNS is in flight. Its new owner must
			// never receive answers from the previous declaration.
			if exists && reflect.DeepEqual(current.intent, set.intent) {
				if lookupErr == nil || len(addresses) > 0 {
					current.addresses = addresses
					current.resolvedAt = time.Now()
				}
				current.lastError = ""
				if lookupErr != nil {
					current.lastError = lookupErr.Error()
				}
				current.next = time.Now().Add(interval)
				changed := !slices.Equal(set.addresses, current.addresses)
				e.addressSets[set.intent.Name] = current
				if changed {
					p := &apply{e: e, ctx: ctx}
					clock, err := boottime.Read()
					if err != nil {
						e.mu.Unlock()
						return err
					}
					currentFirewall := ProjectLifetimes(Desired{Firewall: e.state.Desired.Firewall}, clock).Firewall
					if err := p.firewall(currentFirewall); err != nil {
						e.mu.Unlock()
						return err
					}
				}
			}
			e.mu.Unlock()
		}
		if len(due) > 0 {
			continue
		}
		var wake <-chan time.Time
		var timer *time.Timer
		if !earliest.IsZero() {
			timer = time.NewTimer(time.Until(earliest))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
		case <-e.addressSetsWake:
		case <-wake:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func resolveAddressSet(ctx context.Context, set AddressSet) ([]netip.Addr, time.Duration, error) {
	servers := set.Resolvers
	var addresses []netip.Addr
	var ttl time.Duration
	var failures []error
	for _, name := range set.Names {
		for _, kind := range []uint16{dns.TypeA, dns.TypeAAAA} {
			message := new(dns.Msg).SetQuestion(dns.Fqdn(name), kind)
			var last error
			answered := false
			for _, server := range servers {
				client := dns.Client{Net: "udp", Timeout: 2 * time.Second}
				response, _, err := client.ExchangeContext(ctx, message, server)
				if err == nil && response.Truncated {
					client.Net = "tcp"
					response, _, err = client.ExchangeContext(ctx, message, server)
				}
				if err != nil {
					last = err
					continue
				}
				if response.Rcode != dns.RcodeSuccess && response.Rcode != dns.RcodeNameError {
					last = errors.New(dns.RcodeToString[response.Rcode])
					continue
				}
				answered = true
				for _, record := range response.Answer {
					var ip net.IP
					switch record := record.(type) {
					case *dns.A:
						if kind == dns.TypeA {
							ip = record.A
						}
					case *dns.AAAA:
						if kind == dns.TypeAAAA {
							ip = record.AAAA
						}
					}
					if a, ok := netip.AddrFromSlice(ip); ok {
						addresses = append(addresses, a.Unmap())
						duration := time.Duration(record.Header().Ttl) * time.Second
						if duration > 0 && (ttl == 0 || duration < ttl) {
							ttl = duration
						}
					}
				}
				break
			}
			if !answered {
				if last == nil {
					last = errors.New("no DNS resolver configured")
				}
				failures = append(failures, last)
			}
		}
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	return slices.Compact(addresses), ttl, errors.Join(failures...)
}
