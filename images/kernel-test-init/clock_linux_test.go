//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/clocktest"
	"golang.org/x/sys/unix"
)

func TestBrokerCleanupRestoresOriginalBaselineEvenAfterTestFailure(t *testing.T) {
	for _, failRestore := range []bool{false, true} {
		clock := boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: 1, WallTime: time.Unix(1700000000, 0).UTC()}
		original := clock.WallTime
		nonce := "0123456789abcdef0123456789abcdef"
		fail := false
		c, err := clocktest.NewController(clocktest.Guest{PID: 1, Suite: clocktest.SuiteName, Nonce: nonce, CommandLine: "routerd2.clock-test=" + nonce}, func() (boottime.Clock, error) { return clock, nil }, func(v time.Time) error {
			if fail {
				return errors.New("restore failed")
			}
			clock.WallTime = v
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.Step("forward"); err != nil {
			t.Fatal(err)
		}
		fail = failRestore
		fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		file := os.NewFile(uintptr(fds[0]), "test-server")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		b := &guestClockBroker{controller: c, conn: conn, client: os.NewFile(uintptr(fds[1]), "test-client"), done: make(chan error, 1)}
		go func() { b.done <- clocktest.Serve(conn, c) }()
		err = b.closeAndRestore()
		if (err != nil) != failRestore {
			t.Fatal("restore failure hidden", err)
		}
		if !failRestore && !clock.WallTime.Equal(original) {
			t.Fatal("failed test left guest clock shifted")
		}
	}
}
