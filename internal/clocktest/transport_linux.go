//go:build linux

package clocktest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const FDEnv = "ROUTERD2_TEST_CLOCK_FD"
const maxFrame = 4096

type request struct{ Action string }
type response struct {
	Result Result
	Error  string
}

func readFrame(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	if len(line) > maxFrame {
		return errors.New("oversized clock frame")
	}
	d := json.NewDecoder(bytes.NewReader(line))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("trailing clock frame data")
	}
	return nil
}

// Serve runs on an inherited socketpair; it never creates a listening socket.
// Closing the connection stops it before PID 1 performs its final restore.
func Serve(conn net.Conn, c *Controller) error {
	r := bufio.NewReaderSize(conn, maxFrame)
	for {
		var q request
		if err := readFrame(r, &q); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		result, err := c.Step(q.Action)
		out := response{Result: result}
		if err != nil {
			out.Error = err.Error()
		}
		if err := json.NewEncoder(conn).Encode(out); err != nil {
			return err
		}
	}
}

func verifyPeer(fd, expected int) error {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return err
	}
	if int(cred.Pid) != expected {
		return errors.New("clock broker peer is not dedicated PID 1")
	}
	return nil
}

// ForwardFileFromEnv validates the one fixed inherited descriptor, makes it
// close-on-exec, and returns a private duplicate for the netns test re-exec.
func ForwardFileFromEnv() (*os.File, error) {
	if os.Getenv(FDEnv) != "3" {
		return nil, errors.New("dedicated guest clock FD 3 is required")
	}
	if err := verifyPeer(3, 1); err != nil {
		return nil, err
	}
	unix.CloseOnExec(3)
	fd, err := unix.FcntlInt(3, unix.F_DUPFD_CLOEXEC, 4)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "guest-clock"), nil
}

type Client struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

func ClientFromEnv() (*Client, error) {
	f, err := ForwardFileFromEnv()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// The netns child consumes the inherited descriptor. Neither the original
	// nor our duplicate can survive into a production daemon exec.
	defer unix.Close(3)
	return clientFromFile(f, 1)
}

func clientFromFile(file *os.File, expectedPID int) (*Client, error) {
	if err := verifyPeer(int(file.Fd()), expectedPID); err != nil {
		return nil, err
	}
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, r: bufio.NewReaderSize(conn, maxFrame)}, nil
}

func (c *Client) Step(action string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return Result{}, err
	}
	if err := json.NewEncoder(c.conn).Encode(request{Action: action}); err != nil {
		return Result{}, err
	}
	var out response
	if err := readFrame(c.r, &out); err != nil {
		return Result{}, err
	}
	if out.Error != "" {
		return Result{}, errors.New(out.Error)
	}
	if out.Result.Action != action {
		return Result{}, errors.New("clock action response mismatch")
	}
	return out.Result, nil
}

func (c *Client) Close() error { return c.conn.Close() }
