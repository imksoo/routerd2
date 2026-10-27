//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/imksoo/routerd2/internal/release"
	"github.com/imksoo/routerd2/internal/worker"
	"golang.org/x/sys/unix"
)

const testRelease = "integration-release"

// TestMain turns the same test ELF into real worker processes. These tests
// exercise fork/exec, inherited FDs, OS exits, reaping, and Unix HTTP together.
func TestMain(m *testing.M) {
	if os.Getenv("ROUTERD_SUPERVISOR_HELPER") == "1" {
		os.Exit(helperMain())
	}
	os.Exit(m.Run())
}

func helperMain() int {
	var role, name string
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--role=") {
			role = strings.TrimPrefix(arg, "--role=")
		}
		if strings.HasPrefix(arg, "--name=") {
			name = strings.TrimPrefix(arg, "--name=")
		}
	}
	state := os.Getenv("ROUTERD_STATE_DIR")
	if role == "supervisor" {
		executable, err := os.Executable()
		if err != nil {
			return 75
		}
		childRole := os.Getenv("ROUTERD_TEST_CHILD_ROLE")
		_, err = Run(context.Background(), Options{Executables: map[string]string{childRole: executable}, ReleaseID: testRelease, RuntimeDir: os.Getenv("ROUTERD_TEST_RUNTIME"), StateDir: state, Platform: "host", Modules: []Spec{{Name: "network", Role: childRole}}})
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 76
		}
		return 0
	}
	if role == "orphan-leaf" {
		_ = os.WriteFile(filepath.Join(state, "orphan.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(200 * time.Millisecond)
		return 0
	}
	if file, err := os.OpenFile(filepath.Join(state, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
		_, _ = fmt.Fprintln(file, time.Now().UnixNano())
		_ = file.Close()
	}
	_ = os.WriteFile(filepath.Join(state, "generation."+name), []byte(os.Getenv("ROUTERD_GENERATION")), 0600)
	_ = os.WriteFile(filepath.Join(state, "instance."+name), []byte(os.Getenv(worker.SupervisorInstanceEnv)), 0600)
	_ = os.WriteFile(filepath.Join(state, "authority."+name), []byte(os.Getenv(release.AuthorityEnv)), 0600)
	if role == "nohello" {
		signal.Ignore(syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	}
	if role == "quiet" || role == "orphan" {
		f := os.NewFile(3, "control")
		conn, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			return 71
		}
		defer conn.Close()
		for i, kind := range []string{"hello", "ready"} {
			if err := worker.WriteMessage(conn, worker.Message{Type: kind, Name: name, Role: role, PID: os.Getpid(), ReleaseID: testRelease, Sequence: uint64(i + 1)}); err != nil {
				return 72
			}
		}
		if role == "orphan" {
			executable, err := os.Executable()
			if err != nil {
				return 73
			}
			_, err = os.StartProcess(executable, []string{executable, "--role=orphan-leaf"}, &os.ProcAttr{Env: os.Environ(), Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
			if err != nil {
				return 74
			}
			time.Sleep(40 * time.Millisecond)
			return 17 // Deliberately leave a descendant for the subreaper.
		}
		_, _ = worker.ReadMessage(conn)
		return 0
	}
	release := testRelease
	if role == "badrelease" {
		release = "unexpected-release"
	}
	err := worker.Run(context.Background(), role, release, func(ctx context.Context, ready func()) error {
		ready()
		if role == "exit" {
			time.Sleep(20 * time.Millisecond)
			return errors.New("deliberate worker failure")
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return 23
	}
	return 0
}

type testSupervisor struct {
	options Options
	cancel  context.CancelFunc
	done    chan error
	client  *http.Client
	closed  bool
}

func startTestSupervisor(t *testing.T, modules []Spec) *testSupervisor {
	return startTestSupervisorRoles(t, modules, nil)
}
func startTestSupervisorRoles(t *testing.T, modules []Spec, roles []string) *testSupervisor {
	return startTestSupervisorCatalog(t, modules, roles, false)
}
func startTestSupervisorCatalog(t *testing.T, modules []Spec, roles []string, split bool) *testSupervisor {
	return startTestSupervisorCustomized(t, modules, roles, split, nil)
}

func startTestSupervisorCustomized(t *testing.T, modules []Spec, roles []string, split bool, customize func(*Options)) *testSupervisor {
	t.Helper()
	dir, err := os.MkdirTemp("", "routerd-sup-")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "modules")
	if err := os.Symlink(executable, image); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	catalog := testRolePaths(image, modules, roles)
	if split {
		data, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		for role := range catalog {
			path := filepath.Join(dir, "component-"+role)
			if err := os.WriteFile(path, data, 0700); err != nil {
				t.Fatal(err)
			}
			catalog[role] = path
		}
	}
	o := Options{Executables: catalog, ReleaseID: testRelease, RuntimeDir: filepath.Join(dir, "run"), StateDir: state, Platform: "host", Modules: modules, DynamicRoles: roles, Env: []string{"ROUTERD_SUPERVISOR_HELPER=1", worker.SupervisorInstanceEnv + "=inherited-value-must-not-choose-authority"}}
	if customize != nil {
		customize(&o)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &testSupervisor{options: o, cancel: cancel, done: make(chan error, 1)}
	s.client = &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(o.RuntimeDir, SocketName))
	}}}
	go func() { _, err := Run(ctx, o); s.done <- err }()
	t.Cleanup(func() {
		s.stop(t)
		s.client.CloseIdleConnections()
		_ = os.RemoveAll(dir)
	})
	return s
}

func (s *testSupervisor) stop(t *testing.T) {
	t.Helper()
	if s.closed {
		return
	}
	s.closed = true
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			t.Errorf("supervisor exited: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Error("supervisor did not stop and reap its children")
	}
}

func (s *testSupervisor) status() (Status, error) {
	var status Status
	response, err := s.client.Get("http://routerd/v1/status")
	if err != nil {
		return status, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return status, fmt.Errorf("status HTTP %d", response.StatusCode)
	}
	err = json.NewDecoder(response.Body).Decode(&status)
	return status, err
}

func (s *testSupervisor) wait(t *testing.T, predicate func(Status) bool) Status {
	t.Helper()
	var last Status
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		last, lastErr = s.status()
		if lastErr == nil && predicate(last) {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("status condition not reached: %+v (last error %v)", last, lastErr)
	return last
}

func (s *testSupervisor) request(t *testing.T, method, path string) int {
	t.Helper()
	request, err := http.NewRequest(method, "http://routerd"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestSupervisorReadinessRestartIsolationAndPinnedImage(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "ready"}, {Name: "management", Role: "ready"}})
	first := s.wait(t, func(status Status) bool { return status.Ready && len(status.Modules) == 2 })
	if len(first.InstanceID) != 32 || strings.Contains(first.InstanceID, "inherited") {
		t.Fatal("supervisor did not create its own instance identity")
	}
	for _, module := range first.Modules {
		if module.ReleaseID != testRelease || module.Generation != 1 || !module.Ready {
			t.Fatalf("unexpected initial worker: %+v", module)
		}
		if generation, err := os.ReadFile(filepath.Join(s.options.StateDir, "generation."+module.Name)); err != nil || string(generation) != "1" {
			t.Fatalf("worker generation environment: %q %v", generation, err)
		}
		if instance, err := os.ReadFile(filepath.Join(s.options.StateDir, "instance."+module.Name)); err != nil || string(instance) != first.InstanceID {
			t.Fatalf("worker did not inherit the supervisor's instance: %q %v", instance, err)
		}
	}
	for path, want := range map[string]os.FileMode{s.options.RuntimeDir: 0750, filepath.Join(s.options.RuntimeDir, SocketName): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("permissions %s: %v, %v", path, info, err)
		}
	}
	if got := s.request(t, http.MethodGet, "/v1/modules/network/restart"); got != http.StatusMethodNotAllowed {
		t.Fatalf("GET restart status: %d", got)
	}
	if got := s.request(t, http.MethodPost, "/v1/modules/missing/restart"); got != http.StatusNotFound {
		t.Fatalf("unknown restart status: %d", got)
	}
	// A changed pathname must not make one restarted worker use another image.
	if err := os.Remove(s.options.Executables["ready"]); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/does-not-exist", s.options.Executables["ready"]); err != nil {
		t.Fatal(err)
	}
	if got := s.request(t, http.MethodPost, "/v1/modules/network/restart"); got != http.StatusAccepted {
		t.Fatalf("restart status: %d", got)
	}
	second := s.wait(t, func(status Status) bool { return status.Ready && status.Modules[0].Generation == 2 })
	if second.InstanceID != first.InstanceID {
		t.Fatal("worker restart changed the supervisor's instance identity")
	}
	if second.Modules[0].PID == first.Modules[0].PID || second.Modules[1].PID != first.Modules[1].PID {
		t.Fatalf("restart was not isolated: before=%+v after=%+v", first.Modules, second.Modules)
	}
	if generation, err := os.ReadFile(filepath.Join(s.options.StateDir, "generation.network")); err != nil || string(generation) != "2" {
		t.Fatalf("restart did not export the new generation: %q %v", generation, err)
	}
	s.stop(t)
	for _, module := range second.Modules {
		if err := syscall.Kill(module.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("child %d remains after supervisor shutdown: %v", module.PID, err)
		}
	}
}

