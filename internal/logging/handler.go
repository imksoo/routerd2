package logging

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
)

// Handler and all WithAttrs/WithGroup derivatives share one bounded queue.
// Handle performs no network, filesystem, or console I/O. A failed logger is
// observable through Stats and later producer sequence gaps, never a dependency
// on which a module's work or heartbeat must wait.
type Handler struct {
	core   *handlerCore
	attrs  map[string]any
	groups []string
}

type handlerCore struct {
	options         HandlerOptions
	epoch           string
	client          deliveryClient
	queue           chan Entry
	stop, done      chan struct{}
	cancel          context.CancelFunc
	mu              sync.Mutex
	closed          bool
	produced        atomic.Uint64
	enqueued        atomic.Uint64
	delivered       atomic.Uint64
	queueDropped    atomic.Uint64
	rejected        atomic.Uint64
	deliveryDropped atomic.Uint64
	fallbackWritten atomic.Uint64
	fallbackErrors  atomic.Uint64
}

func NewHandler(options HandlerOptions) (*Handler, error) {
	if !filepath.IsAbs(options.RuntimeDir) || filepath.Clean(options.RuntimeDir) == "/" {
		return nil, errors.New("logging runtime directory must be an absolute non-root path")
	}
	options, err := normalizeHandlerOptions(options)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: options.Timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", filepath.Join(options.RuntimeDir, SocketName))
		},
		MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second,
	}
	client := &localapi.Client{HTTP: &http.Client{Transport: transport, Timeout: options.Timeout}}
	h, err := newHandler(options, client)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	go func() {
		<-h.core.done
		transport.CloseIdleConnections()
	}()
	return h, nil
}

func normalizeHandlerOptions(o HandlerOptions) (HandlerOptions, error) {
	if !moduleName.MatchString(o.Module) {
		return o, errors.New("log handler requires a valid module name")
	}
	if o.PID == 0 {
		o.PID = os.Getpid()
	}
	if o.PID < 1 {
		return o, errors.New("log handler requires a positive PID")
	}
	if o.QueueSize == 0 {
		o.QueueSize = 256
	}
	if o.Timeout == 0 {
		o.Timeout = 200 * time.Millisecond
	}
	if o.Level == nil {
		o.Level = slog.LevelInfo
	}
	if o.QueueSize < 1 || o.QueueSize > 8192 || o.Timeout < 5*time.Millisecond || o.Timeout > 5*time.Second {
		return o, errors.New("log handler queue or delivery timeout is outside supported bounds")
	}
	return o, nil
}

func newHandler(o HandlerOptions, client deliveryClient) (*Handler, error) {
	epoch, err := newEpoch()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	core := &handlerCore{options: o, epoch: epoch, client: client, queue: make(chan Entry, o.QueueSize), stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	handler := &Handler{core: core}
	go core.run(ctx)
	return handler, nil
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.core.options.Level.Level()
}

func (h *Handler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any, len(h.attrs)+4)
	for key, value := range h.attrs {
		attrs[key] = value
	}
	count := 0
	record.Attrs(func(attr slog.Attr) bool {
		if count >= 64 {
			attrs["attributes_truncated"] = true
			return false
		}
		count++
		appendAttr(attrs, h.groups, attr, 0)
		return true
	})
	level := "DEBUG"
	switch {
	case record.Level >= slog.LevelError:
		level = "ERROR"
	case record.Level >= slog.LevelWarn:
		level = "WARN"
	case record.Level >= slog.LevelInfo:
		level = "INFO"
	}
	entry := Entry{
		Timestamp: record.Time, Module: h.core.options.Module, PID: h.core.options.PID, Generation: h.core.options.Generation,
		Level: level, Message: boundedString(record.Message, 4096), Attributes: attrs,
		ProducerEpoch: h.core.epoch, ProducerSequence: ^uint64(0), ProducerDropped: ^uint64(0),
	}
	entry, err := validateEntry(entry)
	h.core.mu.Lock()
	defer h.core.mu.Unlock()
	entry.ProducerSequence = h.core.produced.Add(1)
	if err != nil {
		h.core.rejected.Add(1)
		return nil
	}
	entry.ProducerDropped = h.core.queueDropped.Load() + h.core.rejected.Load() + h.core.deliveryDropped.Load()
	if h.core.closed {
		h.core.queueDropped.Add(1)
		return nil
	}
	select {
	case h.core.queue <- entry:
		h.core.enqueued.Add(1)
	default:
		h.core.queueDropped.Add(1)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := &Handler{core: h.core, groups: append([]string(nil), h.groups...), attrs: make(map[string]any, len(h.attrs)+min(len(attrs), 64))}
	for key, value := range h.attrs {
		clone.attrs[key] = value
	}
	for i, attr := range attrs {
		if i >= 64 {
			clone.attrs["attributes_truncated"] = true
			break
		}
		appendAttr(clone.attrs, clone.groups, attr, 0)
	}
	return clone
}

func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := &Handler{core: h.core, attrs: h.attrs, groups: append([]string(nil), h.groups...)}
	if len(clone.groups) < 8 {
		if sensitiveKey(name) {
			name = "secret"
		}
		clone.groups = append(clone.groups, boundedString(name, 128))
	}
	return clone
}

