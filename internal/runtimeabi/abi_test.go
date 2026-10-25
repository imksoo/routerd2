// SPDX-License-Identifier: BSD-3-Clause

package runtimeabi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentRecordIsCanonicalBoundedAndCopied(t *testing.T) {
	d, err := ParseRecord(CurrentRecord)
	if err != nil || !d.Equal(Current()) {
		t.Fatal("invalid compiled ABI declaration", d, err)
	}
	record, err := EncodeRecord(d)
	if err != nil || record != CurrentRecord || len(record) > MaxRecordBytes {
		t.Fatal("noncanonical compiled ABI", err)
	}
	d.State[0].Format.Write = 60000
	if d.Equal(Current()) {
		t.Fatal("caller mutated shared ABI declaration")
	}
	for _, bad := range []string{
		"", Prefix + "END", Prefix + Suffix, CurrentRecord + " ", strings.Replace(CurrentRecord, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(CurrentRecord, `"version":1`, `"Version":1`, 1),
		strings.Replace(CurrentRecord, `"version":1`, `"version":1,"unknown":0`, 1),
		strings.Repeat(" ", MaxRecordBytes+1),
	} {
		if _, err := ParseRecord(bad); err == nil {
			t.Fatalf("accepted ambiguous ABI declaration: %.80s", bad)
		}
	}
	for _, change := range []func(*Descriptor){
		func(d *Descriptor) { d.Version = 2 }, func(d *Descriptor) { d.Profile = "../profile" },
		func(d *Descriptor) { d.Configuration.ReadMin = 0 }, func(d *Descriptor) { d.Configuration.Write = 3 },
		func(d *Descriptor) { d.State[1].Name = d.State[0].Name },
		func(d *Descriptor) { d.State[0], d.State[1] = d.State[1], d.State[0] },
		func(d *Descriptor) { d.State = d.State[:0] },
	} {
		x := Current()
		change(&x)
		if err := x.Validate(); err == nil {
			t.Fatal("accepted malformed ABI descriptor")
		}
	}
}

func currentFacts() Facts {
	d := Current()
	f := Facts{Configuration: 2, ConfigurationContract: d.ConfigurationContract}
	for _, s := range d.State {
		f.State = append(f.State, StateVersion{Name: s.Name, Version: s.Format.Write})
	}
	return f
}

func TestTransitionChecksObservedReadersAndBothWriters(t *testing.T) {
	if err := CheckTransition(Current(), Current(), currentFacts()); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Descriptor, *Descriptor, *Facts){
		func(_, target *Descriptor, _ *Facts) { target.ConfigurationContract = "other-contract" },
		func(_, target *Descriptor, _ *Facts) { target.Profile = "other-profile" },
		func(_, target *Descriptor, _ *Facts) { target.WorkerABI = 2 },
		func(_, target *Descriptor, _ *Facts) {
			target.State[0].Format = Format{ReadMin: 2, ReadMax: 2, Write: 2}
		},
		func(_, target *Descriptor, _ *Facts) { target.Configuration = Format{ReadMin: 2, ReadMax: 3, Write: 3} },
		func(_, _ *Descriptor, f *Facts) { f.State = f.State[1:] },
		func(_, _ *Descriptor, f *Facts) { f.State[1].Name = f.State[0].Name },
		func(_, _ *Descriptor, f *Facts) { f.ConfigurationContract = "forged-observation" },
	} {
		active, target, facts := Current(), Current(), currentFacts()
		change(&active, &target, &facts)
		if err := CheckTransition(active, target, facts); err == nil {
			t.Fatal("unsafe ABI transition accepted")
		}
	}
	active, target, facts := Current(), Current(), currentFacts()
	active.State[1].Format = Format{ReadMin: 5, ReadMax: 6, Write: 6}
	target.State[1].Format = Format{ReadMin: 5, ReadMax: 5, Write: 5}
	facts.State[1].Version = 5
	if err := CheckTransition(active, target, facts); err == nil {
		t.Fatal("target cannot read the running writer's next format")
	}
	facts = currentFacts()
	facts.State[len(facts.State)-1].Version = 0
	if err := CheckTransition(Current(), Current(), facts); err != nil {
		t.Fatal("explicitly absent compatible identity cannot be initialized", err)
	}
}

func TestRecordScannerHandlesChunkBoundariesAndAmbiguity(t *testing.T) {
	for _, offset := range []int{0, 1, 32760, 65534} {
		data := append(bytes.Repeat([]byte("x"), offset), []byte(CurrentRecord)...)
		got, err := ReadRecord(bytes.NewReader(data))
		if err != nil || !got.Equal(Current()) {
			t.Fatal("record across scan boundary", offset, err)
		}
	}
	for _, data := range []string{"no declaration", CurrentRecord + CurrentRecord} {
		if _, err := ReadRecord(strings.NewReader(data)); err == nil {
			t.Fatal("missing or multiple complete declarations accepted")
		}
	}
	raw, _ := json.Marshal(Current())
	if len(raw) > MaxRecordBytes {
		t.Fatal("unbounded current metadata")
	}
}