func TestSupervisorRejectsMismatchedReleaseWithoutRestartLoop(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "badrelease", StopTimeout: time.Second}})
	status := s.wait(t, func(status Status) bool { return len(status.Modules) == 1 && status.Modules[0].State == "failed" })
	module := status.Modules[0]
	if status.Ready || module.ReleaseID != "unexpected-release" || !strings.Contains(module.LastError, "release mismatch") {
		t.Fatalf("mismatch not visible: %+v", status)
	}
	time.Sleep(250 * time.Millisecond)
	status, err := s.status()
	if err != nil || status.Modules[0].Generation != 1 {
		t.Fatalf("permanent mismatch restarted: %+v, %v", status, err)
	}
}

func TestSupervisorStartupTimeoutKillsUnresponsiveWorker(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "nohello", Restart: RestartNever, StartupTimeout: 100 * time.Millisecond, StopTimeout: 100 * time.Millisecond}})
	status := s.wait(t, func(status Status) bool { return len(status.Modules) == 1 && status.Modules[0].LastExit != nil })
	module := status.Modules[0]
	if module.Ready || module.PID != 0 || module.LastExit.Signal != "killed" || !strings.Contains(module.LastError, "startup readiness timeout") {
		t.Fatalf("startup timeout did not kill/reap worker: %+v", module)
	}
}

