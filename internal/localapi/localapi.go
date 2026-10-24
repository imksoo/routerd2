// Package localapi provides bounded, versioned HTTP/JSON APIs over private Unix sockets.
// No module API is exposed on TCP and no API accepts an operating-system command.
package localapi

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
	"path/filepath"
	"time"
)

const MaxBody = 2 << 20

type Error struct {
	Error string `json:"error"`
}

// HTTPError keeps the status available to bounded retry/fallback clients. Its
// text remains compatible with the previous string-only client errors.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("module returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
}

func IsHTTPStatus(err error, code int) bool {
	var status *HTTPError
	return errors.As(err, &status) && status.StatusCode == code
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Fail(w http.ResponseWriter, status int, err error) { JSON(w, status, Error{err.Error()}) }

func Decode(w http.ResponseWriter, r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

// Serve removes only a stale socket, never an arbitrary filesystem entry or a live listener.
func Serve(ctx context.Context, path string, h http.Handler, ready func()) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refuse to replace non-socket %s", path)
		}
		c, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			return fmt.Errorf("socket is already active: %s", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	s := &http.Server{Handler: h, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.Shutdown(stop)
			_ = s.Close()
		case <-done:
		}
	}()
	if ready != nil {
		ready()
	}
	err = s.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type Client struct{ HTTP *http.Client }

func NewClient(socket string) *Client {
	d := &net.Dialer{Timeout: time.Second}
	return &Client{HTTP: &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) }, DisableKeepAlives: true},
		Timeout:   20 * time.Second,
	}}
}

func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://routerd"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	r, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		return err
	}
	if len(b) > MaxBody {
		return errors.New("module response exceeds size limit")
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		var e Error
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return &HTTPError{StatusCode: r.StatusCode, Message: e.Error}
		}
		return &HTTPError{StatusCode: r.StatusCode}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
