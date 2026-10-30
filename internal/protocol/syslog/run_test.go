// SPDX-License-Identifier: BSD-3-Clause

package syslog

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

// Exercise the real local logging API and an independent UDP receiver. This
// checks cursor consumption and packet contents, not a mirrored sink interface.
func TestUDPForwardingConsumesLocalCursor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtimeDir := t.TempDir()
	store, err := logging.Open(logging.Options{StateDir: t.TempDir(), RuntimeDir: runtimeDir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), 2*time.Second)
		defer done()
		if err := store.Close(stop); err != nil {
			t.Error(err)
		}
	}()
	apiReady := make(chan struct{})
	apiDone := make(chan error, 1)
	go func() {
		apiDone <- localapi.Serve(ctx, filepath.Join(runtimeDir, logging.SocketName), logging.NewAPI(store), func() { close(apiReady) })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-apiDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("local log API did not stop")
		}
	})
	select {
	case <-apiReady:
	case <-time.After(3 * time.Second):
		t.Fatal("local log API did not start")
	}
	receiver, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	client := localapi.NewClient(filepath.Join(runtimeDir, logging.SocketName))
	defer client.HTTP.CloseIdleConnections()
	var sequence uint64
	eventTime := time.Date(2026, 1, 2, 3, 4, 5, 123000000, time.UTC)
	appendRecord := func(level, message string, pid int) {
		t.Helper()
		sequence++
		entry := logging.Entry{
			Timestamp: eventTime, Module: "network", PID: pid, Generation: 7,
			Level: level, Message: message, ProducerEpoch: strings.Repeat("a", 32),
			ProducerSequence: sequence,
		}
		request, done := context.WithTimeout(ctx, time.Second)
		defer done()
		var result logging.IngestResult
		if err := client.Do(request, http.MethodPost, "/v1/logs", logging.IngestRequest{Entries: []logging.Entry{entry}}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Accepted != 1 {
			t.Fatalf("log record was not accepted: %+v", result)
		}
	}
	appendRecord("INFO", "retained before forwarding", 42)
	ready := make(chan struct{})
	stopped := make(chan error, 1)
	go func() {
		stopped <- Run(ctx, syslogconfig.Config{Address: receiver.LocalAddr().String(), Facility: "local0", Tag: "forward-test", MinLevel: "info"}, runtimeDir, func() { close(ready) })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("syslog worker did not stop within its I/O deadline")
		}
	})
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("syslog did not acquire its initial cursor")
	}
	appendRecord("DEBUG", "below minimum level", 42)
	appendRecord("INFO", "link configured", 42)
	appendRecord("WARN", "link degraded", 42)
	appendRecord("ERROR", "link unavailable", 42)
	appendRecord("ERROR", "own forwarding error must not circulate", os.Getpid())
	cases := []struct{ priority, message string }{
		{"<134>", "info link configured"},
		{"<132>", "warning link degraded"},
		{"<131>", "error link unavailable"},
	}
	packet := make([]byte, 16384)
	for _, want := range cases {
		if err := receiver.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		n, _, err := receiver.ReadFrom(packet)
		if err != nil {
			t.Fatal(err)
		}
		message := string(packet[:n])
		for _, part := range []string{want.message, "forward-test[", "timestamp=2026-01-02T03:04:05.123Z", "module=network pid=42 generation=7", "epoch=" + store.Status().Epoch, "sequence="} {
			if !strings.Contains(message, part) {
				t.Fatalf("packet missing %q: %s", part, message)
			}
		}
		if !strings.HasPrefix(message, want.priority) {
			t.Fatalf("wrong syslog priority: %s", message)
		}
	}
	if err := receiver.SetReadDeadline(time.Now().Add(400 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if n, _, err := receiver.ReadFrom(packet); err == nil {
		t.Fatalf("replayed, filtered or self-generated record was forwarded: %s", packet[:n])
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatal(err)
	}
	result, err := store.Query(logging.Query{})
	if err != nil || len(result.Records) != 6 {
		t.Fatalf("forwarding altered local retention: count=%d error=%v", len(result.Records), err)
	}
}