func TestSupervisorDetectsMissingHeartbeat(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "quiet", Restart: RestartNever, HeartbeatTimeout: 120 * time.Millisecond, StopTimeout: time.Second}})
	status := s.wait(t, func(status Status) bool { return len(status.Modules) == 1 && status.Modules[0].LastExit != nil })
	if status.Ready || !strings.Contains(status.Modules[0].LastError, "heartbeat timeout") {
		t.Fatalf("missing heartbeat not detected: %+v", status)
	}
}

func TestSupervisorCrashLoopUsesIncreasingBackoff(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "exit", BackoffMin: 80 * time.Millisecond, BackoffMax: 400 * time.Millisecond, StopTimeout: time.Second}})
	s.wait(t, func(status Status) bool { return len(status.Modules) == 1 && status.Modules[0].Restarts >= 3 })
	var starts []int64
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		contents, _ := os.ReadFile(filepath.Join(s.options.StateDir, "starts"))
		starts = nil
		for _, field := range strings.Fields(string(contents)) {
			n, err := strconv.ParseInt(field, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			starts = append(starts, n)
		}
		if len(starts) >= 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(starts) < 4 {
		t.Fatalf("only %d child starts recorded", len(starts))
	}
	for i, minimum := range []time.Duration{80 * time.Millisecond, 160 * time.Millisecond, 320 * time.Millisecond} {
		if gap := time.Duration(starts[i+1] - starts[i]); gap < minimum {
			t.Fatalf("restart %d gap %s below %s", i, gap, minimum)
		}
	}
}

