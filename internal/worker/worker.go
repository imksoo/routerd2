package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

var ErrParentDisconnected = errors.New("supervisor lifecycle channel closed")

const (
	heartbeatInterval = time.Second
	writeTimeout      = 2 * time.Second
)

// ShutdownTimeout leaves the DHCPv6 owner time to withdraw dependent effects
// and complete its bounded Release exchange before the process is terminated.
func ShutdownTimeout(role string) time.Duration {
	if role == "dhcp6" {
		return 22 * time.Second
	}
	return 3 * time.Second
}

// Run identifies this worker with its compiled release ID, reports readiness,
// emits heartbeats, and cancels service when its supervisor disappears. The
// service must call ready only after its initialization has actually succeeded.
// releaseID must come from build information, not ROUTERD_RELEASE_ID.
func Run(ctx context.Context, role, releaseID string, service func(context.Context, func()) error) error {
	if service == nil || role == "" || releaseID == "" {
		return errors.New("worker requires role, compiled release ID, and service")
	}
	fd, err := strconv.Atoi(os.Getenv(ControlFDEnv))
	if err != nil || fd < 3 {
		return errors.New("worker requires an inherited lifecycle descriptor")
	}
	file := os.NewFile(uintptr(fd), "routerd-lifecycle")
	if file == nil {
		return errors.New("invalid lifecycle descriptor")
	}
	conn, err := net.FileConn(file)
	_ = file.Close() // FileConn owns a duplicate; do not leak the inherited fd.
	if err != nil {
		return fmt.Errorf("open lifecycle channel: %w", err)
	}
	defer conn.Close()
	return run(ctx, conn, os.Getenv(ModuleNameEnv), role, releaseID, service, heartbeatInterval)
}

func run(ctx context.Context, conn net.Conn, name, role, releaseID string, service func(context.Context, func()) error, interval time.Duration) error {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var writeMu sync.Mutex
	var sequence uint64
	send := func(kind, detail string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		sequence++
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		return WriteMessage(conn, Message{
			Version: ProtocolVersion, Type: kind, Name: name, Role: role,
			ReleaseID: releaseID, PID: os.Getpid(), Sequence: sequence, Error: detail,
		})
	}
	if err := send("hello", ""); err != nil {
		return fmt.Errorf("worker hello: %w", err)
	}
	channelDone := make(chan error, 2)
	go func() {
		for {
			m, err := ReadMessage(conn)
			if err != nil {
				channelDone <- fmt.Errorf("%w: %v", ErrParentDisconnected, err)
				return
			}
			if m.Type != "stop" {
				channelDone <- fmt.Errorf("unexpected supervisor message %q", m.Type)
				return
			}
			channelDone <- nil
			return
		}
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := send("heartbeat", ""); err != nil {
					select {
					case channelDone <- fmt.Errorf("worker heartbeat: %w", err):
					default:
					}
					return
				}
			}
		}
	}()
	serviceDone := make(chan error, 1)
	var once sync.Once
	go func() {
		serviceDone <- service(ctx, func() {
			once.Do(func() {
				if err := send("ready", ""); err != nil {
					select {
					case channelDone <- fmt.Errorf("worker ready: %w", err):
					default:
					}
				}
			})
		})
	}()
	var reason error
	select {
	case err := <-serviceDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			_ = send("failed", boundedError(err))
		}
		return err
	case reason = <-channelDone:
	case <-ctx.Done():
	}
	cancel()
	// Closing also interrupts a reader blocked on a dead or unresponsive peer.
	_ = conn.Close()
	timer := time.NewTimer(ShutdownTimeout(role))
	defer timer.Stop()
	select {
	case err := <-serviceDone:
		if reason != nil {
			return reason
		}
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	case <-timer.C:
		if reason != nil {
			return reason
		}
		return errors.New("worker service did not stop after cancellation")
	}
}

func boundedError(err error) string {
	s := err.Error()
	if len(s) > 4096 {
		s = s[:4096]
	}
	return s
}
