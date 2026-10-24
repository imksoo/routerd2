//go:build linux

// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

func resolve(ctx context.Context, c Config, in Input) (resolution, error) {
	var failures []string
	for candidateIndex, candidate := range in.Candidates {
		if err := ctx.Err(); err != nil {
			return resolution{}, err
		}
		now, err := currentInstant()
		if err != nil {
			return resolution{}, err
		}
		if !in.Active || !alive(in.Source.PreferredUntil, now.boot) || !alive(in.Source.ValidUntil, now.boot) {
			return resolution{}, errors.New("AFTR source expired during resolution")
		}
		if !alive(candidate.ValidUntil, now.boot) {
			failures = append(failures, fmt.Sprintf("candidate %d expired", candidateIndex))
			continue
		}
		if _, err := netip.ParseAddr(candidate.Value); err == nil {
			return resolution{candidate: candidateIndex, resolver: -1, warning: fallbackWarning(failures)}, nil
		}
		if len(in.Resolvers) == 0 {
			failures = append(failures, fmt.Sprintf("candidate %d has no explicit live IPv6 resolver", candidateIndex))
		}
		for resolverIndex, server := range in.Resolvers {
			if err := ctx.Err(); err != nil {
				return resolution{}, err
			}
			now, err = currentInstant()
			if err != nil {
				return resolution{}, err
			}
			until := minimum(in.Source.PreferredUntil, in.Source.ValidUntil, candidate.ValidUntil, server.ValidUntil)
			if !alive(until, now.boot) {
				failures = append(failures, fmt.Sprintf("candidate %d resolver %d dependency expired", candidateIndex, resolverIndex))
				continue
			}
			budget := timeout(c)
			if !until.IsZero() && until.Sub(now.boot) < budget {
				budget = until.Sub(now.boot)
			}
			attempt, cancel := context.WithTimeout(ctx, budget)
			answers, err := lookupAAAA(attempt, c, in.Source.Address, server, candidate.Value)
			cancel()
			if err == nil {
				now, err = currentInstant()
				if err != nil {
					return resolution{}, err
				}
				if _, err = chooseAnswer(c, liveAnswers(answers, now.boot)); err == nil {
					return resolution{candidate: candidateIndex, resolver: resolverIndex, answers: answers, warning: fallbackWarning(failures)}, nil
				}
			}
			failures = append(failures, fmt.Sprintf("candidate %d resolver %d: %v", candidateIndex, resolverIndex, err))
		}
	}
	if len(failures) == 0 {
		return resolution{}, errors.New("no live AFTR candidate")
	}
	return resolution{}, errors.New(errorText(errors.New(strings.Join(failures, "; "))))
}

func fallbackWarning(failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	return errorText(fmt.Errorf("AFTR fallback used after %s", strings.Join(failures, "; ")))
}

type chainState struct {
	names map[string]bool
	links int
	until time.Time
}

func fqdn(value string) string { return dns.Fqdn(strings.ToLower(value)) }

func lookupAAAA(ctx context.Context, c Config, local string, server Resolver, value string) ([]Answer, error) {
	name, chain := fqdn(value), chainState{names: map[string]bool{}}
	for queries := 0; queries <= MaxCNAMEs; queries++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request := new(dns.Msg)
		request.SetQuestion(name, dns.TypeAAAA)
		request.SetEdns0(1232, false)
		response, err := exchange(ctx, c, local, server, "udp6", request)
		if err != nil {
			return nil, err
		}
		if err := validateResponse(request, response); err != nil {
			return nil, err
		}
		if response.Truncated {
			response, err = exchange(ctx, c, local, server, "tcp6", request)
			if err != nil {
				return nil, err
			}
			if err := validateResponse(request, response); err != nil {
				return nil, err
			}
			if response.Truncated {
				return nil, errors.New("DNS TCP response remained truncated")
			}
		}
		if response.Rcode != dns.RcodeSuccess {
			return nil, fmt.Errorf("DNS returned %s", dns.RcodeToString[response.Rcode])
		}
		now, err := currentInstant()
		if err != nil {
			return nil, err
		}
		answers, follow, err := extractAnswers(response, name, &chain, now.boot)
		if err != nil {
			return nil, err
		}
		if len(answers) > 0 {
			return answers, nil
		}
		name = follow
	}
	return nil, errors.New("AFTR CNAME query depth exceeded")
}

