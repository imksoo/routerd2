// SPDX-License-Identifier: BSD-3-Clause

package aftr

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func dnsReply(t *testing.T, records ...string) *dns.Msg {
	t.Helper()
	question := new(dns.Msg)
	question.SetQuestion("aftr.example.", dns.TypeAAAA)
	m := new(dns.Msg)
	m.SetReply(question)
	for _, text := range records {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		m.Answer = append(m.Answer, rr)
	}
	return m
}
func chainForTest() *chainState { return &chainState{names: map[string]bool{}} }

func TestCNAMEAndAAAAExpiryAndAnswerOwnership(t *testing.T) {
	now := clockForTest(0).boot
	m := dnsReply(t, "aftr.example. 20 IN CNAME target.example.", "target.example. 60 IN AAAA 2001:db8::2", "target.example. 60 IN AAAA 2001:db8::1", "unrelated.example. 3600 IN AAAA 2001:db8::bad")
	answers, follow, err := extractAnswers(m, "aftr.example.", chainForTest(), now)
	if err != nil || follow != "" || len(answers) != 2 || answers[0].Address != "2001:db8::2" || answers[1].Address != "2001:db8::1" {
		t.Fatalf("wrong reachable RR order/set: %+v %q %v", answers, follow, err)
	}
	for _, a := range answers {
		if a.TTL != 60 || !a.ValidUntil.Equal(now.Add(20*time.Second)) {
			t.Fatal("CNAME expiry was ignored or AAAA order lost")
		}
	}
	chain := chainForTest()
	first := dnsReply(t, "aftr.example. 20 IN CNAME target.example.")
	_, follow, err = extractAnswers(first, "aftr.example.", chain, now)
	if err != nil || follow != "target.example." {
		t.Fatal("CNAME-only reply did not request its target")
	}
	second := dnsReply(t, "target.example. 60 IN AAAA 2001:db8::3")
	answers, _, err = extractAnswers(second, follow, chain, now.Add(10*time.Second))
	if err != nil || len(answers) != 1 || !answers[0].ValidUntil.Equal(now.Add(20*time.Second)) {
		t.Fatal("second query reset the original CNAME TTL")
	}
}

func TestMalformedDNSChainsCannotBecomePersistentEndpoints(t *testing.T) {
	for name, records := range map[string][]string{
		"unrelated-only":  {"unrelated.example. 60 IN AAAA 2001:db8::1"},
		"loop":            {"aftr.example. 60 IN CNAME target.example.", "target.example. 60 IN CNAME aftr.example."},
		"two-targets":     {"aftr.example. 60 IN CNAME a.example.", "aftr.example. 60 IN CNAME b.example."},
		"CNAME-and-AAAA":  {"aftr.example. 60 IN CNAME a.example.", "aftr.example. 60 IN AAAA 2001:db8::1"},
		"CNAME-zero":      {"aftr.example. 0 IN CNAME target.example.", "target.example. 60 IN AAAA 2001:db8::1"},
		"AAAA-zero":       {"aftr.example. 0 IN AAAA 2001:db8::1"},
		"AAAA-high-bit":   {"aftr.example. 2147483648 IN AAAA 2001:db8::1"},
		"link-local-aftr": {"aftr.example. 60 IN AAAA fe80::1"},
		"multicast-aftr":  {"aftr.example. 60 IN AAAA ff02::1"},
		"mapped-aftr":     {"aftr.example. 60 IN AAAA ::ffff:192.0.2.1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := extractAnswers(dnsReply(t, records...), "aftr.example.", chainForTest(), clockForTest(0).boot); err == nil {
				t.Fatal("invalid DNS chain accepted")
			}
		})
	}
	var deep []string
	name := "aftr.example."
	for i := 0; i <= MaxCNAMEs; i++ {
		target := fmt.Sprintf("n%d.example.", i)
		deep = append(deep, fmt.Sprintf("%s 60 IN CNAME %s", name, target))
		name = target
	}
	deep = append(deep, fmt.Sprintf("%s 60 IN AAAA 2001:db8::1", name))
	if _, _, err := extractAnswers(dnsReply(t, deep...), "aftr.example.", chainForTest(), clockForTest(0).boot); err == nil {
		t.Fatal("CNAME depth limit missing")
	}
}

