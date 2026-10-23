//go:build linux

package supervisor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/imksoo/routerd2/internal/worker"
	"golang.org/x/sys/unix"
)

var running atomic.Bool
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

type child struct {
	spec             Spec
	status           ModuleStatus
	process          *os.Process
	conn             net.Conn
	hello            bool
	lastSequence     uint64
	startTime        time.Time
	heartbeat        time.Time
	nextStart        time.Time
	stopDeadline     time.Time
	failures         int
	failed           bool
	restartAfterExit bool
	permanentFailure bool
	dynamic          bool
	retired          bool
}

type childEvent struct {
	name       string
	generation uint64
	message    worker.Message
	err        error
	origin     *child
}

// Run supervises the module process tree until canceled. Worker failures are
// exposed through Status and recovered locally; they do not terminate PID 1.
// The only child waiter is this function's central reaper. In particular, no
// os.Process.Wait or exec.Cmd.Wait may race with it.
func Run(parent context.Context, options Options) error {
	if !running.CompareAndSwap(false, true) {
		return errors.New("only one supervisor may own process reaping in a process")
	}
	defer running.Store(false)
	o, err := normalize(options)
	if err != nil {
		return err
	}
	image, err := os.Open(o.ModulePath)
	if err != nil {
		return fmt.Errorf("open module image: %w", err)
	}
	defer image.Close()
	info, err := image.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("module image must be a regular executable file")
	}
	// Pdeathsig follows the creating OS thread. Pin the process manager to one
	// thread until every managed child has exited, rather than starting workers
	// on temporary Go runtime threads.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getpid() != 1 {
		var previous int32
		if err := unix.Prctl(unix.PR_GET_CHILD_SUBREAPER, uintptr(unsafe.Pointer(&previous)), 0, 0, 0); err != nil {
			return fmt.Errorf("get subreaper state: %w", err)
		}
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("become subreaper: %w", err)
		}
		defer unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, uintptr(previous), 0, 0, 0)
	}
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	apiCtx, apiCancel := context.WithCancel(context.Background())
	defer apiCancel()
	started := time.Now()
	initial := Status{ReleaseID: o.ReleaseID, Platform: o.Platform, PID: os.Getpid(), StartedAt: started}
	a, err := openAPI(apiCtx, o.RuntimeDir, initial)
	if err != nil {
		return fmt.Errorf("supervisor API: %w", err)
	}
	defer a.close()
	apiErrors := make(chan error, 1)
	go func() { apiErrors <- a.server.Serve(a.listener) }()
	events := make(chan childEvent, 256)
	children := make([]*child, 0, len(o.Modules))
	byName := make(map[string]*child, len(o.Modules))
	byPID := make(map[int]*child, len(o.Modules))
	for _, spec := range o.Modules {
		c := &child{spec: spec, status: ModuleStatus{Name: spec.Name, Role: spec.Role, Required: spec.Required, State: "pending"}, nextStart: started}
		children = append(children, c)
		byName[spec.Name] = c
	}
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	defer signal.Stop(sigchld)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	stopping, wasReady := false, false
	var fatalErr error
	ctxDone := ctx.Done()

	publish := func() {
		s := initial
		s.Stopping = stopping
		s.Ready = !stopping
		s.Modules = make([]ModuleStatus, 0, len(children))
		for _, c := range children {
			s.Modules = append(s.Modules, c.status)
			if !c.retired && c.spec.Required && !c.status.Ready {
				s.Ready = false
			}
		}
		a.publish(s)
		if s.Ready && !wasReady {
			slog.Info("ROUTERD_READY", "release", o.ReleaseID, "platform", o.Platform)
			// Keep a boot-console marker even when structured diagnostics are
			// delivered to the independent logging worker.
			_, _ = fmt.Fprintln(os.Stdout, "ROUTERD_READY")
		}
		wasReady = s.Ready
	}
	beginShutdown := func() {
		if stopping {
			return
		}
		stopping = true
		ctxDone = nil
		apiCancel()
		now := time.Now()
		// Stop in reverse declaration order; timeouts still run concurrently.
		for i := len(children) - 1; i >= 0; i-- {
			stopChild(children[i], "", false, false, now)
		}
	}
	if ctx.Err() != nil {
		beginShutdown()
	}

	for {
		now := time.Now()
		for _, c := range children {
			if !stopping && !c.retired && c.process == nil && !c.permanentFailure && !c.nextStart.IsZero() && !now.Before(c.nextStart) {
				if err := startChild(ctx, o, image, c, events); err != nil {
					c.status.LastError = err.Error()
					c.failures++
					c.status.State = "backoff"
					c.nextStart = now.Add(backoff(c.spec, c.failures))
					if c.spec.Restart == RestartNever {
						c.status.State, c.nextStart = "failed", time.Time{}
					}
					slog.Error("worker start failed", "module", c.spec.Name, "error", err)
				} else {
					byPID[c.process.Pid] = c
				}
			}
			if c.process == nil {
				continue
			}
			if !c.stopDeadline.IsZero() {
				if !now.Before(c.stopDeadline) {
					_ = c.process.Kill()
					// Retry only if a pathological kernel state delays exit.
					c.stopDeadline = now.Add(time.Second)
				}
				continue
			}
			if !c.status.Ready && now.Sub(c.startTime) > c.spec.StartupTimeout {
				stopChild(c, "startup readiness timeout", false, false, now)
			} else if c.hello && now.Sub(c.heartbeat) > c.spec.HeartbeatTimeout {
				stopChild(c, "worker heartbeat timeout", false, false, now)
			}
		}
		if err := reap(byPID, stopping); err != nil && fatalErr == nil {
			fatalErr = err
			beginShutdown()
		}
		// A removed worker is retained until wait4 has reaped it, then its
		// identity is retired. Late events carry the child pointer, so reusing
		// a name cannot mistake an old lifecycle frame for a new generation.
		kept := children[:0]
		for _, c := range children {
			if c.retired && c.process == nil {
				delete(byName, c.spec.Name)
				continue
			}
			kept = append(kept, c)
		}
		children = kept
		publish()
		if stopping && len(byPID) == 0 {
			return fatalErr
		}
		select {
		case <-ctxDone:
			beginShutdown()
		case <-ticker.C:
		case <-sigchld:
		case err := <-apiErrors:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatalErr = fmt.Errorf("supervisor API stopped: %w", err)
				beginShutdown()
			}
		case event := <-events:
			c := byName[event.name]
			if c == nil || (event.origin != nil && event.origin != c) || c.retired || c.process == nil || c.status.Generation != event.generation || !c.stopDeadline.IsZero() {
				continue
			}
			handleEvent(c, event, o.ReleaseID, time.Now())
		case request := <-a.restarts:
			c := byName[request.name]
			if c == nil || c.retired || stopping {
				request.reply <- fmt.Errorf("module %q is not available", request.name)
				continue
			}
			c.failures = 0
			c.permanentFailure = false
			if c.process == nil {
				c.nextStart = time.Now()
				c.status.State = "pending"
			} else {
				stopChild(c, "", false, true, time.Now())
			}
			request.reply <- nil
		case request := <-a.reconciles:
			if stopping {
				request.reply <- errors.New("supervisor is stopping")
				continue
			}
			specs, err := dynamicSpecs(o, request.request)
			if err != nil {
				request.reply <- err
				continue
			}
			desired := map[string]DynamicModule{}
			for _, s := range request.request.Modules {
				desired[s.Name] = s
			}
			for _, c := range children {
				if !c.dynamic {
					continue
				}
				if _, ok := desired[c.spec.Name]; !ok {
					c.retired = true
					stopChild(c, "", true, false, time.Now())
				}
			}
			for _, spec := range specs {
				digest := desired[spec.Name].ConfigDigest
				c := byName[spec.Name]
				if c == nil {
					c = &child{spec: spec, dynamic: true, status: ModuleStatus{Name: spec.Name, Role: spec.Role, Required: true, State: "pending", ConfigDigest: digest}, nextStart: time.Now()}
					children = append(children, c)
					byName[spec.Name] = c
					continue
				}
				if !c.retired && c.spec.Role == spec.Role && c.status.ConfigDigest == digest {
					continue
				}
				stopChild(c, "", false, true, time.Now())
				c.spec = spec
				c.retired = false
				c.failures = 0
				c.permanentFailure = false
				c.status.Role = spec.Role
				c.status.Required = true
				c.status.ConfigDigest = digest
				if c.process == nil {
					c.nextStart = time.Now()
					c.status.State = "pending"
				}
			}
			publish()
			request.reply <- nil
		}
	}
}