// The connected UDP/TCP socket accepts replies from its literal endpoint only.
// The device bind prevents a learned resolver from taking an unrelated egress;
// no hostname, resolv.conf, NSS or local recursive-resolver path is consulted.
func exchange(ctx context.Context, c Config, local string, server Resolver, network string, request *dns.Msg) (*dns.Msg, error) {
	remote, err := netip.ParseAddr(server.Address)
	if err != nil {
		return nil, err
	}
	if remote.IsLinkLocalUnicast() {
		remote = remote.WithZone(server.Interface)
	}
	dialer := net.Dialer{ControlContext: func(_ context.Context, _, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, c.Interface)
		})
		return errors.Join(err, inner)
	}}
	if !remote.IsLinkLocalUnicast() && local != "" {
		ip := net.ParseIP(local)
		if network == "udp6" {
			dialer.LocalAddr = &net.UDPAddr{IP: ip}
		} else {
			dialer.LocalAddr = &net.TCPAddr{IP: ip}
		}
	}
	conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(remote.String(), strconv.Itoa(int(resolverPort(server)))))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	client := dns.Client{Net: network, Timeout: timeout(c), UDPSize: 1232}
	response, _, err := client.ExchangeWithConnContext(ctx, request, &dns.Conn{Conn: conn, UDPSize: 1232})
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return response, err
}

func validateResponse(request, response *dns.Msg) error {
	if response == nil || !response.Response || response.Id != request.Id || response.Opcode != dns.OpcodeQuery || len(response.Question) != 1 || len(request.Question) != 1 {
		return errors.New("invalid DNS response identity/header")
	}
	a, b := request.Question[0], response.Question[0]
	if !strings.EqualFold(a.Name, b.Name) || a.Qtype != b.Qtype || a.Qclass != b.Qclass {
		return errors.New("DNS response question mismatch")
	}
	if len(response.Answer) > 256 {
		return errors.New("DNS answer record limit exceeded")
	}
	return nil
}

func extractAnswers(response *dns.Msg, query string, chain *chainState, now time.Time) ([]Answer, string, error) {
	name := fqdn(query)
	for {
		if chain.names[name] {
			return nil, "", errors.New("AFTR CNAME loop")
		}
		var target string
		var cnames []uint32
		var records []*dns.AAAA
		for _, rr := range response.Answer {
			if rr.Header().Class != dns.ClassINET || !strings.EqualFold(rr.Header().Name, name) {
				continue
			}
			switch value := rr.(type) {
			case *dns.CNAME:
				next := fqdn(value.Target)
				if !hostname(next) || target != "" && target != next {
					return nil, "", errors.New("invalid or conflicting AFTR CNAME target")
				}
				target = next
				cnames = append(cnames, value.Hdr.Ttl)
			case *dns.AAAA:
				records = append(records, value)
			}
		}
		if target != "" {
			if len(records) > 0 {
				return nil, "", errors.New("AFTR DNS owner has both CNAME and AAAA")
			}
			chain.names[name] = true
			chain.links++
			if chain.links > MaxCNAMEs {
				return nil, "", errors.New("AFTR CNAME depth exceeded")
			}
			for _, ttl := range cnames {
				if ttl == 0 || ttl > MaxDNSTTL {
					return nil, "", errors.New("zero/invalid CNAME TTL cannot authorize a persistent AFTR endpoint")
				}
				chain.until = minimum(chain.until, now.Add(time.Duration(ttl)*time.Second))
			}
			name = target
			continue
		}
		if len(records) == 0 {
			if name == fqdn(query) {
				return nil, "", errors.New("DNS response contains no reachable AAAA answer")
			}
			return nil, name, nil
		}
		answers := make([]Answer, 0, len(records))
		positions := map[string]int{}
		// RFC 2181 section 5.2 treats our explicitly selected resolvers as
		// authoritative for RRset TTL normalization. A longer-lived member
		// must never remain after the shorter TTL in the same set has expired.
		rrsetTTL := MaxDNSTTL
		for _, record := range records {
			if record.Hdr.Ttl == 0 || record.Hdr.Ttl > MaxDNSTTL {
				return nil, "", errors.New("zero/invalid AAAA TTL cannot authorize a persistent AFTR endpoint")
			}
			if record.Hdr.Ttl < rrsetTTL {
				rrsetTTL = record.Hdr.Ttl
			}
		}
		for _, record := range records {
			a, ok := netip.AddrFromSlice(record.AAAA)
			if !ok || !global(a.String()) {
				return nil, "", errors.New("AFTR AAAA is not an eligible global IPv6 endpoint")
			}
			value := Answer{Address: a.String(), TTL: rrsetTTL, ReceivedAt: now, ValidUntil: minimum(chain.until, now.Add(time.Duration(rrsetTTL)*time.Second))}
			if i, ok := positions[value.Address]; ok {
				if value.ValidUntil.Before(answers[i].ValidUntil) {
					answers[i] = value
				}
				continue
			}
			positions[value.Address] = len(answers)
			answers = append(answers, value)
			if len(answers) > MaxAnswers {
				return nil, "", errors.New("AFTR AAAA answer limit exceeded")
			}
		}
		return answers, "", nil
	}
}
