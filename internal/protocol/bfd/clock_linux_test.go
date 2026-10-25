//go:build linux

package bfd

import (
	"github.com/imksoo/routerd2/internal/boottime"
	"testing"
)

func TestBFDClockUsesSharedImmutableProcessBootIdentity(t *testing.T) {
	first, err := ReadClock()
	if err != nil {
		t.Fatal(err)
	}
	second, err := boottime.Read()
	if err != nil || first.BootID != second.BootID || first.BoottimeNanoseconds > second.BoottimeNanoseconds {
		t.Fatal(first, second, err)
	}
}
