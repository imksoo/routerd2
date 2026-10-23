package logging

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
)

func startService(t *testing.T, options Options) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() { done <- Run(ctx, options, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("logging service failed to start: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("logging service startup timed out")
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("logging service stopped with error: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("logging service did not shut down")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestUnixServiceProducerOutageAndRestart(t *testing.T) {
	o := testOptions(t)
	stop := startService(t, o)
	info, err := os.Stat(filepath.Join(o.RuntimeDir, SocketName))
	if err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("private socket permissions: %v %v", info, err)
	}
	var fallback bytes.Buffer
	handler, err := NewHandler(HandlerOptions{RuntimeDir: o.RuntimeDir, Module: "network", Generation: 2, Timeout: 50 * time.Millisecond, Fallback: &fallback})
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandler(t, handler)
	logger := slog.New(handler)
	logger.Info("before logging restart")
	eventually(t, func() bool { return handler.Stats().Delivered == 1 })
	client := localapi.NewClient(filepath.Join(o.RuntimeDir, SocketName))
	var before QueryResult
	if err := client.Do(context.Background(), http.MethodGet, "/v1/logs", nil, &before); err != nil || len(before.Records) != 1 {
		t.Fatalf("first query: %+v %v", before, err)
	}
	stop()
	logger.Warn("while logging service is down")
	eventually(t, func() bool { return handler.Stats().DeliveryDropped == 1 && handler.Stats().FallbackWritten == 1 })
	stopAgain := startService(t, o)
	defer stopAgain()
	logger.Info("after logging restart")
	eventually(t, func() bool { return handler.Stats().Delivered == 2 })
	var after QueryResult
	path := fmt.Sprintf("/v1/logs?epoch=%s&after=%d", before.Cursor.Epoch, before.Cursor.Sequence)
	if err := client.Do(context.Background(), http.MethodGet, path, nil, &after); err != nil {
		t.Fatal(err)
	}
	if !after.Reset || !after.Gap || after.Cursor.Epoch == before.Cursor.Epoch || len(after.Records) != 2 || after.Records[1].ProducerGap != 1 {
		t.Fatalf("restart or outage gap was hidden: %+v", after)
	}
	closeHandler(t, handler)
	if !strings.Contains(fallback.String(), "while logging service is down") {
		t.Fatal("console fallback did not preserve the outage diagnostic")
	}
}

type slowResponse struct {
	header  http.Header
	entered chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *slowResponse) Header() http.Header { return w.header }
func (w *slowResponse) WriteHeader(int)     {}
func (w *slowResponse) Write(b []byte) (int, error) {
	w.once.Do(func() { w.entered <- struct{}{} })
	<-w.release
	return len(b), nil
}

func TestSlowQueriesCannotBlockIngestAndAreConcurrencyBounded(t *testing.T) {
	o := testOptions(t)
	store, _ := startStore(o, &testSink{}, nil, 0, nil, nil)
	defer closeStore(t, store)
	_, _ = store.Append([]Entry{testEntry(1)})
	api := NewAPI(store)
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			api.ServeHTTP(&slowResponse{header: http.Header{}, entered: entered, release: release}, httptest.NewRequest(http.MethodGet, "/v1/logs", nil))
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("slow queries failed to reach their response phase")
		}
	}
	overload := httptest.NewRecorder()
	api.ServeHTTP(overload, httptest.NewRequest(http.MethodGet, "/v1/logs", nil))
	if overload.Code != http.StatusTooManyRequests {
		t.Fatalf("query concurrency was not bounded: %d", overload.Code)
	}
	b, _ := json.Marshal(IngestRequest{Entries: []Entry{testEntry(2)}})
	response := httptest.NewRecorder()
	api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(b)))
	if response.Code != http.StatusAccepted || store.Status().Accepted != 2 {
		t.Fatalf("slow consumers blocked ingestion: %d %s", response.Code, response.Body.String())
	}
	close(release)
	wg.Wait()
}

func TestAPIRejectsMalformedBoundaries(t *testing.T) {
	o := testOptions(t)
	store, _ := startStore(o, &testSink{}, nil, 0, nil, nil)
	defer closeStore(t, store)
	api := NewAPI(store)
	for _, path := range []string{"/v1/logs?limit=201", "/v1/logs?after=5", "/v1/logs?epoch=bad", "/v1/logs?level=TRACE", "/v1/logs?limit=1&limit=2", "/v1/logs?tail=perhaps", "/v1/logs?unknown=yes"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid query %s accepted: %d", path, response.Code)
		}
	}
	for _, body := range []string{`{"entries":[],"secret":true}`, `{"entries":[]} {}`, strings.Repeat("x", MaxBatch*MaxRecordBytes+2048)} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Errorf("invalid ingest boundary accepted: %d", response.Code)
		}
	}
}

// A disposable helper proves that SIGKILL releases the file lock and complete
// appended records survive without a Store.Close or HTTP shutdown sequence.
func TestLoggingCrashHelper(t *testing.T) {
	if os.Getenv("ROUTERD_TEST_LOG_CRASH") != "1" {
		return
	}
	o, err := normalizeOptions(Options{StateDir: os.Getenv("ROUTERD_TEST_LOG_DIR"), MemoryEntries: 16, DiskQueueSize: 128, SyncInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	for n := uint64(1); n <= 64; n++ {
		if _, err := store.Append([]Entry{testEntry(n)}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, func() bool { return store.Status().DiskWritten == 64 })
	fmt.Println("LOG_READY", store.Status().Epoch)
	select {} // The test parent deliberately kills this process.
}

func TestLogWorkerSIGKILLRecovery(t *testing.T) {
	o := testOptions(t)
	o.MemoryEntries = 16
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoggingCrashHelper$")
	cmd.Env = append(os.Environ(), "ROUTERD_TEST_LOG_CRASH=1", "ROUTERD_TEST_LOG_DIR="+o.StateDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- text
	}()
	var previousEpoch string
	select {
	case text := <-line:
		fields := strings.Fields(text)
		if len(fields) != 2 || fields[0] != "LOG_READY" {
			t.Fatalf("crash helper did not initialize: %q", text)
		}
		previousEpoch = fields[1]
	case <-time.After(3 * time.Second):
		t.Fatal("crash helper startup timed out")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || !exitErr.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatalf("helper was not killed as intended: %v", err)
	}
	recovered, err := Open(o)
	if err != nil {
		t.Fatalf("killed writer kept its lock: %v", err)
	}
	defer closeStore(t, recovered)
	query, _ := recovered.Query(Query{Cursor: Cursor{Epoch: previousEpoch, Sequence: 64}, Tail: true})
	if !query.Reset || len(query.Records) != 16 || query.Records[15].Sequence != 64 {
		t.Fatalf("killed worker's appended tail was lost: %+v", query)
	}
	if result, err := recovered.Append([]Entry{testEntry(65)}); err != nil || result.Cursor.Sequence != 65 {
		t.Fatalf("post-crash append failed: %+v %v", result, err)
	}
}
