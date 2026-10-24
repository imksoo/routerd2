//go:build linux

package bfd

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestProcessClockLoadsImmutableBootIdentityOnceAcrossConcurrentReads(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	var loads atomic.Int32
	reader := processClockReader{load: func() (string, error) { loads.Add(1); return id, nil }}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				clock, err := reader.Read()
				if err != nil || clock.BootID != id || clock.BoottimeNanoseconds <= 0 {
					t.Error(clock, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatal("clock hot path repeated boot identity I/O", loads.Load())
	}
}

func TestProcessClockRetriesInitialFailureWithoutCachingIt(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	failure := errors.New("initial boot identity read failed")
	for _, invalid := range []bool{false, true} {
		loads := 0
		reader := processClockReader{load: func() (string, error) {
			loads++
			if loads == 1 {
				if invalid {
					return "bad", nil
				}
				return "", failure
			}
			return id, nil
		}}
		if _, err := reader.Read(); err == nil {
			t.Fatal("failed boot identity was accepted")
		}
		for range 3 {
			clock, err := reader.Read()
			if err != nil || clock.BootID != id {
				t.Fatal("clock did not recover after initial failure", clock, err)
			}
		}
		if loads != 2 {
			t.Fatal("successful identity was not retained", loads)
		}
	}
}
