//go:build linux

package clocktest

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"golang.org/x/sys/unix"
)

func testPair(t *testing.T) (net.Conn, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "server")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	peer := os.NewFile(uintptr(fds[1]), "client")
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
	return conn, peer
}

func TestClockClientRejectsOrdinaryProcessPeerAndMissingFD(t *testing.T) {
	_, file := testPair(t)
	if c, err := clientFromFile(file, 1); err == nil {
		_ = c.Close()
		t.Fatal("non-PID-1 peer accepted")
	}
	t.Setenv(FDEnv, "")
	if _, err := ForwardFileFromEnv(); err == nil {
		t.Fatal("missing inherited FD accepted")
	}
	t.Setenv(FDEnv, "4")
	if _, err := ForwardFileFromEnv(); err == nil {
		t.Fatal("arbitrary inherited FD accepted")
	}
}

func TestClockBrokerOnlyRunsFixedActionsAndReportsOriginalClock(t *testing.T) {
	server, file := testPair(t)
	clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: int64(time.Hour), WallTime: time.Unix(1700000000, 0).UTC()}
	original := clock
	c, err := NewController(validGuest(), func() (boottime.Clock, error) { return clock, nil }, func(v time.Time) error { clock.WallTime = v; return nil })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(server, c) }()
	client, err := clientFromFile(file, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close() // net.FileConn owns its duplicate; do not keep a spare peer alive.
	for _, action := range []string{"forward", "backward", "restore"} {
		r, err := client.Step(action)
		if err != nil || r.Action != action || r.Clock.BootID != original.BootID {
			t.Fatal(r, err)
		}
	}
	if _, err := client.Step("arbitrary"); err == nil {
		t.Fatal("arbitrary action succeeded")
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("broker did not stop")
	}
	if !clock.WallTime.Equal(original.WallTime) {
		t.Fatal("restore did not return to baseline")
	}
}

func TestClockBrokerRejectsOversizedAndUnknownFieldsBeforeMutation(t *testing.T) {
	for _, request := range []string{`{"action":"forward","timestamp":123}` + "\n", string(make([]byte, 5000)) + "\n"} {
		server, peer := net.Pipe()
		clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: 1, WallTime: time.Unix(1700000000, 0).UTC()}
		writes := 0
		c, err := NewController(validGuest(), func() (boottime.Clock, error) { return clock, nil }, func(time.Time) error { writes++; return nil })
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- Serve(server, c); _ = server.Close() }()
		_ = peer.SetDeadline(time.Now().Add(time.Second))
		_, _ = peer.Write([]byte(request))
		_ = peer.Close()
		select {
		case err := <-done:
			if err == nil || writes != 0 {
				t.Fatal("invalid request mutated clock", writes, err)
			}
		case <-time.After(time.Second):
			t.Fatal("oversized request blocked broker")
		}
	}
}