func normalize(o Options) (Options, error) {
	if o.Platform != "host" && o.Platform != "container" && o.Platform != "appliance" {
		return o, errors.New("platform must explicitly be host, container, or appliance")
	}
	if o.Platform == "appliance" && os.Getpid() != 1 {
		return o, errors.New("appliance supervision requires PID 1")
	}
	if o.ReleaseID == "" || o.ModulePath == "" || len(o.Modules) == 0 {
		return o, errors.New("supervisor requires module path, release ID, and module specs")
	}
	var err error
	o.ModulePath, err = filepath.Abs(o.ModulePath)
	if err != nil {
		return o, err
	}
	if o.RuntimeDir == "" {
		o.RuntimeDir = "/run/routerd"
	}
	o.Modules = append([]Spec(nil), o.Modules...)
	names := map[string]bool{}
	hasRequired := false
	for i := range o.Modules {
		s := &o.Modules[i]
		if s.Role == "" {
			s.Role = s.Name
		}
		if !validName.MatchString(s.Name) || !validName.MatchString(s.Role) || names[s.Name] {
			return o, fmt.Errorf("invalid or duplicate module name/role: %q/%q", s.Name, s.Role)
		}
		names[s.Name] = true
		hasRequired = hasRequired || s.Required
		if s.Restart == "" {
			s.Restart = RestartAlways
		}
		if s.Restart != RestartAlways && s.Restart != RestartNever && s.Restart != RestartOnFailure {
			return o, fmt.Errorf("invalid restart policy for %s", s.Name)
		}
		for _, d := range []struct {
			value *time.Duration
			def   time.Duration
		}{{&s.StartupTimeout, 10 * time.Second}, {&s.HeartbeatTimeout, 5 * time.Second}, {&s.StopTimeout, 5 * time.Second}, {&s.BackoffMin, 100 * time.Millisecond}, {&s.BackoffMax, 30 * time.Second}} {
			if *d.value < 0 {
				return o, fmt.Errorf("negative timeout for %s", s.Name)
			}
			if *d.value == 0 {
				*d.value = d.def
			}
		}
		if s.BackoffMax < s.BackoffMin {
			return o, fmt.Errorf("backoff maximum is below minimum for %s", s.Name)
		}
	}
	// A simple all-default specification requires every worker. Setting any
	// Required flag explicitly allows the remaining workers to be optional.
	if !hasRequired {
		for i := range o.Modules {
			o.Modules[i].Required = true
		}
	}
	return o, nil
}

