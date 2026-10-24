package releaseboot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/release"
)

func TestContinuousHealthWindowAndBusyConfirmation(t *testing.T) {
	t.Parallel()
	checks, confirms, failures := 0, 0, 0
	start := time.Now()
	err := watch(context.Background(), 4*time.Second, 550*time.Millisecond,
		func(context.Context) error {
			checks++
			if checks == 2 {
				return errors.New("worker generation changed")
			}
			return nil
		},
		func(context.Context) error {
			confirms++
			if confirms == 1 {
				return release.ErrBusy
			}
			return nil
		},
		func(context.Context) error { failures++; return nil })
	if err != nil || confirms != 2 || failures != 0 || checks < 7 || time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("unstable or busy trial incorrectly confirmed: checks=%d confirms=%d failures=%d elapsed=%s err=%v", checks, confirms, failures, time.Since(start), err)
	}
}

func TestDeadlineCannotConfirmAfterSlowHealthRead(t *testing.T) {
	t.Parallel()
	confirmed, failed := false, false
	err := watch(context.Background(), 400*time.Millisecond, time.Nanosecond,
		func(ctx context.Context) error {
			select {
			case <-time.After(250 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		func(context.Context) error { confirmed = true; return nil },
		func(context.Context) error { failed = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "deadline") || confirmed || !failed {
		t.Fatalf("deadline bypass: confirmed=%t failed=%t err=%v", confirmed, failed, err)
	}
}

func TestShutdownDoesNotConfirmOrDeclareFailedBoot(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	callback := func(context.Context) error { called = true; return nil }
	if err := watch(ctx, time.Second, time.Millisecond, callback, callback, callback); err != nil || called {
		t.Fatal("shutdown changed boot selection", called, err)
	}
}

func TestStaleConfirmationIsNotRetriedAsCurrentBoot(t *testing.T) {
	t.Parallel()
	confirmed := 0
	err := watch(context.Background(), 2*time.Second, time.Nanosecond,
		func(context.Context) error { return nil },
		func(context.Context) error { confirmed++; return release.ErrStale },
		func(context.Context) error { t.Fatal("failure callback must not target an unrelated boot"); return nil })
	if !errors.Is(err, release.ErrStale) || confirmed != 1 {
		t.Fatal("stale attempt retried", confirmed, err)
	}
}

func TestConfirmationMayVerifyPayloadsBeyondSocketReadTimeout(t *testing.T) {
	t.Parallel()
	err := watch(context.Background(), 4*time.Second, time.Nanosecond,
		func(context.Context) error { return nil },
		func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= time.Second {
				t.Error("confirmation was not given the remaining trial budget")
			}
			select {
			case <-time.After(1100 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		func(context.Context) error { t.Error("valid slow verification failed the boot"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfirmationCannotOutliveTrialBudget(t *testing.T) {
	t.Parallel()
	failed := false
	err := watch(context.Background(), time.Second, time.Nanosecond,
		func(context.Context) error { return nil },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		func(context.Context) error { failed = true; return nil })
	if err == nil || !failed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overdue verification did not fail the trial: failed=%t err=%v", failed, err)
	}
}

func TestShutdownDuringConfirmationPreservesUnconfirmedTrial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := watch(ctx, 4*time.Second, time.Nanosecond,
		func(context.Context) error { return nil },
		func(call context.Context) error { cancel(); <-call.Done(); return call.Err() },
		func(context.Context) error { t.Error("ordinary shutdown declared a failed boot"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}
