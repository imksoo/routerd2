// SPDX-License-Identifier: BSD-3-Clause

package bfd

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func goldenPacket(t *testing.T) []byte {
	t.Helper()
	data, err := hex.DecodeString("20c003181234567890abcdef000493e00007a12000000000")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestControlWireGoldenAndPadding(t *testing.T) {
	data := goldenPacket(t)
	p, err := parseControl(append(append([]byte(nil), data...), 0xde, 0xad))
	if err != nil {
		t.Fatal(err)
	}
	if p.state != wireUp || p.multiplier != 3 || p.my != 0x12345678 || p.your != 0x90abcdef || p.minTx != 300000 || p.minRx != 500000 || p.minEchoRx != 0 || p.flags != 0 || p.diagnostic != 0 {
		t.Fatalf("unexpected parsed fields: %+v", p)
	}
	if !bytes.Equal(p.bytes(), data) {
		t.Fatalf("independent wire vector differs: %x", p.bytes())
	}
	data[0], data[1] = 0x3f, 0xc8 // Reserved diagnostic and C flag are not modes.
	data[20], data[21], data[22], data[23] = 0x7f, 0xff, 0xff, 0xff
	if p, err := parseControl(data); err != nil || p.diagnostic != 31 || p.minEchoRx != 0x7fffffff {
		t.Fatalf("remote echo capability/C bit/reserved diagnostic rejected: %+v %v", p, err)
	}
}

func TestRejectUnsupportedAndMalformedControl(t *testing.T) {
	tests := map[string]func([]byte) []byte{
		"truncated":                func(b []byte) []byte { return b[:23] },
		"version zero":             func(b []byte) []byte { b[0] = 0; return b },
		"version two":              func(b []byte) []byte { b[0] = 0x40; return b },
		"length short":             func(b []byte) []byte { b[3] = 23; return b },
		"length exceeds datagram":  func(b []byte) []byte { b[3] = 25; return b },
		"unknown extension":        func(b []byte) []byte { b[3] = 25; return append(b, 0) },
		"authentication":           func(b []byte) []byte { b[1] |= 4; return b },
		"demand":                   func(b []byte) []byte { b[1] |= 2; return b },
		"multipoint":               func(b []byte) []byte { b[1] |= 1; return b },
		"poll and final":           func(b []byte) []byte { b[1] |= 0x30; return b },
		"zero multiplier":          func(b []byte) []byte { b[2] = 0; return b },
		"zero local discriminator": func(b []byte) []byte { clear(b[4:8]); return b },
		"zero desired tx":          func(b []byte) []byte { clear(b[12:16]); return b },
		"zero your in up":          func(b []byte) []byte { clear(b[8:12]); return b },
		"zero your in init":        func(b []byte) []byte { clear(b[8:12]); b[1] = 0x80; return b },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			if p, err := parseControl(mutate(goldenPacket(t))); err == nil {
				t.Fatalf("invalid packet accepted: %+v", p)
			}
		})
	}
	for _, state := range []byte{0, 0x40} {
		b := goldenPacket(t)
		b[1] = state
		clear(b[8:12])
		clear(b[16:20]) // RequiredMinRx zero suppresses periodic TX, valid on wire.
		if _, err := parseControl(b); err != nil {
			t.Fatal(err)
		}
	}
}

func FuzzControl(f *testing.F) {
	f.Add([]byte{0x20, 0x40, 3, 24, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0x0f, 0x42, 0x40, 0, 0x04, 0x93, 0xe0, 0, 0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := parseControl(data)
		if err != nil {
			return
		}
		roundTrip, err := parseControl(p.bytes())
		if err != nil || roundTrip != p || len(p.bytes()) != 24 {
			t.Fatalf("unstable accepted control: %+v %+v %v", p, roundTrip, err)
		}
	})
}
