package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type testDelivery struct {
	mu      sync.Mutex
	entries []Entry
	err     error
	block   <-chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (d *testDelivery) Do(ctx context.Context, _, _ string, input, _ any) error {
	if d.entered != nil {
		d.once.Do(func() { close(d.entered) })
	}
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.entries = append(d.entries, input.(IngestRequest).Entries...)
	return nil
}

func testHandler(t *testing.T, o HandlerOptions, client deliveryClient) *Handler {
	t.Helper()
	if o.Module == "" {
		o.Module = "network"
	}
	o, err := normalizeHandlerOptions(o)
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHandler(o, client)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func closeHandler(t *testing.T, h *Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

type unsafeMarshaler struct{}

func (unsafeMarshaler) MarshalJSON() ([]byte, error) { panic("opaque values must not be marshaled") }

type unsafeLogValuer struct{}

func (unsafeLogValuer) LogValue() slog.Value { panic("opaque LogValuer must not run") }

func TestHandlerStructuredMetadataRedactionAndGrouping(t *testing.T) {
	client := &testDelivery{}
	h := testHandler(t, HandlerOptions{Module: "control", PID: 123, Generation: 8}, client)
	logger := slog.New(h).With("release", "test").WithGroup("operation").With("revision", 7)
	logger.Info("configuration committed", "password", "never-store", "argv", []string{"--token=secret"}, "config", map[string]any{"private": "secret"}, "opaque", unsafeMarshaler{}, "valuer", unsafeLogValuer{}, "duration", time.Second)
	closeHandler(t, h)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.entries) != 1 {
		t.Fatalf("missing delivered record: %+v", h.Stats())
	}
	e := client.entries[0]
	if e.Module != "control" || e.PID != 123 || e.Generation != 8 || e.Timestamp.IsZero() || e.ProducerSequence != 1 || e.Level != "INFO" {
		t.Fatalf("wrong source metadata: %+v", e)
	}
	for _, key := range []string{"operation.password", "operation.argv", "operation.config"} {
		if e.Attributes[key] != "[redacted]" {
			t.Errorf("sensitive attribute %s survived: %v", key, e.Attributes[key])
		}
	}
	if e.Attributes["release"] != "test" || e.Attributes["operation.revision"] != int64(7) || e.Attributes["operation.valuer"] != "[unsupported log value]" {
		t.Fatalf("WithAttrs/WithGroup semantics changed: %v", e.Attributes)
	}
	b, err := json.Marshal(e)
	if err != nil || strings.Contains(string(b), "never-store") || strings.Contains(string(b), "--token") || strings.Contains(string(b), `"private"`) {
		t.Fatal("structured output exposed an opaque or sensitive attribute")
	}
	if stats := h.Stats(); stats.Produced != 1 || stats.Delivered != 1 || stats.QueueDropped != 0 {
		t.Fatalf("unexpected producer counters: %+v", stats)
	}
}

func TestConcurrentHandlersPreserveProducerSequenceOrder(t *testing.T) {
	client := &testDelivery{}
	h := testHandler(t, HandlerOptions{QueueSize: 4096}, client)
	logger := slog.New(h)
	var wg sync.WaitGroup
	for worker := 0; worker < 20; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			child := logger.With("worker", worker)
			for i := 0; i < 50; i++ {
				child.Info("parallel record", "index", i)
			}
		}(worker)
	}
	wg.Wait()
	closeHandler(t, h)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.entries) != 1000 {
		t.Fatalf("lost concurrent records without overload: %d %+v", len(client.entries), h.Stats())
	}
	for i, entry := range client.entries {
		if entry.ProducerSequence != uint64(i+1) {
			t.Fatalf("concurrent enqueue reordered sequence at %d: %d", i, entry.ProducerSequence)
		}
	}
}

