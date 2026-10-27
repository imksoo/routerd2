// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocolevents"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/runtimeplan"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
	"golang.org/x/sys/unix"
)

func await(ctx context.Context, label string, observe func() error) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		if last = observe(); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w (last: %v)", label, ctx.Err(), last)
		case <-ticker.C:
		}
	}
}

func runProbe(phase string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	if phase == "produce" {
		listener, err := net.Listen("tcp4", "127.0.0.1:18089")
		if err != nil {
			return err
		}
		defer listener.Close()
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	client := localapi.NewClient(filepath.Join(runtimeDir, control.SocketName))
	manager := localapi.NewClient(filepath.Join(runtimeDir, supervisor.SocketName))
	var processes supervisor.Status
	var state control.State
	if err := await(ctx, "actual product startup", func() error {
		if err := manager.Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
			return err
		}
		if processes.PID != 1 || processes.Platform != "appliance" || processes.ReleaseID != identity || !processes.Ready || processes.Lifecycle != supervisor.Running {
			return fmt.Errorf("actual appliance PID 1 not ready: pid=%d platform=%q release=%q ready=%t lifecycle=%s", processes.PID, processes.Platform, processes.ReleaseID, processes.Ready, processes.Lifecycle)
		}
		if err := verifyExecutables(processes); err != nil {
			return err
		}
		if err := client.Do(ctx, "GET", "/v1/config", nil, &state); err != nil {
			return err
		}
		if state.Services == nil || state.Services.Phase != "ready" {
			return errors.New("routing projection not ready")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := verifyMountedData(); err != nil {
		return err
	}
	if err := requireNoNIC(); err != nil {
		return err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	w := witness{Phase: phase, BootID: strings.TrimSpace(string(boot)), PID: 1, ExecVerified: true, Filesystem: "ext4", Revision: state.Revision}
	if phase == "produce" {
		return produce(ctx, client, manager, w)
	}
	expected, err := readExpected()
	if err != nil {
		return err
	}
	if expected.BootID == w.BootID || expected.OriginBootID == w.BootID || processes.InstanceID == expected.OriginalSupervisor {
		return errors.New("new boot reused old protocol authority")
	}
	w.OriginBootID = expected.OriginBootID
	w.OriginalSupervisor = expected.OriginalSupervisor
	w.OriginalEpoch = expected.OriginalEpoch
	w.OriginalGeneration = expected.OriginalGeneration
	w.OriginalDigest = expected.OriginalDigest
	w.Observations = expected.Observations
	currentCompleted, err := readCompleted(ctx)
	if err != nil {
		return err
	}
	if err = verifyCompleted(expected.Completed, currentCompleted); err != nil {
		return err
	}
	w.Completed = currentCompleted
	if phase == "registry" {
		return recoverAndCut(ctx, client, manager, state, w)
	}
	return finishRecovery(ctx, client, manager, w)
}

func produce(ctx context.Context, client, manager *localapi.Client, w witness) error {
	if w.Revision != 1 {
		return errors.New("produce did not use seeded revision")
	}
	var queue []events.Event
	var instance runtimeplan.InstanceStatus
	var processes supervisor.Status
	if err := await(ctx, "two durable passed probes", func() error {
		var err error
		queue, err = pending(ctx)
		if err != nil {
			return err
		}
		passed := 0
		for _, e := range queue {
			if e.Topic == "routerd.healthcheck.wan.passed" {
				passed++
			}
		}
		if passed < 2 {
			return errors.New("two passed observations not yet accepted")
		}
		if err = client.Do(ctx, "GET", "/v1/services/service.wan", nil, &instance); err != nil {
			return err
		}
		if instance.Sequence == 0 || instance.Generation == 0 {
			return errors.New("health producer not registered")
		}
		if err = manager.Do(ctx, "GET", "/v1/status", nil, &processes); err != nil {
			return err
		}
		h, err := readHealth(ctx, client)
		if err != nil {
			return err
		}
		if h.ProtocolEvents.LastError == "" || h.ProtocolEvents.Registered != 1 {
			return errors.New("target fault not observed by real control")
		}
		return nil
	}); err != nil {
		return err
	}
	pid := 0
	for _, m := range processes.Modules {
		if m.Name == "service.wan" && m.Ready && m.Generation == instance.Generation {
			pid = m.PID
		}
	}
	if pid <= 1 {
		return errors.New("health worker missing before freeze")
	}
	if err := unix.Kill(pid, unix.SIGSTOP); err != nil {
		return err
	}
	if err := await(ctx, "health SIGSTOP", func() error {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "State:\tT") {
			return errors.New("health worker not stopped")
		}
		return nil
	}); err != nil {
		return err
	}
	var err error
	queue, err = pending(ctx)
	if err != nil {
		return err
	}
	if len(queue) < 2 {
		return errors.New("frozen accepted queue is incomplete")
	}
	w.Observations = observations(queue)
	w.OriginBootID = w.BootID
	w.OriginalSupervisor = instance.SupervisorInstance
	w.OriginalEpoch = instance.ProducerEpoch
	w.OriginalGeneration = instance.Generation
	w.OriginalDigest = instance.ConfigDigest
	if w.OriginalSupervisor != processes.InstanceID {
		return errors.New("producer was not registered to actual supervisor")
	}
	db, err := openInjector()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, o := range w.Observations {
		var count int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_id=?`, o.ID).Scan(&count); err != nil || count != 0 {
			return errors.New("frozen observation was already published")
		}
	}
	h, err := readHealth(ctx, client)
	if err != nil {
		return err
	}
	w.PendingCount = len(queue)
	w.Registered = h.ProtocolEvents.Registered
	w.Retiring = h.ProtocolEvents.Retiring
	w.HealthWorker = true
	w.HealthError = h.ProtocolEvents.LastError != ""
	if err = captureCompleted(ctx, &w, false); err != nil {
		return err
	}
	return emitAndWait(w, true)
}

func recoverAndCut(ctx context.Context, client, manager *localapi.Client, state control.State, w witness) error {
	if state.Revision != 1 {
		return errors.New("first power cut lost initial committed source")
	}
	queue, err := pending(ctx)
	if err != nil {
		return err
	}
	if err = requirePendingOriginals(w.Observations, queue); err != nil {
		return err
	}
	db, err := openInjector()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, registryTrigger); err != nil {
		return err
	}
	// Use the real configuration transaction to withdraw health and Derived;
	// its independent raw-history EventRule remains declared throughout.
	next := state.Config
	next.Resources = nil
	for _, r := range state.Config.Resources {
		if r.Service != nil && r.Service.Health != nil || r.DerivedEvent != nil {
			continue
		}
		next.Resources = append(next.Resources, r)
	}
	var committed control.State
	if err = client.Do(ctx, "POST", "/v1/config/commit", control.CommitRequest{ExpectedRevision: state.Revision, RequestID: "health-power-withdraw", Config: next}, &committed); err != nil {
		return err
	}
	if committed.Revision != 2 {
		return errors.New("withdrawal did not commit revision 2")
	}
	w.Revision = committed.Revision
	if err = await(ctx, "health withdrawal", func() error { return requireNoHealth(ctx, client, manager) }); err != nil {
		return err
	}
	queue, err = pending(ctx)
	if err != nil {
		return err
	}
	if err = requirePendingOriginals(w.Observations, queue); err != nil {
		return err
	}
	w.Observations = observations(queue) // include all newly accepted second-boot probes, too
	if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_health_power_target`); err != nil {
		return err
	}
	if err = await(ctx, "target publication and local ACK before registry ACK", func() error {
		records, err := history(ctx, client)
		if err != nil {
			return err
		}
		observed, err := verifyHistory(w.Observations, records, false)
		if err != nil {
			return err
		}
		queue, err := pending(ctx)
		if err != nil {
			return err
		}
		if len(queue) != 0 {
			return errors.New("local ACK is still pending")
		}
		h, err := readHealth(ctx, client)
		if err != nil {
			return err
		}
		if h.Healthy || h.ProtocolEvents.Registered != 1 || h.ProtocolEvents.Retiring != 1 || !strings.Contains(h.ProtocolEvents.LastError, "registry") {
			return errors.New("registry failure is not durably discoverable")
		}
		w.Observations = observed
		w.Registered = 1
		w.Retiring = 1
		w.HealthError = true
		return nil
	}); err != nil {
		return err
	}
	if err = requireNoHealth(ctx, client, manager); err != nil {
		return err
	}
	if err = requireHistoryDoesNotRestoreCurrentAuthority(ctx, client, w); err != nil {
		return err
	}
	if err = captureCompleted(ctx, &w, true); err != nil {
		return err
	}
	if err = await(ctx, "all original raw events durably archived", func() error { return requireArchivedObservations(ctx, &w) }); err != nil {
		return err
	}

	return emitAndWait(w, true)
}

func finishRecovery(ctx context.Context, client, manager *localapi.Client, w witness) error {
	if err := requireNoHealth(ctx, client, manager); err != nil {
		return err
	}
	queue, err := pending(ctx)
	if err != nil {
		return err
	}
	if len(queue) != 0 {
		return errors.New("second power cut lost durable local ACK")
	}
	if err = await(ctx, "retained empty registry after second cut", func() error {
		h, err := readHealth(ctx, client)
		if err != nil {
			return err
		}
		if h.Healthy || h.ProtocolEvents.Registered != 1 || h.ProtocolEvents.Retiring != 1 || !strings.Contains(h.ProtocolEvents.LastError, "registry") {
			return errors.New("registry ACK failure not retained after power cut")
		}
		return nil
	}); err != nil {
		return err
	}
	records, err := history(ctx, client)
	if err != nil {
		return err
	}
	if _, err = verifyHistory(w.Observations, records, true); err != nil {
		return err
	}
	if err = requireHistoryDoesNotRestoreCurrentAuthority(ctx, client, w); err != nil {
		return err
	}
	db, err := openInjector()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.ExecContext(ctx, `DROP TRIGGER fail_health_power_registry`); err != nil {
		return err
	}
	if err = await(ctx, "final healthy registry ACK", func() error {
		h, err := readHealth(ctx, client)
		if err != nil {
			return err
		}
		if !h.Healthy || h.Revision != 2 || h.ProtocolEvents.Registered != 0 || h.ProtocolEvents.Retiring != 0 {
			return errors.New("recovery has not finished")
		}
		return nil
	}); err != nil {
		return err
	}
	records, err = history(ctx, client)
	if err != nil {
		return err
	}
	if _, err = verifyHistory(w.Observations, records, true); err != nil {
		return err
	}
	if err = await(ctx, "retained EventRule output", func() error {
		records, err := history(ctx, client)
		if err != nil {
			return err
		}
		for _, r := range records {
			if r.Event.Topic == "routerd.fixture.power.samples" {
				return nil
			}
		}
		return errors.New("retained EventRule did not consume recovered raw history")
	}); err != nil {
		return err
	}
	w.Revision = 2
	if err = captureCompleted(ctx, &w, true); err != nil {
		return err
	}
	if err = requireArchivedObservations(ctx, &w); err != nil {
		return err
	}
	return emitAndWait(w, false)
}

func requireHistoryDoesNotRestoreCurrentAuthority(ctx context.Context, client *localapi.Client, w witness) error {
	records, err := history(ctx, client)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Event.ID == w.Observations[0].ID {
			u := serviceconfig.Update{SupervisorInstance: w.OriginalSupervisor, Name: "service.wan", Role: "health", ConfigDigest: w.OriginalDigest, Generation: w.OriginalGeneration, ProducerEpoch: w.OriginalEpoch}
			err = client.Do(ctx, "POST", "/v1/protocol-events", protocolevents.Batch{Resource: "wan", Events: []events.Event{r.Event}}, nil)
			// Retirement has not acknowledged the registry yet. This historical
			// duplicate is accepted without restoring the old current producer.
			if err != nil {
				return fmt.Errorf("registered historical retry was rejected: %w", err)
			}
			u.Sequence = 1
			u.Snapshot, _ = json.Marshal(health.Snapshot{Resource: "wan", Interface: "lo", BootID: w.OriginBootID, ObservedNanos: 1, ExpiresNanos: 2})
			if err = client.Do(ctx, "POST", "/v1/services/state", u, nil); !localapi.IsHTTPStatus(err, 409) {
				return fmt.Errorf("old current notice was not fenced: %v", err)
			}
			return nil
		}
	}
	return errors.New("original observation missing before stale authority check")
}

func requireNoHealth(ctx context.Context, client, manager *localapi.Client) error {
	var s supervisor.Status
	if err := manager.Do(ctx, "GET", "/v1/status", nil, &s); err != nil {
		return err
	}
	for _, m := range s.Modules {
		if m.Role == "health" {
			return errors.New("health process remains")
		}
	}
	if err := client.Do(ctx, "GET", "/v1/services/service.wan", nil, nil); !localapi.IsHTTPStatus(err, 404) {
		return errors.New("history restored current health instance")
	}
	if err := client.Do(ctx, "GET", "/v1/derived/wan-current", nil, nil); !localapi.IsHTTPStatus(err, 404) {
		return errors.New("history restored current Derived assertion")
	}
	return nil
}

func readHealth(ctx context.Context, client *localapi.Client) (control.Health, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://routerd/v1/health", nil)
	if err != nil {
		return control.Health{}, err
	}
	response, err := client.HTTP.Do(req)
	if err != nil {
		return control.Health{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 503 {
		return control.Health{}, fmt.Errorf("unexpected health status %d", response.StatusCode)
	}
	var h control.Health
	err = json.NewDecoder(response.Body).Decode(&h)
	return h, err
}

func verifyExecutables(s supervisor.Status) error {
	return verifyExecutableCatalog(s, os.Stat)
}

func verifyExecutableCatalog(s supervisor.Status, stat func(string) (os.FileInfo, error)) error {
	if err := sameExecutable(1, "/test/bin/routerd", stat); err != nil {
		return err
	}
	workers := runtimeconfig.NativeWorkers("/test/bin/routerd-modules")
	seen := map[string]bool{}
	for _, m := range s.Modules {
		if !m.Ready || m.PID <= 1 || m.ReleaseID != identity || seen[m.Name] {
			return errors.New("supervised process not ready on fresh command pair")
		}
		path, ok := workers[m.Role]
		if !ok {
			return fmt.Errorf("unbound factory role %q", m.Role)
		}
		if err := sameExecutable(m.PID, path, stat); err != nil {
			return fmt.Errorf("module %s role %s: %w", m.Name, m.Role, err)
		}
		seen[m.Name] = true
	}
	for _, name := range []string{"logging", "network", "control", "management"} {
		if !seen[name] {
			return fmt.Errorf("missing actual core module %s", name)
		}
	}
	return nil
}

func sameExecutable(pid int, path string, stat func(string) (os.FileInfo, error)) error {
	a, err := stat(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return err
	}
	b, err := stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return fmt.Errorf("PID %d did not execute expected fresh ELF inode %s", pid, path)
	}
	return nil
}

func verifyMountedData() error {
	var fs unix.Statfs_t
	if err := unix.Statfs(stateDir, &fs); err != nil {
		return err
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC {
		return errors.New("product state is not ext4")
	}
	var node, target unix.Stat_t
	if err := unix.Stat(devicePath, &node); err != nil {
		return err
	}
	if err := unix.Stat(stateDir, &target); err != nil {
		return err
	}
	if node.Mode&unix.S_IFMT != unix.S_IFBLK || node.Rdev != target.Dev {
		return errors.New("wrong data device mounted")
	}
	return nil
}

func emitAndWait(w witness, cut bool) error {
	body, err := json.Marshal(w)
	if err != nil {
		return err
	}
	kind := "PASS"
	if cut {
		kind = "CUT"
	}
	fmt.Printf("%s_%s %s\n", marker, kind, body)
	// CUT never stops children, closes their SQLite stores, syncs files or
	// powers down. Only the host may SIGKILL the live QEMU process.
	if cut {
		for {
			time.Sleep(time.Second)
		}
	}
	_ = unix.IoctlSetInt(int(os.Stdout.Fd()), unix.TCSBRK, 1)
	unix.Sync()
	if err = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF); err != nil {
		return err
	}
	for {
		time.Sleep(time.Second)
	}
}
