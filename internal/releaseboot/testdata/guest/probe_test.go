package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeUsesRemainingGlobalBudgetForVerifiedRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := observeWithBudgets(ctx, 10*time.Millisecond, func(call context.Context) error {
		deadline, ok := call.Deadline()
		if !ok || time.Until(deadline) > 20*time.Millisecond {
			t.Fatal("socket check lost its short bound")
		}
		return nil
	}, func(call context.Context) error {
		// ELF re-verification can legitimately take longer than a Unix RPC.
		want, _ := ctx.Deadline()
		got, ok := call.Deadline()
		if !ok || !got.Equal(want) {
			return errors.New("runtime inspection inherited the short socket deadline")
		}
		timer := time.NewTimer(30 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-call.Done():
			return call.Err()
		}
	})
	if err != nil {
		t.Fatal("short socket budget incorrectly canceled runtime inspection", err)
	}
}

func TestProbeKeepsSocketTimeoutAndGlobalDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called := false
	err := observeWithBudgets(ctx, 5*time.Millisecond, func(call context.Context) error { <-call.Done(); return call.Err() }, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called || !strings.Contains(err.Error(), "worker observation") || ctx.Err() != nil {
		t.Fatal("socket deadline or stage diagnostic lost", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer stop()
	err = observeWithBudgets(ctx, time.Second, func(context.Context) error { return nil }, func(call context.Context) error { <-call.Done(); return call.Err() })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "verified runtime observation") {
		t.Fatal("runtime escaped global deadline", err)
	}
}