func appendAttr(attrs map[string]any, groups []string, attr slog.Attr, depth int) {
	if attr.Equal(slog.Attr{}) {
		return
	}
	if len(attrs) >= 32 || depth > 8 {
		attrs["attributes_truncated"] = true
		return
	}
	key := strings.Join(append(append([]string(nil), groups...), attr.Key), ".")
	redact := sensitiveKey(key)
	key = boundedString(key, 128)
	if redact {
		attrs[key] = "[redacted]"
		return
	}
	value := attr.Value
	switch value.Kind() {
	case slog.KindGroup:
		nextGroups := groups
		if attr.Key != "" {
			nextGroups = append(append([]string(nil), groups...), attr.Key)
		}
		for i, child := range value.Group() {
			if i >= 64 {
				attrs["attributes_truncated"] = true
				break
			}
			appendAttr(attrs, nextGroups, child, depth+1)
		}
	case slog.KindString:
		attrs[key] = boundedString(value.String(), 1024)
	case slog.KindBool:
		attrs[key] = value.Bool()
	case slog.KindInt64:
		attrs[key] = value.Int64()
	case slog.KindUint64:
		attrs[key] = value.Uint64()
	case slog.KindFloat64:
		attrs[key] = scalarAttribute(value.Float64())
	case slog.KindDuration:
		attrs[key] = value.Duration().String()
	case slog.KindTime:
		attrs[key] = value.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindAny:
		attrs[key] = scalarAttribute(value.Any())
	default:
		// Do not execute arbitrary LogValuer code on a routing/control hot path.
		attrs[key] = "[unsupported log value]"
	}
}

func (c *handlerCore) deliver(parent context.Context, batch []Entry) {
	ctx, cancel := context.WithTimeout(parent, c.options.Timeout)
	err := c.client.Do(ctx, http.MethodPost, "/v1/logs", IngestRequest{Entries: batch}, nil)
	cancel()
	if err == nil {
		c.delivered.Add(uint64(len(batch)))
		return
	}
	c.deliveryDropped.Add(uint64(len(batch)))
	if c.options.Fallback == nil {
		return
	}
	for _, entry := range batch {
		b, encodeErr := json.Marshal(struct {
			Entry
			Delivery string `json:"delivery"`
		}{entry, "console_fallback"})
		if encodeErr != nil {
			c.fallbackErrors.Add(1)
			continue
		}
		b = append(b, '\n')
		n, writeErr := c.options.Fallback.Write(b)
		if writeErr != nil || n != len(b) {
			c.fallbackErrors.Add(1)
		} else {
			c.fallbackWritten.Add(1)
		}
	}
}

func (c *handlerCore) run(ctx context.Context) {
	defer close(c.done)
	defer c.cancel()
	batch := make([]Entry, 0, MaxBatch)
	flush := func(first Entry) {
		batch = append(batch[:0], first)
		for len(batch) < MaxBatch {
			select {
			case entry := <-c.queue:
				batch = append(batch, entry)
			default:
				c.deliver(ctx, batch)
				return
			}
		}
		c.deliver(ctx, batch)
	}
	for {
		select {
		case entry := <-c.queue:
			flush(entry)
		case <-c.stop:
			for {
				select {
				case entry := <-c.queue:
					flush(entry)
				default:
					return
				}
			}
		}
	}
}

func (h *Handler) Stats() HandlerStats {
	c := h.core
	return HandlerStats{ProducerEpoch: c.epoch, Produced: c.produced.Load(), Enqueued: c.enqueued.Load(), Delivered: c.delivered.Load(), QueueDropped: c.queueDropped.Load(), Rejected: c.rejected.Load(), DeliveryDropped: c.deliveryDropped.Load(), FallbackWritten: c.fallbackWritten.Load(), FallbackErrors: c.fallbackErrors.Load(), QueueDepth: len(c.queue), QueueLimit: cap(c.queue)}
}

func (h *Handler) Close(ctx context.Context) error {
	c := h.core
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	}
}

var _ slog.Handler = (*Handler)(nil)
