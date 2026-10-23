package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return w.Buffer.Write(p)
}

func TestFramingHandlesPartialWritesAndRejectsBadInput(t *testing.T) {
	w := new(shortWriter)
	want := Message{Type: "hello", Name: "wan", Role: "network", ReleaseID: "r1", PID: 42, Sequence: 1}
	if err := WriteMessage(w, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(bytes.NewReader(w.Bytes()))
	if err != nil || got.Type != want.Type || got.ReleaseID != want.ReleaseID || got.PID != want.PID {
		t.Fatalf("roundtrip: got %+v, err %v", got, err)
	}
	for _, n := range []uint32{0, MaxFrameSize + 1, ^uint32(0)} {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], n)
		if _, err := ReadMessage(bytes.NewReader(header[:])); err == nil {
			t.Errorf("accepted invalid size %d", n)
		}
	}
	if _, err := ReadMessage(bytes.NewReader(w.Bytes()[:len(w.Bytes())-1])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame: %v", err)
	}
	var unsupported bytes.Buffer
	if err := WriteMessage(&unsupported, Message{Version: 999, Type: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMessage(&unsupported); err == nil {
		t.Fatal("accepted incompatible protocol version")
	}
}

func TestParentEOFStopsService(t *testing.T) {
	parent, child := net.Pipe()
	defer parent.Close()
	canceled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), child, "network", "network", "compiled", func(ctx context.Context, ready func()) error {
			ready()
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}, 10*time.Millisecond)
	}()
	for _, kind := range []string{"hello", "ready"} {
		m, err := ReadMessage(parent)
		if err != nil || m.Type != kind || m.ReleaseID != "compiled" {
			t.Fatalf("want %s, got %+v (%v)", kind, m, err)
		}
	}
	_ = parent.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrParentDisconnected) {
			t.Fatalf("parent disconnect not reported: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop on parent EOF")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("service context was not canceled")
	}
}

func TestReadyIsOnceHeartbeatContinuesAndStopIsGraceful(t *testing.T) {
	parent, child := net.Pipe()
	defer parent.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), child, "dns", "dns", "compiled", func(ctx context.Context, ready func()) error {
			ready()
			ready()
			<-ctx.Done()
			return ctx.Err()
		}, 10*time.Millisecond)
	}()
	readyCount, heartbeatCount := 0, 0
	var sequence uint64
	for i := 0; i < 5; i++ {
		_ = parent.SetReadDeadline(time.Now().Add(time.Second))
		m, err := ReadMessage(parent)
		if err != nil {
			t.Fatal(err)
		}
		if m.Sequence <= sequence {
			t.Fatal("non-monotonic lifecycle sequence")
		}
		sequence = m.Sequence
		if m.Type == "ready" {
			readyCount++
		}
		if m.Type == "heartbeat" {
			heartbeatCount++
		}
	}
	if readyCount != 1 || heartbeatCount < 2 {
		t.Fatalf("ready=%d heartbeat=%d", readyCount, heartbeatCount)
	}
	if err := WriteMessage(parent, Message{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestRunRequiresPrivateDescriptor(t *testing.T) {
	t.Setenv(ControlFDEnv, "2")
	if err := Run(context.Background(), "dns", "r1", func(context.Context, func()) error { return nil }); err == nil {
		t.Fatal("worker started without its private lifecycle descriptor")
	}
}