func TestDNSIdentityAndAnswerLimit(t *testing.T) {
	request := new(dns.Msg)
	request.SetQuestion("aftr.example.", dns.TypeAAAA)
	base := new(dns.Msg)
	base.SetReply(request)
	for _, change := range []func(*dns.Msg){
		func(m *dns.Msg) { m.Id++ }, func(m *dns.Msg) { m.Question[0].Name = "forged.example." }, func(m *dns.Msg) { m.Question[0].Qtype = dns.TypeA }, func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS }, func(m *dns.Msg) { m.Response = false }, func(m *dns.Msg) { m.Question = nil },
	} {
		m := base.Copy()
		change(m)
		if validateResponse(request, m) == nil {
			t.Fatal("DNS identity mismatch accepted")
		}
	}
	m := base.Copy()
	for i := 0; i <= MaxAnswers; i++ {
		m.Answer = append(m.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: "aftr.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: net.ParseIP(fmt.Sprintf("2001:db8::%x", i+1))})
	}
	if _, _, err := extractAnswers(m, "aftr.example.", chainForTest(), clockForTest(0).boot); err == nil {
		t.Fatal("oversized AFTR answer set was truncated silently")
	}
}

func TestDuplicateAAAAUsesShortestLifetime(t *testing.T) {
	m := dnsReply(t, "aftr.example. 60 IN AAAA 2001:db8::1", "aftr.example. 10 IN AAAA 2001:db8::1")
	answers, _, err := extractAnswers(m, "aftr.example.", chainForTest(), clockForTest(0).boot)
	if err != nil || len(answers) != 1 || answers[0].TTL != 10 || !answers[0].ValidUntil.Equal(clockForTest(10).boot) {
		t.Fatal("duplicate answer lifetime was extended")
	}
}

func TestAAAASetExpiresAtItsShortestMemberTTL(t *testing.T) {
	m := dnsReply(t, "aftr.example. 60 IN AAAA 2001:db8::2", "aftr.example. 10 IN AAAA 2001:db8::1")
	answers, _, err := extractAnswers(m, "aftr.example.", chainForTest(), clockForTest(0).boot)
	if err != nil || len(answers) != 2 || answers[0].Address != "2001:db8::2" || answers[1].Address != "2001:db8::1" {
		t.Fatalf("RRset normalization changed answer identity/order: %+v %v", answers, err)
	}
	for _, answer := range answers {
		if answer.TTL != 10 || !answer.ValidUntil.Equal(clockForTest(10).boot) {
			t.Fatal("partial RRset could outlive its shortest TTL")
		}
	}
}

func FuzzAFTRDNSAnswerScope(f *testing.F) {
	for _, value := range []string{"2001:db8::1", "::1", "fe80::1", "ff02::1"} {
		m := new(dns.Msg)
		m.SetQuestion("aftr.example.", dns.TypeAAAA)
		m.Response = true
		m.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: "aftr.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60}, AAAA: net.ParseIP(value)}}
		b, _ := m.Pack()
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65535 {
			return
		}
		m := new(dns.Msg)
		if m.Unpack(b) != nil || len(m.Answer) > 256 {
			return
		}
		answers, _, err := extractAnswers(m, "aftr.example.", chainForTest(), clockForTest(0).boot)
		if err != nil {
			return
		}
		if len(answers) > MaxAnswers {
			t.Fatal("unbounded answers")
		}
		for _, a := range answers {
			if !global(a.Address) || a.TTL == 0 || a.TTL > MaxDNSTTL || !strings.Contains(a.Address, ":") {
				t.Fatal("invalid accepted AFTR answer")
			}
		}
	})
}
