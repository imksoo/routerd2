package control

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/supervisor"
)

func TestRecoveryOwnershipWaitDoesNotBlockControlAuthority(t *testing.T) {
	for _, sourceBusy := range []bool{false, true} {
		t.Run(map[bool]string{false: "available_source", true: "busy_source"}[sourceBusy], func(t *testing.T) {
			e, network := fixture(t)
			journal := attachJournal(t, e, 0)
			e.protocolRelays = newProtocolRelays(t, journal, e.store.Dir)
			if err := e.protocolRelays.Register(context.Background(), []string{"wan"}); err != nil {
				t.Fatal(err)
			}
			dir, err := os.MkdirTemp("", "rd-recovery-") // keep AF_UNIX path below 108 bytes
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready, entered, release := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
			var block atomic.Bool
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/status" && block.Load() {
					select {
					case entered <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				clock, err := boottime.Read()
				if err != nil {
					localapi.Fail(w, 500, err)
					return
				}
				localapi.JSON(w, 200, supervisor.Status{Lifecycle: supervisor.Running, Ready: true, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(5*time.Second)})
			})
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- localapi.Serve(ctx, filepath.Join(dir, supervisor.SocketName), handler, func() { close(ready) })
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			e.runtime = runtimeplan.New(dir, e.store.Dir, network, "11111111111111111111111111111111")
			if err := e.runtime.ApplyConfig(ctx, e.state.Revision, e.state.Config); err != nil {
				t.Fatal(err)
			}
			block.Store(true)
			done := make(chan error, 1)
			go func() { done <- e.ProcessRetiredProtocolEvents(ctx) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			owned := e.mu.TryLock()
			if !owned {
				t.Error("ownership wait holds control authority and blocks current history publication")
			} else if !sourceBusy {
				e.mu.Unlock()
			}
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-ctx.Done():
				t.Error("recovery inverted source/reconciliation lock order")
			}
			if owned && sourceBusy {
				e.mu.Unlock()
			}
			// A busy source defers this pass without forgetting ownership; the
			// next pass must complete after the source lock becomes available.
			if sourceBusy && owned && e.protocolRelays.Status().Registered != 1 {
				t.Error("recovery mutated a busy source's registry")
			}
			if err := e.ProcessRetiredProtocolEvents(ctx); err != nil || e.protocolRelays.Status().Registered != 0 {
				t.Error("recovery did not resume", e.protocolRelays.Status(), err)
			}
			cancel()
			if err := <-serverDone; err != nil {
				t.Error(err)
			}
		})
	}
}