func startChild(ctx context.Context, o Options, image *os.File, c *child, events chan<- childEvent) error {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	parentFD := os.NewFile(uintptr(fds[0]), "lifecycle-parent")
	childFD := os.NewFile(uintptr(fds[1]), "lifecycle-child")
	defer childFD.Close()
	conn, err := net.FileConn(parentFD)
	_ = parentFD.Close()
	if err != nil {
		return err
	}
	args := append([]string{o.ModulePath, "worker", "--role=" + c.spec.Role, "--name=" + c.spec.Name}, c.spec.Args...)
	env := mergeEnv(os.Environ(), o.Env, []string{
		worker.ControlFDEnv + "=3", worker.ModuleNameEnv + "=" + c.spec.Name,
		"ROUTERD_RELEASE_ID=" + o.ReleaseID, "ROUTERD_RUNTIME_DIR=" + o.RuntimeDir,
		"ROUTERD_STATE_DIR=" + o.StateDir, "ROUTERD_CONFIG_PATH=" + o.ConfigPath,
		"ROUTERD_PLATFORM=" + o.Platform,
		"ROUTERD_GENERATION=" + strconv.FormatUint(c.status.Generation+1, 10),
	})
	// fd 4 is the open module image, pinned at supervisor startup. Replacing
	// ModulePath or its symlink cannot mix another generation into this tree.
	process, err := os.StartProcess("/proc/self/fd/4", args, &os.ProcAttr{
		Env: env, Files: []*os.File{os.Stdin, os.Stdout, os.Stderr, childFD, image},
		Sys: &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL},
	})
	if err != nil {
		_ = conn.Close()
		return err
	}
	now := time.Now()
	c.process, c.conn = process, conn
	c.startTime, c.heartbeat = now, now
	c.hello, c.lastSequence = false, 0
	c.nextStart, c.stopDeadline = time.Time{}, time.Time{}
	c.restartAfterExit, c.permanentFailure = false, false
	c.failed = false
	c.status.Generation++
	if c.status.Generation > 1 {
		c.status.Restarts++
	}
	c.status.PID = process.Pid
	c.status.State, c.status.Ready = "starting", false
	c.status.ReleaseID, c.status.LastError = "", ""
	c.status.StartedAt, c.status.LastHeartbeat = &now, nil
	name, generation := c.spec.Name, c.status.Generation
	go func() {
		for {
			message, err := worker.ReadMessage(conn)
			select {
			case events <- childEvent{name: name, generation: generation, message: message, err: err, origin: c}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	slog.Info("worker started", "module", name, "pid", process.Pid, "generation", generation)
	return nil
}

func dynamicSpecs(o Options, request ReconcileModules) ([]Spec, error) {
	if len(request.Modules) > 128 {
		return nil, errors.New("at most 128 dynamic module instances are supported")
	}
	allowed := map[string]bool{}
	for _, role := range o.DynamicRoles {
		allowed[role] = true
	}
	core := map[string]bool{}
	for _, s := range o.Modules {
		core[s.Name] = true
	}
	seen := map[string]bool{}
	var specs []Spec
	for _, m := range request.Modules {
		if !validName.MatchString(m.Name) || !strings.HasPrefix(m.Name, "service.") || core[m.Name] || seen[m.Name] {
			return nil, fmt.Errorf("invalid, duplicate or core module name %q", m.Name)
		}
		if !allowed[m.Role] {
			return nil, fmt.Errorf("dynamic role %q is not enabled", m.Role)
		}
		b, err := hex.DecodeString(m.ConfigDigest)
		if err != nil || len(b) != 32 || strings.ToLower(m.ConfigDigest) != m.ConfigDigest {
			return nil, errors.New("module config_digest must be a lowercase SHA-256")
		}
		seen[m.Name] = true
		path := filepath.Join(o.RuntimeDir, "instances", m.Name, m.ConfigDigest+".json")
		specs = append(specs, Spec{Name: m.Name, Role: m.Role, Required: true, Args: []string{"--instance-config=" + path, "--config-digest=" + m.ConfigDigest}})
	}
	if len(specs) == 0 {
		return specs, nil
	}
	copy := o
	copy.Modules = append(append([]Spec(nil), o.Modules...), specs...)
	normal, err := normalize(copy)
	if err != nil {
		return nil, err
	}
	return normal.Modules[len(o.Modules):], nil
}

func handleEvent(c *child, event childEvent, releaseID string, now time.Time) {
	if event.err != nil {
		if !errors.Is(event.err, io.EOF) {
			c.status.LastError = event.err.Error()
		}
		stopChild(c, "worker lifecycle channel closed", false, false, now)
		return
	}
	m := event.message
	if m.Name != c.spec.Name || m.Role != c.spec.Role || m.PID != c.status.PID || m.Sequence <= c.lastSequence {
		stopChild(c, "invalid worker lifecycle identity or sequence", true, false, now)
		return
	}
	if m.ReleaseID != releaseID {
		c.status.ReleaseID = m.ReleaseID
		stopChild(c, fmt.Sprintf("release mismatch: expected %q, received %q", releaseID, m.ReleaseID), true, false, now)
		return
	}
	if !c.hello {
		if m.Type != "hello" {
			stopChild(c, "worker did not send hello first", true, false, now)
			return
		}
		c.hello = true
		c.status.ReleaseID = m.ReleaseID
	} else {
		switch m.Type {
		case "ready":
			c.status.Ready, c.status.State = true, "running"
		case "heartbeat":
		case "failed":
			detail := m.Error
			if len(detail) > 4096 {
				detail = detail[:4096]
			}
			stopChild(c, "worker failure: "+detail, false, false, now)
		default:
			stopChild(c, "unexpected lifecycle message: "+m.Type, true, false, now)
			return
		}
	}
	c.lastSequence, c.heartbeat = m.Sequence, now
	c.status.LastHeartbeat = &now
}

func stopChild(c *child, detail string, permanent, restart bool, now time.Time) {
	c.status.Ready = false
	c.restartAfterExit = restart
	c.permanentFailure = permanent
	if detail != "" {
		c.status.LastError = detail
		if detail != "worker lifecycle channel closed" {
			c.failed = true
		}
		slog.Warn("worker stopping", "module", c.spec.Name, "reason", detail)
	}
	if c.process == nil {
		c.nextStart = time.Time{}
		c.status.State = "stopped"
		return
	}
	if !c.stopDeadline.IsZero() {
		return
	}
	c.status.State = "stopping"
	c.stopDeadline = now.Add(c.spec.StopTimeout)
	_ = c.conn.SetWriteDeadline(now.Add(100 * time.Millisecond))
	if err := worker.WriteMessage(c.conn, worker.Message{Type: "stop"}); err != nil {
		_ = c.process.Signal(syscall.SIGTERM)
	}
}

func reap(byPID map[int]*child, stopping bool) error {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) || (err == nil && pid == 0) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reap child: %w", err)
		}
		c := byPID[pid]
		if c == nil {
			slog.Info("reaped adopted child", "pid", pid)
			continue
		}
		delete(byPID, pid)
		now := time.Now()
		exit := &ExitStatus{At: now, Code: status.ExitStatus()}
		if status.Signaled() {
			exit.Signal = status.Signal().String()
		}
		c.status.LastExit = exit
		c.status.PID, c.status.Ready = 0, false
		_ = c.conn.Close()
		_ = c.process.Release() // Wait4 already reaped it; never call Process.Wait.
		c.conn, c.process = nil, nil
		c.stopDeadline = time.Time{}
		if now.Sub(c.startTime) >= 30*time.Second {
			c.failures = 0
		}
		c.status.State, c.nextStart = "stopped", time.Time{}
		if c.permanentFailure {
			c.status.State = "failed"
		} else if !stopping && (c.restartAfterExit || c.spec.Restart == RestartAlways || (c.spec.Restart == RestartOnFailure && (c.failed || !status.Exited() || status.ExitStatus() != 0))) {
			c.failures++
			c.status.State = "backoff"
			c.nextStart = now.Add(backoff(c.spec, c.failures))
		}
		slog.Info("worker exited", "module", c.spec.Name, "pid", pid, "exit", exit.Code, "signal", exit.Signal, "state", c.status.State)
	}
}

func backoff(spec Spec, failures int) time.Duration {
	d := spec.BackoffMin
	for i := 1; i < failures && d < spec.BackoffMax; i++ {
		if d > spec.BackoffMax/2 {
			return spec.BackoffMax
		}
		d *= 2
	}
	if d > spec.BackoffMax {
		return spec.BackoffMax
	}
	return d
}

func mergeEnv(groups ...[]string) []string {
	result := make([]string, 0)
	indexes := make(map[string]int)
	for _, group := range groups {
		for _, item := range group {
			key, _, ok := strings.Cut(item, "=")
			if !ok || key == "" {
				continue
			}
			if i, ok := indexes[key]; ok {
				result[i] = item
			} else {
				indexes[key] = len(result)
				result = append(result, item)
			}
		}
	}
	return result
}
