package dhcp6server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/miekg/dns"
)

type request struct {
	xid               [3]byte
	client            []byte
	dns, domains, ntp bool
}

func decode(b, server []byte) (request, error) {
	if len(b) < 4 || b[0] != 11 {
		return request{}, errors.New("not a DHCPv6 information request")
	}
	r := request{xid: [3]byte(b[1:4])}
	for b = b[4:]; len(b) > 0; {
		if len(b) < 4 {
			return request{}, errors.New("short DHCPv6 option header")
		}
		code, size := binary.BigEndian.Uint16(b), int(binary.BigEndian.Uint16(b[2:]))
		b = b[4:]
		if size > len(b) {
			return request{}, errors.New("short DHCPv6 option body")
		}
		value := b[:size]
		b = b[size:]
		switch code {
		case 1:
			if len(value) > 128 {
				return request{}, errors.New("client DUID exceeds 128 bytes")
			}
			r.client = value
		case 2:
			if !bytes.Equal(value, server) {
				return request{}, errors.New("information request selects another server")
			}
		case 6:
			for len(value) >= 2 {
				switch binary.BigEndian.Uint16(value) {
				case 23:
					r.dns = true
				case 24:
					r.domains = true
				case 31:
					r.ntp = true
				}
				value = value[2:]
			}
		}
	}
	return r, nil
}

func option(b []byte, code uint16, value []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, code)
	b = binary.BigEndian.AppendUint16(b, uint16(len(value)))
	return append(b, value...)
}

type options struct {
	dns, ntp, domains []byte
	expires           uint64
}

func prepare(c dhcp6serverconfig.Config, in dhcp6serverconfig.Input, clock boottime.Clock) (options, error) {
	var out options
	live := func(d lifetime.Deadline) bool {
		if !d.Live(clock) {
			return false
		}
		if d.Nanos != 0 && (out.expires == 0 || d.Nanos < out.expires) {
			out.expires = d.Nanos
		}
		return true
	}
	for _, set := range []struct {
		static  []netip.Addr
		learned []dhcp6serverconfig.Address
		dest    *[]byte
	}{{c.DNSServers, in.DNSServers, &out.dns}, {c.SNTPServers, in.SNTPServers, &out.ntp}} {
		seen := map[netip.Addr]bool{}
		add := func(ip netip.Addr) {
			if !seen[ip] {
				seen[ip] = true
				*set.dest = append(*set.dest, ip.AsSlice()...)
			}
		}
		for _, ip := range set.static {
			add(ip)
		}
		for _, entry := range set.learned {
			if live(entry.ValidUntil) {
				add(entry.Address)
			}
		}
	}
	seen := map[string]bool{}
	add := func(name string) error {
		name = strings.ToLower(dns.Fqdn(name))
		if seen[name] {
			return nil
		}
		seen[name] = true
		b := make([]byte, len(name)+1)
		n, err := dns.PackDomainName(name, b, 0, nil, false)
		if err != nil {
			return err
		}
		out.domains = append(out.domains, b[:n]...)
		return nil
	}
	for _, name := range c.DomainSearch {
		if err := add(name); err != nil {
			return out, err
		}
	}
	for _, entry := range in.DomainSearch {
		if live(entry.ValidUntil) {
			if err := add(entry.Name); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func reply(r request, server []byte, data options, refresh uint32) []byte {
	b := []byte{7, r.xid[0], r.xid[1], r.xid[2]}
	b = option(b, 2, server)
	if len(r.client) > 0 {
		b = option(b, 1, r.client)
	}
	if r.dns && len(data.dns) > 0 {
		b = option(b, 23, data.dns)
	}
	if r.domains && len(data.domains) > 0 {
		b = option(b, 24, data.domains)
	}
	if r.ntp && len(data.ntp) > 0 {
		b = option(b, 31, data.ntp)
	}
	return option(b, 32, binary.BigEndian.AppendUint32(nil, refresh))
}
