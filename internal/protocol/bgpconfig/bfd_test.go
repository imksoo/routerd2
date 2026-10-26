package bgpconfig

import (
	"strings"
	"testing"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
)

func TestBFDReferenceDefaultsAndRejectedModes(t *testing.T) {
	c := validConfig()
	c.PeerBFD = map[string]BFDReference{"192.0.2.2": {Service: "uplink-health"}}
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	normalized := Normalize(c)
	if normalized.PeerBFD["192.0.2.2"].Address != "192.0.2.2" || c.PeerBFD["192.0.2.2"].Address != "" {
		t.Fatal("BFD normalization did not preserve caller/default address")
	}
	for name, mutate := range map[string]func(*Config){
		"unknown peer":   func(c *Config) { c.PeerBFD["192.0.2.99"] = BFDReference{Service: "uplink-health"} },
		"reference path": func(c *Config) { c.PeerBFD["192.0.2.2"] = BFDReference{Service: "../uplink"} },
		"family mismatch": func(c *Config) {
			c.PeerBFD["192.0.2.2"] = BFDReference{Service: "uplink-health", Address: "2001:db8::2"}
		},
		"multicast address": func(c *Config) { c.PeerBFD["192.0.2.2"] = BFDReference{Service: "uplink-health", Address: "224.0.0.1"} },
		"peer graceful restart": func(c *Config) {
			p := c.Runtime.Peers["192.0.2.2"]
			p.GracefulRestart = &bgpdaemon.AppliedGracefulRestart{Enabled: true}
			c.Runtime.Peers["192.0.2.2"] = p
		},
		"global graceful restart": func(c *Config) { c.Runtime.Global.GracefulRestart = &bgpdaemon.AppliedGracefulRestart{Enabled: true} },
		"old profile reference":   func(c *Config) { p := c.Runtime.Peers["192.0.2.2"]; p.BFD = "fast"; c.Runtime.Peers["192.0.2.2"] = p },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := Normalize(c)
			mutate(&candidate)
			if Validate(candidate) == nil {
				t.Fatal("ambiguous or unsupported BFD intent accepted")
			}
		})
	}
}

func projectionFixture() (Config, BFDProjection) {
	c := Normalize(validConfig())
	c.PeerBFD = map[string]BFDReference{"192.0.2.2": {Service: "health", Address: "192.0.2.2"}}
	g := BFDGrant{Service: "health", Address: "192.0.2.2", ConfigDigest: strings.Repeat("b", 64), Generation: 7, ProducerEpoch: strings.Repeat("c", 32), Sequence: 3, BootID: "01234567-89ab-cdef-0123-456789abcdef", LastReceivedBootNanoseconds: 1000000000, DeadlineBootNanoseconds: 1150000000, Up: true}
	return c, BFDProjection{ConfigDigest: strings.Repeat("a", 64), Generation: 9, Peers: map[string]BFDGrant{"192.0.2.2": g}}
}

func TestBFDProjectionBindingAndFiniteSourceValidation(t *testing.T) {
	c, original := projectionFixture()
	if err := ValidateBFDProjection(c, original, original.ConfigDigest, 9); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*BFDProjection){
		"recipient digest":     func(p *BFDProjection) { p.ConfigDigest = strings.Repeat("d", 64) },
		"recipient generation": func(p *BFDProjection) { p.Generation++ },
		"unconfigured peer":    func(p *BFDProjection) { p.Peers["192.0.2.99"] = p.Peers["192.0.2.2"] },
		"source scope":         func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.Service = "other"; p.Peers["192.0.2.2"] = g },
		"source address":       func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.Address = "192.0.2.3"; p.Peers["192.0.2.2"] = g },
		"source digest":        func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.ConfigDigest = ""; p.Peers["192.0.2.2"] = g },
		"source generation":    func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.Generation = 0; p.Peers["192.0.2.2"] = g },
		"source epoch":         func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.ProducerEpoch = ""; p.Peers["192.0.2.2"] = g },
		"source sequence":      func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.Sequence = 0; p.Peers["192.0.2.2"] = g },
		"boot scope":           func(p *BFDProjection) { g := p.Peers["192.0.2.2"]; g.BootID = "boot"; p.Peers["192.0.2.2"] = g },
		"unbounded deadline": func(p *BFDProjection) {
			g := p.Peers["192.0.2.2"]
			g.DeadlineBootNanoseconds = g.LastReceivedBootNanoseconds + MaxBFDDetectionNanoseconds + 1
			p.Peers["192.0.2.2"] = g
		},
		"no lease": func(p *BFDProjection) {
			g := p.Peers["192.0.2.2"]
			g.DeadlineBootNanoseconds = 0
			p.Peers["192.0.2.2"] = g
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, candidate := projectionFixture()
			mutate(&candidate)
			if ValidateBFDProjection(c, candidate, original.ConfigDigest, 9) == nil {
				t.Fatal("invalid BFD grant accepted")
			}
		})
	}
	_, missing := projectionFixture()
	missing.Peers = nil
	if err := ValidateBFDProjection(c, missing, original.ConfigDigest, 9); err != nil {
		t.Fatal("missing source must represent no health, not malformed input")
	}
	missing.Peers = map[string]BFDGrant{"192.0.2.2": {Service: "health", Address: "192.0.2.2"}}
	if err := ValidateBFDProjection(c, missing, original.ConfigDigest, 9); err != nil {
		t.Fatal("unobserved source Down grant rejected", err)
	}
}
