// Package releaseboot confirms a signed userland trial only after the actual
// supervised process tree and committed configuration remain ready together.
package releaseboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type Trial struct {
	Store      *release.Store
	BootID     string
	Token      string
	Release    release.Ref
	RuntimeDir string
	Timeout    time.Duration
	Stable     time.Duration
}

func (t Trial) witness(ctx context.Context) (string, error) {
	var processes supervisor.Status
	if err := localapi.NewClient(filepath.Join(t.RuntimeDir, supervisor.SocketName)).Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
		return "", err
	}
	if !processes.Ready || processes.Lifecycle != supervisor.Running || processes.PID != 1 || processes.Platform != "appliance" || processes.ReleaseID != t.Release.ReleaseID {
		return "", errors.New("selected appliance supervisor is not ready")
	}
	for _, module := range processes.Modules {
		if module.Required && (!module.Ready || module.PID <= 1 || module.ReleaseID != t.Release.ReleaseID) {
			return "", fmt.Errorf("required module %s is not ready for selected release", module.Name)
		}
	}
	var state control.State
	if err := localapi.NewClient(filepath.Join(t.RuntimeDir, control.SocketName)).Do(ctx, "GET", "/v1/health", nil, &state); err != nil {
		return "", err
	}
	if !state.Healthy || state.Revision == 0 || state.Confirmation != nil || state.Services == nil || state.Services.Phase != "ready" || state.Services.Revision != state.Revision || state.Services.RouteIssueCount != 0 {
		return "", errors.New("committed routing configuration is not ready for confirmation")
	}
	// A restart or configuration change resets the continuous health window,
	// even if it happened between polls and the replacement is already ready.
	type process struct {
		Name       string
		PID        int
		Generation uint64
		Digest     string
	}
	var required []process
	for _, module := range processes.Modules {
		if module.Required {
			required = append(required, process{module.Name, module.PID, module.Generation, module.ConfigDigest})
		}
	}
	sort.Slice(required, func(i, j int) bool { return required[i].Name < required[j].Name })
	b, _ := json.Marshal(struct {
		Revision uint64
		Epoch    string
		Required []process
	}{state.Revision, state.ProducerEpoch, required})
	return string(b), nil
}

// Watch uses elapsed monotonic time. Context cancellation is ordinary shutdown,
// leaving a trial unconfirmed for the next boot's bounded retry policy.
func (t Trial) Watch(ctx context.Context) error {
	if t.Store == nil || t.Timeout <= 0 || t.Stable <= 0 || t.Stable >= t.Timeout {
		return errors.New("invalid release health trial")
	}
	previous := ""
	check := func(ctx context.Context) error {
		current, err := t.witness(ctx)
		if err != nil {
			previous = ""
			return err
		}
		changed := previous != "" && current != previous
		previous = current
		if changed {
			return errors.New("required process generation or committed configuration changed")
		}
		return nil
	}
	return watch(ctx, t.Timeout, t.Stable, check, func(ctx context.Context) error {
		_, err := t.Store.Confirm(ctx, t.BootID, t.Token, t.Release.Digest)
		if errors.Is(err, release.ErrUncertain) {
			state, readErr := t.Store.Status(ctx)
			if readErr == nil && state.Active != nil && state.Active.Digest == t.Release.Digest && state.Attempt == nil && state.Pending == nil && state.Operation == "confirm" {
				return nil
			}
			return errors.Join(err, readErr)
		}
		return err
	}, func(ctx context.Context) error { return t.Fail(ctx, "release readiness deadline exceeded") })
}

func (t Trial) Fail(ctx context.Context, reason string) error {
	// Failure selection verifies the stored executable pairs too. Bound this
	// best-effort recovery even when a startup caller supplied Background, but
	// allow slow appliance storage more than a short local-API request budget.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := t.Store.FailBoot(ctx, t.BootID, t.Token, reason)
	if errors.Is(err, release.ErrUncertain) {
		state, readErr := t.Store.Status(ctx)
		if readErr == nil && state.LastFailure != nil && state.LastFailure.BootID == t.BootID && state.LastFailure.Release.Digest == t.Release.Digest && state.Attempt == nil && state.Pending == nil {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}

func watch(ctx context.Context, timeout, stable time.Duration, check, confirm, fail func(context.Context) error) error {
	deadlineAt := time.Now().Add(timeout)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var healthySince time.Time
	var last error
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := fail(cleanup)
			cancel()
			return errors.Join(errors.New("release readiness deadline exceeded"), last, err)
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, time.Second)
			err := check(call)
			cancel()
			if err != nil {
				healthySince = time.Time{}
				last = err
				continue
			}
			if healthySince.IsZero() {
				healthySince = time.Now()
			}
			if !time.Now().Before(deadlineAt) {
				continue
			}
			if time.Since(healthySince) < stable {
				continue
			}
			// Confirm re-verifies the signed ELF payloads before writing its
			// durable journal. Give it the remaining trial budget, not the short
			// socket-read budget; this matters on small CPUs and slow storage.
			call, cancel = context.WithDeadline(ctx, deadlineAt)
			err = confirm(call)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				return nil
			}
			if !time.Now().Before(deadlineAt) {
				last = err
				continue
			}
			if errors.Is(err, release.ErrBusy) {
				last = err
				continue
			}
			return fmt.Errorf("confirm selected release: %w", err)
		}
	}
}