func TestSupervisorReapsAdoptedGrandchild(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "orphan", Restart: RestartNever}})
	s.wait(t, func(status Status) bool { return len(status.Modules) == 1 && status.Modules[0].LastExit != nil })
	contents, err := os.ReadFile(filepath.Join(s.options.StateDir, "orphan.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(contents))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("adopted child %d was not reaped", pid)
}

func TestRuntimeAPIRefusesReplacingRegularFileAndSymlinkDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SocketName)
	if err := os.WriteFile(path, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if a, err := openAPI(context.Background(), dir, Status{}); err == nil {
		a.close()
		t.Fatal("API replaced regular file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "preserve me" {
		t.Fatalf("existing file changed: %q %v", data, err)
	}
	link := filepath.Join(t.TempDir(), "runtime")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if a, err := openAPI(context.Background(), link, Status{}); err == nil {
		a.close()
		t.Fatal("API accepted symlink runtime directory")
	}
}

func TestParentDeathSignalKillsWorkerAndSIGTERMDrainsTree(t *testing.T) {
	// Adopt the nested supervisor's workers if it is killed, so the test itself
	// can verify and reap their exit rather than relying on the host's init.
	var previous int32
	if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0)
	for _, termination := range []syscall.Signal{syscall.SIGKILL, syscall.SIGTERM} {
		t.Run(termination.String(), func(t *testing.T) {
			dir, err := os.MkdirTemp("", "routerd-death-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			runtimeDir := filepath.Join(dir, "run")
			childRole := "ready"
			if termination == syscall.SIGKILL {
				// A cooperative worker can exit on socket EOF before the
				// kernel delivers SIGKILL. Use the existing stuck worker to
				// measure the parent-death signal itself.
				childRole = "nohello"
			}
			process, err := os.StartProcess(executable, []string{executable, "--role=supervisor"}, &os.ProcAttr{
				Env:   mergeEnv(os.Environ(), []string{"ROUTERD_SUPERVISOR_HELPER=1", "ROUTERD_TEST_RUNTIME=" + runtimeDir, "ROUTERD_STATE_DIR=" + dir, "ROUTERD_TEST_CHILD_ROLE=" + childRole}),
				Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer process.Kill()
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(runtimeDir, SocketName))
			}}}
			defer client.CloseIdleConnections()
			s := &testSupervisor{client: client}
			status := s.wait(t, func(status Status) bool {
				return len(status.Modules) == 1 && status.Modules[0].PID > 0 && (termination == syscall.SIGKILL || status.Ready)
			})
			pid := status.Modules[0].PID
			if err := process.Signal(termination); err != nil {
				t.Fatal(err)
			}
			state, err := process.Wait() // This process has no running Run/reaper.
			if err != nil {
				t.Fatal(err)
			}
			if termination == syscall.SIGTERM {
				if !state.Success() {
					t.Fatalf("graceful supervisor stop: %v", state)
				}
				if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
					t.Fatalf("SIGTERM left worker %d alive: %v", pid, err)
				}
				return
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				var exit syscall.WaitStatus
				got, err := syscall.Wait4(pid, &exit, syscall.WNOHANG, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got == pid {
					if !exit.Signaled() || exit.Signal() != syscall.SIGKILL {
						t.Fatalf("worker did not receive parent-death SIGKILL: %v", exit)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_, _ = syscall.Wait4(pid, nil, 0, nil)
			t.Fatal("worker survived supervisor death")
		})
	}
}

func TestDynamicModulesReconcileFencesCoreAndReplacesProcesses(t *testing.T) {
	s := startTestSupervisorRoles(t, []Spec{{Name: "network", Role: "ready", Required: true}}, []string{"ready"})
	first := s.wait(t, func(v Status) bool { return v.Ready && len(v.Modules) == 1 })
	corePID := first.Modules[0].PID
	submit := func(modules []DynamicModule, want int) {
		t.Helper()
		b, _ := json.Marshal(ReconcileModules{Modules: modules})
		response, err := s.client.Post("http://routerd/v1/modules/reconcile", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("reconcile status %d want %d: %s", response.StatusCode, want, body)
		}
	}
	a := DynamicModule{Name: "service.wan", Role: "ready", ConfigDigest: strings.Repeat("a", 64)}
	submit([]DynamicModule{a}, 202)
	active := s.wait(t, func(v Status) bool {
		return v.Ready && len(v.Modules) == 2 && v.Modules[1].ConfigDigest == a.ConfigDigest
	})
	pid, gen := active.Modules[1].PID, active.Modules[1].Generation
	submit([]DynamicModule{a}, 202)
	unchanged := s.wait(t, func(v Status) bool { return v.Ready && len(v.Modules) == 2 })
	if unchanged.Modules[1].PID != pid || unchanged.Modules[1].Generation != gen {
		t.Fatal("unchanged instance restarted")
	}
	a.ConfigDigest = strings.Repeat("b", 64)
	submit([]DynamicModule{a}, 202)
	replaced := s.wait(t, func(v Status) bool {
		return v.Ready && len(v.Modules) == 2 && v.Modules[1].PID != pid && v.Modules[1].Generation > gen
	})
	if replaced.Modules[0].PID != corePID || replaced.Modules[1].ConfigDigest != a.ConfigDigest {
		t.Fatalf("wrong replacement: %+v", replaced)
	}
	for _, bad := range []DynamicModule{
		{Name: "network", Role: "ready", ConfigDigest: a.ConfigDigest},
		{Name: "service.x", Role: "control", ConfigDigest: a.ConfigDigest},
		{Name: "service.x", Role: "ready", ConfigDigest: "../x"},
	} {
		submit([]DynamicModule{bad}, 422)
	}
	submit(nil, 202)
	removed := s.wait(t, func(v Status) bool { return v.Ready && len(v.Modules) == 1 })
	if removed.Modules[0].PID != corePID {
		t.Fatal("core process was restarted")
	}
	// Name reuse must ignore lifecycle frames from the reaped child object.
	submit([]DynamicModule{a}, 202)
	readded := s.wait(t, func(v Status) bool { return v.Ready && len(v.Modules) == 2 })
	if readded.Modules[1].Generation <= replaced.Modules[1].Generation {
		t.Fatal("removed/re-added worker reused an old protocol generation", readded.Modules[1].Generation, replaced.Modules[1].Generation)
	}
}

func testRolePaths(path string, modules []Spec, roles []string) map[string]string {
	paths := map[string]string{}
	for _, s := range modules {
		role := s.Role
		if role == "" {
			role = s.Name
		}
		paths[role] = path
	}
	for _, role := range roles {
		paths[role] = path
	}
	return paths
}

func TestHostSupervisorRejectsPowerOperation(t *testing.T) {
	s := startTestSupervisor(t, []Spec{{Name: "network", Role: "ready"}})
	before := s.wait(t, func(status Status) bool { return status.Ready })
	for _, action := range []PowerAction{PowerOff, Reboot} {
		body, err := json.Marshal(PowerRequest{Action: action})
		if err != nil {
			t.Fatal(err)
		}
		response, err := s.client.Post("http://supervisor/v1/power", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusConflict {
			t.Fatalf("%s status: %d", action, response.StatusCode)
		}
	}
	after := s.wait(t, func(status Status) bool { return status.Ready })
	if after.Stopping || after.Modules[0].PID != before.Modules[0].PID {
		t.Fatal("rejected power request stopped the host-mode supervisor")
	}
}