func TestUnavailableLoggerFallsBackWithoutFailingCaller(t *testing.T) {
	var fallback bytes.Buffer
	h := testHandler(t, HandlerOptions{Fallback: &fallback}, &testDelivery{err: errors.New("logger crashed")})
	logger := slog.New(h)
	for i := 0; i < 4; i++ {
		logger.Warn("delivery fallback", "record", i)
	}
	closeHandler(t, h)
	stats := h.Stats()
	if stats.DeliveryDropped != 4 || stats.FallbackWritten != 4 || stats.FallbackErrors != 0 {
		t.Fatalf("logger outage was not counted: %+v", stats)
	}
	if count := strings.Count(fallback.String(), `"delivery":"console_fallback"`); count != 4 {
		t.Fatalf("missing fallback records: %d", count)
	}
}

func TestSlowLoggerAndQueueOverloadRemainBounded(t *testing.T) {
	block := make(chan struct{})
	client := &testDelivery{block: block, entered: make(chan struct{})}
	h := testHandler(t, HandlerOptions{QueueSize: 2, Timeout: time.Second}, client)
	logger := slog.New(h)
	logger.Info("first")
	<-client.entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			logger.Info("overload", "index", i)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(block)
		t.Fatal("Handle blocked on the unavailable log service")
	}
	if stats := h.Stats(); stats.QueueDepth > 2 || stats.QueueDropped < 98 {
		t.Fatalf("producer queue grew or failed to count drops: %+v", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close waited indefinitely on delivery: %v", err)
	}
	close(block)
	closeHandler(t, h)
	if h.Stats().DeliveryDropped == 0 {
		t.Fatal("timed-out delivery was not visible")
	}
}

type blockedConsole struct {
	entered chan struct{}
	unblock <-chan struct{}
	once    sync.Once
}

func (w *blockedConsole) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.unblock
	return len(b), nil
}

func TestBlockedConsoleDoesNotBlockModuleOrCloseDeadline(t *testing.T) {
	unblock := make(chan struct{})
	console := &blockedConsole{entered: make(chan struct{}), unblock: unblock}
	h := testHandler(t, HandlerOptions{QueueSize: 1, Fallback: console}, &testDelivery{err: errors.New("offline")})
	logger := slog.New(h)
	logger.Info("first")
	<-console.entered
	for i := 0; i < 10; i++ {
		logger.Info("while console blocked")
	}
	if h.Stats().QueueDropped < 9 {
		t.Fatal("blocked console did not fill the bounded queue")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("console blocked Close's deadline: %v", err)
	}
	close(unblock)
	closeHandler(t, h)
}

func TestQueueDropsAreVisibleInNextDeliveredProducerRecord(t *testing.T) {
	block := make(chan struct{})
	client := &testDelivery{block: block, entered: make(chan struct{})}
	h := testHandler(t, HandlerOptions{QueueSize: 1, Timeout: time.Second}, client)
	logger := slog.New(h)
	logger.Info("first")
	<-client.entered
	logger.Info("queued")
	for i := 0; i < 5; i++ {
		logger.Info("dropped")
	}
	close(block)
	eventually(t, func() bool { return h.Stats().Delivered == 2 })
	logger.Info("after outage")
	closeHandler(t, h)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.entries) != 3 || client.entries[2].ProducerSequence != 8 || client.entries[2].ProducerDropped != 5 {
		t.Fatalf("later record hid the gap: %+v", client.entries)
	}
}

type panickingError struct{}

func (panickingError) Error() string { panic("bad error implementation") }

func TestLongSensitiveAttributeAndPanickingError(t *testing.T) {
	client := &testDelivery{}
	h := testHandler(t, HandlerOptions{}, client)
	key := strings.Repeat("prefix", 30) + "_password"
	slog.New(h).Info("diagnostic", key, "must-not-appear", "error", panickingError{})
	closeHandler(t, h)
	client.mu.Lock()
	defer client.mu.Unlock()
	b, err := json.Marshal(client.entries)
	if err != nil || strings.Contains(string(b), "must-not-appear") || !strings.Contains(string(b), "error formatter panicked") {
		t.Fatalf("attribute defense failed: %s %v", b, err)
	}
}
