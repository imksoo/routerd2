// Package releaseboot confirms a signed userland trial only after the actual
// supervised process tree and committed configuration remain ready together.
package releaseboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"github.com/imksoo/routerd2/internal/configref"
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
	Startup    *configref.Selection
	RuntimeDir string
	Timeout    time.Duration
	Stable     time.Duration
}

var errTrialHealthChanged = errors.New("trial health changed during confirmation")

type healthWitness struct {
	key           string
	configuration *release.ConfigurationConfirmation
}

func (t Trial) witness(ctx context.Context) (healthWitness, error) {
	var processes supervisor.Status
	if err := localapi.NewClient(filepath.Join(t.RuntimeDir, supervisor.SocketName)).Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
		return healthWitness{}, err
	}
	if !processes.Ready || processes.Lifecycle != supervisor.Running || processes.PID != 1 || processes.Platform != "appliance" || processes.ReleaseID != t.Release.ReleaseID {
		return healthWitness{}, errors.New("selected appliance supervisor is not ready")
	}
	for _, module := range processes.Modules {
		if module.Required && (!module.Ready || module.PID <= 1 || module.ReleaseID != t.Release.ReleaseID) {
			return healthWitness{}, fmt.Errorf("required module %s is not ready for selected release", module.Name)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://routerd/v1/health", nil)
	if err != nil {
		return healthWitness{}, err
	}
	response, err := localapi.NewClient(filepath.Join(t.RuntimeDir, control.SocketName)).HTTP.Do(request)
	if err != nil {
		return healthWitness{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
		return healthWitness{}, &localapi.HTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, localapi.MaxBody+1))
	if err != nil {
		return healthWitness{}, err
	}
	if len(body) > localapi.MaxBody {
		return healthWitness{}, errors.New("release health response exceeds size limit")
	}
	var state control.Health
	if err := json.Unmarshal(body, &state); err != nil {
		return healthWitness{}, err
	}
	if state.Services == nil {
		return healthWitness{}, errors.New("committed routing projection is unavailable")
	}
	if response.StatusCode != http.StatusOK || !state.Healthy || state.Revision == 0 || state.Confirmation != nil || state.Services.Phase != "ready" || state.Services.Revision != state.Revision || state.Services.RouteIssueCount != 0 {
		return healthWitness{}, fmt.Errorf("committed routing configuration is not ready: healthy=%t revision=%d phase=%s service_revision=%d route_issues=%d confirmation=%t error=%s services=%s journal=%s storage=%s rules=%s derived=%s protocol=%s", state.Healthy, state.Revision, state.Services.Phase, state.Services.Revision, state.Services.RouteIssueCount, state.Confirmation != nil, state.LastError, state.Services.LastError, state.EventJournal.LastError, state.EventJournal.Storage.LastError, state.EventRules.LastError, state.Derived.LastError, state.ProtocolEvents.LastError)
	}
	var proof *release.ConfigurationConfirmation
	if t.Startup != nil {
		if state.Startup == nil || !state.Startup.Selection.Equal(*t.Startup) || state.Startup.Recovered || state.Startup.Validate(state.StoreID, state.Revision) != nil {
			return healthWitness{}, errors.New("committed configuration does not match the selected runtime/configuration pair")
		}
		proof = &release.ConfigurationConfirmation{Adoption: *state.Startup, Revision: state.Revision}
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
	return healthWitness{key: string(b), configuration: proof}, nil
}

// Watch uses elapsed monotonic time. Context cancellation is ordinary shutdown,
// leaving a trial unconfirmed for the next boot's bounded retry policy.
func (t Trial) Watch(ctx context.Context) error {
	if t.Store == nil || t.Timeout <= 0 || t.Stable <= 0 || t.Stable >= t.Timeout {
		return errors.New("invalid release health trial")
	}
	slog.Info("runtime update trial started", "release", t.Release.ReleaseID, "timeout_seconds", t.Timeout.Seconds(), "stable_seconds", t.Stable.Seconds())
	previous := ""
	check := func(ctx context.Context) error {
		current, err := t.witness(ctx)
		if err != nil {
			previous = ""
			return err
		}
		changed := previous != "" && current.key != previous
		previous = current.key
		if changed {
			return errors.New("required process generation or committed configuration changed")
		}
		return nil
	}
	return watch(ctx, t.Timeout, t.Stable, check, func(ctx context.Context) error {
		var err error
		if t.Startup != nil {
			_, err = t.Store.ConfirmConfiguration(ctx, t.BootID, t.Token, t.Release.Digest, func(call context.Context) (release.ConfigurationConfirmation, error) {
				current, err := t.witness(call)
				if err != nil {
					return release.ConfigurationConfirmation{}, errors.Join(errTrialHealthChanged, err)
				}
				if current.key != previous {
					return release.ConfigurationConfirmation{}, errTrialHealthChanged
				}
				return *current.configuration, nil
			})
		} else {
			_, err = t.Store.Confirm(ctx, t.BootID, t.Token, t.Release.Digest)
		}
		if errors.Is(err, release.ErrUncertain) {
			confirmed := false
			readErr := t.Store.WithState(ctx, func(state release.State) error {
				confirmed = state.Active != nil && state.Active.Digest == t.Release.Digest && state.Attempt == nil && state.Pending == nil && state.Operation == "confirm"
				return nil
			})
			if readErr == nil && confirmed {
				return nil
			}
			return errors.Join(err, readErr)
		}
		if err == nil {
			slog.Info("runtime update confirmed", "release", t.Release.ReleaseID, "digest", t.Release.Digest)
		}
		return err
	}, func(ctx context.Context) error { return t.Fail(ctx, "release readiness deadline exceeded") })
}

func (t Trial) Fail(ctx context.Context, reason string) error {
	// Recording failure changes only selection. The next BeginBoot qualifies
	// executable bytes before fallback execution. Bound this journal cleanup.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := t.Store.FailBoot(ctx, t.BootID, t.Token, reason)
	if errors.Is(err, release.ErrUncertain) {
		failed := false
		readErr := t.Store.WithState(ctx, func(state release.State) error {
			failed = state.LastFailure != nil && state.LastFailure.BootID == t.BootID && state.LastFailure.Release.Digest == t.Release.Digest && state.Attempt == nil && state.Pending == nil
			return nil
		})
		if readErr == nil && failed {
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
				if last == nil || last.Error() != err.Error() {
					slog.Info("runtime update trial waiting", "error", err)
				}
				healthySince = time.Time{}
				last = err
				continue
			}
			if healthySince.IsZero() {
				slog.Info("runtime update trial readiness observed")
				healthySince = time.Now()
			}
			if !time.Now().Before(deadlineAt) {
				continue
			}
			if time.Since(healthySince) < stable {
				continue
			}
			// Confirmation rechecks the running pair and publishes its journal.
			// Give that transaction the remaining trial budget.
			call, cancel = context.WithDeadline(ctx, deadlineAt)
			slog.Info("runtime update trial confirming")
			confirmationStarted := time.Now()
			err = confirm(call)
			cancel()
			if err != nil {
				slog.Info("runtime update trial confirmation deferred", "seconds", time.Since(confirmationStarted).Seconds(), "error", err)
			}
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
			if errors.Is(err, errTrialHealthChanged) {
				healthySince = time.Time{}
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
