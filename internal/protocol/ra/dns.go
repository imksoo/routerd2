//go:build linux

package ra

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"slices"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

// DNSServer is a learned RDNSS address whose source has an absolute lifetime.
// A zero ValidUntil is permitted only when the source explicitly has no expiry;
// absence of a learned source must be represented by removing the entry.
// Config.RDNSS takes precedence over a learned copy of the same address.
type DNSServer struct {
	Address    string            `json:"address"`
	ValidUntil lifetime.Deadline `json:"valid_until,omitempty"`
}

// SearchDomain is the DNSSL counterpart of DNSServer. Config.DNSSL takes
// precedence over a learned copy of the same canonical domain.
type SearchDomain struct {
	Domain     string            `json:"domain"`
	ValidUntil lifetime.Deadline `json:"valid_until,omitempty"`
}

type dnsEntry struct {
	Name      string
	Lifetimes []lifetime.Deadline
}

// A name appears once on the wire. Configured names have no source deadline;
// duplicate learned sources retain the name while any source is still valid.
// The longest deadline wins, with an explicit infinite source above finite ones.
func mergeDNS(configured []string, learned []dnsEntry) []dnsEntry {
	entries := make([]dnsEntry, 0, len(configured)+len(learned))
	index := make(map[string]int, len(configured)+len(learned))
	for _, name := range configured {
		if _, exists := index[name]; exists {
			continue
		}
		index[name] = len(entries)
		entries = append(entries, dnsEntry{Name: name, Lifetimes: []lifetime.Deadline{{}}})
	}
	for _, entry := range learned {
		if i, exists := index[entry.Name]; exists {
			entries[i].Lifetimes = append(entries[i].Lifetimes, entry.Lifetimes...)
			continue
		}
		index[entry.Name] = len(entries)
		entries = append(entries, entry)
	}
	return entries
}

func dnsEntries(c Config) (servers, domains []dnsEntry) {
	learned := make([]dnsEntry, 0, len(c.LearnedRDNSS))
	for _, entry := range c.LearnedRDNSS {
		learned = append(learned, dnsEntry{Name: entry.Address, Lifetimes: []lifetime.Deadline{entry.ValidUntil}})
	}
	servers = mergeDNS(c.RDNSS, learned)
	learned = make([]dnsEntry, 0, len(c.LearnedDNSSL))
	for _, entry := range c.LearnedDNSSL {
		learned = append(learned, dnsEntry{Name: entry.Domain, Lifetimes: []lifetime.Deadline{entry.ValidUntil}})
	}
	return servers, mergeDNS(c.DNSSL, learned)
}

func dnsNames(entries []dnsEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

type dnsGroup struct {
	Lifetime uint32
	Names    []string
}

// Options with different remaining lifetimes need distinct wire headers.
// Lifetime is rounded down so an advertisement never extends a source deadline.
func groupDNS(entries []dnsEntry, maximum uint32, clock boottime.Clock, withdraw bool) []dnsGroup {
	var groups []dnsGroup
	index := map[uint32]int{}
	for _, entry := range entries {
		var remaining uint32
		for _, d := range entry.Lifetimes {
			remaining = max(remaining, d.Seconds(maximum, clock))
		}
		if withdraw {
			remaining = 0
		}
		i, exists := index[remaining]
		if !exists {
			i = len(groups)
			index[remaining] = i
			groups = append(groups, dnsGroup{Lifetime: remaining})
		}
		groups[i].Names = append(groups[i].Names, entry.Name)
	}
	return groups
}

// Two different deadlines can initially have the same capped wire lifetime,
// then require separate options as time passes. Validate the largest possible
// grouping, not only the packet that happens to be encoded at validation time.
func maximumDNSSize(c Config) (int, error) {
	servers, domains := dnsEntries(c)
	groups := func(entries []dnsEntry) [][]string {
		var keys [][]lifetime.Deadline
		var names [][]string
		for _, entry := range entries {
			i := slices.IndexFunc(keys, func(key []lifetime.Deadline) bool { return slices.Equal(key, entry.Lifetimes) })
			if i < 0 {
				i = len(keys)
				keys = append(keys, entry.Lifetimes)
				names = append(names, nil)
			}
			names[i] = append(names[i], entry.Name)
		}
		return names
	}
	size := 0
	for _, names := range groups(servers) {
		if len(names) > 127 {
			return 0, errors.New("too many RDNSS addresses")
		}
		size += 8 + 16*len(names)
	}
	for _, names := range groups(domains) {
		length := 8
		for _, name := range names {
			encoded, err := domain(name)
			if err != nil {
				return 0, err
			}
			length += len(encoded)
		}
		length = (length + 7) / 8 * 8
		if length/8 > 255 {
			return 0, errors.New("DNSSL option exceeds wire limit")
		}
		size += length
	}
	return size, nil
}

func maximumPacketSize(c Config, hardware bool) (int, error) {
	dnsSize, err := maximumDNSSize(c)
	if err != nil {
		return 0, err
	}
	size := 16 + 32*len(c.Prefixes) + dnsSize
	if c.MTU != 0 {
		size += 8
	}
	if hardware {
		size += 8
	}
	return size, nil
}

func appendDNSOptions(packet []byte, c Config, clock boottime.Clock, withdraw bool) ([]byte, error) {
	servers, domains := dnsEntries(c)
	for _, group := range groupDNS(servers, c.Authority.Seconds(uint32(c.DNSSLifetimeSeconds), clock), clock, withdraw) {
		if len(group.Names) > 127 {
			return nil, errors.New("too many RDNSS addresses")
		}
		option := make([]byte, 8+16*len(group.Names))
		option[0], option[1] = 25, byte(len(option)/8)
		binary.BigEndian.PutUint32(option[4:], group.Lifetime)
		for i, name := range group.Names {
			address, err := netip.ParseAddr(name)
			if err != nil {
				return nil, err
			}
			copy(option[8+16*i:], address.AsSlice())
		}
		packet = append(packet, option...)
	}
	for _, group := range groupDNS(domains, c.Authority.Seconds(uint32(c.DNSSLifetimeSeconds), clock), clock, withdraw) {
		option := make([]byte, 8)
		option[0] = 31
		binary.BigEndian.PutUint32(option[4:], group.Lifetime)
		for _, name := range group.Names {
			encoded, err := domain(name)
			if err != nil {
				return nil, err
			}
			option = append(option, encoded...)
		}
		for len(option)%8 != 0 {
			option = append(option, 0)
		}
		if len(option)/8 > 255 {
			return nil, errors.New("DNSSL option exceeds wire limit")
		}
		option[1] = byte(len(option) / 8)
		packet = append(packet, option...)
	}
	return packet, nil
}
