//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestCompletedWitnessKeepsFullIdentityThroughBothCuts(t *testing.T) {
	first := []completedObservation{{Kind: 1, Name: "producer", Key: "output", Position: 1, SHA256: strings.Repeat("a", 64)}}
	second := append(append([]completedObservation(nil), first...), completedObservation{Kind: 0, Name: "consumer", Key: "effect", Position: 7, SHA256: strings.Repeat("b", 64)})
	if err := verifyCompleted(first, second); err != nil {
		t.Fatal(err)
	}
	if err := verifyCompleted(second, second); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]completedObservation) []completedObservation{
		"missing":          func(v []completedObservation) []completedObservation { return v[1:] },
		"digest":           func(v []completedObservation) []completedObservation { v[0].SHA256 = strings.Repeat("c", 64); return v },
		"position":         func(v []completedObservation) []completedObservation { v[0].Position++; return v },
		"kind":             func(v []completedObservation) []completedObservation { v[0].Kind = 0; return v },
		"name":             func(v []completedObservation) []completedObservation { v[0].Name = "other"; return v },
		"key":              func(v []completedObservation) []completedObservation { v[0].Key = "other"; return v },
		"duplicate":        func(v []completedObservation) []completedObservation { return append(v, v[0]) },
		"zero":             func(v []completedObservation) []completedObservation { v[0].Position = 0; return v },
		"malformed digest": func(v []completedObservation) []completedObservation { v[0].SHA256 = "invalid"; return v },
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyCompleted(second, mutate(append([]completedObservation(nil), second...))); err == nil {
				t.Fatal("accepted changed completed identity")
			}
		})
	}
}
