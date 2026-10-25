//go:build linux && integration

package integration

import (
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestClockFixtureSocketSurvivesNamespaceChildExec(t *testing.T) {
	for _, name := range []string{"TestDaemonDHCPv4ClockCorrections", "TestDaemonVRRPClockCorrections"} {
		t.Run(name, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[0])
			file := os.NewFile(uintptr(fds[1]), "inherited-clock-test-socket")
			defer file.Close()
			files, err := fixtureClockFiles(name, func() (*os.File, error) { return file, nil })
			if err != nil || len(files) != 1 {
				t.Fatal("dedicated clock fixture omitted inherited socket", name, err, len(files))
			}
			command := exec.Command(os.Args[0], "-test.run=^TestClockFixtureSocketExecHelper$")
			command.ExtraFiles = files
			command.Env = append(os.Environ(), "ROUTERD2_CLOCK_SOCKET_HELPER=1")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("clock socket lost during child exec: %v %s", err, output)
			}
			var message [1]byte
			if n, err := unix.Read(fds[0], message[:]); err != nil || n != 1 || message[0] != 42 {
				t.Fatal("child did not use forwarded socket", n, err)
			}
		})
	}
	called := false
	for _, name := range []string{"TestDaemonVRRPOriginalOwnership", "TestDaemonDHCPv4OriginalLeaseAuthority"} {
		files, err := fixtureClockFiles(name, func() (*os.File, error) { called = true; return nil, nil })
		if err != nil || len(files) != 0 || called {
			t.Fatal("ordinary fixture acquired clock channel", name, err)
		}
	}
}

func TestClockFixtureSocketExecHelper(t *testing.T) {
	if os.Getenv("ROUTERD2_CLOCK_SOCKET_HELPER") != "1" {
		return
	}
	kind, err := unix.GetsockoptInt(3, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || kind != unix.SOCK_STREAM {
		t.Fatal("inherited FD 3 is not the original socket", kind, err)
	}
	if _, err := unix.Write(3, []byte{42}); err != nil {
		t.Fatal(err)
	}
}
