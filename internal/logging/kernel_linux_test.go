//go:build linux

package logging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKernelRecordPreservesIdentityPriorityAndEscapes(t *testing.T) {
	epoch := strings.Repeat("b", 32)
	stamp := time.Date(2026, 10, 23, 12, 34, 56, 0, time.UTC)
	got, err := parseKernelRecord([]byte("30,0,5690716,-,future=value;device \\x1b[31m warning\n SUBSYSTEM=net\n DEVICE=n8\n SECRET=do-not-export\n"), epoch, 77, func(micros uint64) time.Time {
		if micros != 5690716 {
			t.Fatalf("wrong monotonic timestamp: %d", micros)
		}
		return stamp
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Module != "kernel" || got.PID != 77 || got.Level != "INFO" || got.ProducerEpoch != epoch || got.ProducerSequence != 1 || !got.Timestamp.Equal(stamp) {
		t.Fatalf("wrong kernel metadata: %+v", got)
	}
	if got.Message != `device \x1b[31m warning` || got.Attributes["syslog_facility"] != uint64(3) || got.Attributes["kernel_subsystem"] != "net" || got.Attributes["kernel_device"] != "n8" || got.Attributes["SECRET"] != nil {
		t.Fatalf("kernel record damaged escapes or imported arbitrary metadata: %+v", got)
	}
	for priority, level := range map[string]string{"0": "ERROR", "3": "ERROR", "4": "WARN", "5": "INFO", "6": "INFO", "7": "DEBUG"} {
		entry, err := parseKernelRecord([]byte(priority+",4,1,c;fragment\n"), epoch, 77, func(uint64) time.Time { return stamp })
		if err != nil || entry.Level != level || entry.Attributes["kernel_flags"] != "c" {
			t.Fatalf("priority %s mapped to %+v (%v)", priority, entry, err)
		}
	}
}

func TestKernelParserRejectsMalformedHeadersAndBoundsMessages(t *testing.T) {
	epoch := strings.Repeat("c", 32)
	now := func(uint64) time.Time { return time.Now().UTC() }
	for _, input := range []string{
		"missing separator", "6,1,2;short", "x,1,2,-;bad priority", "6,-1,2,-;negative sequence",
		"6,18446744073709551615,2,-;sequence overflow", "6,1,x,-;bad timestamp",
	} {
		if _, err := parseKernelRecord([]byte(input), epoch, 42, now); err == nil {
			t.Errorf("malformed header accepted: %q", input)
		}
	}
	entry, err := parseKernelRecord([]byte("6,1,2,-;"+strings.Repeat("日", 3000)+"\n"), epoch, 42, now)
	if err != nil || len(entry.Message) > 4096 || !strings.HasSuffix(entry.Message, " [truncated]") {
		t.Fatalf("unbounded kernel record: size %d, error %v", len(entry.Message), err)
	}
}

type kernelReadResult struct {
	data string
	err  error
}

type fakeKernelReader struct {
	reads   []kernelReadResult
	waiting chan struct{}
}

func (r *fakeKernelReader) Read(b []byte) (int, error) {
	if len(r.reads) == 0 {
		return 0, unix.EAGAIN
	}
	next := r.reads[0]
	r.reads = r.reads[1:]
	return copy(b, next.data), next.err
}
func (r *fakeKernelReader) Wait(ctx context.Context) error {
	close(r.waiting)
	<-ctx.Done()
	return ctx.Err()
}
func (*fakeKernelReader) Close() error { return nil }

func TestKernelOverrunRestartReceiptAndCancellation(t *testing.T) {
	store, err := startStore(testOptions(t), &testSink{}, nil, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, store)
	reader := &fakeKernelReader{waiting: make(chan struct{}), reads: []kernelReadResult{
		{data: "6,0,1,-;first\n"},
		{data: "6,0,1,-;same record replayed after logger restart\n"},
		{err: unix.EINTR},
		{err: unix.EPIPE},
		{data: "4,4,5,-;after kernel buffer overrun\n"},
		{data: "malformed record"},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		readKernel(ctx, store, reader, strings.Repeat("d", 32), 42, func(uint64) time.Time { return time.Now().UTC() })
	}()
	select {
	case <-reader.waiting:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("kernel reader did not reach its nonblocking wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("kernel reader ignored shutdown")
	}
	status := store.Status()
	if status.KernelEnabled || status.KernelOverruns != 1 || status.KernelReceived != 3 || status.KernelRejected != 1 || status.Accepted != 2 || status.ProducerGaps != 3 {
		t.Fatalf("kernel loss or replay was hidden: %+v", status)
	}
	result, err := store.Query(Query{Module: "kernel"})
	if err != nil || len(result.Records) != 2 || !result.Gap || result.Records[1].ProducerGap != 3 {
		t.Fatalf("kernel query did not expose its sequence gap: %+v (%v)", result, err)
	}
}

func TestKernelReadFailureLeavesDiagnosticWithoutStoppingStore(t *testing.T) {
	store, err := startStore(testOptions(t), &testSink{}, nil, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(t, store)
	reader := &fakeKernelReader{reads: []kernelReadResult{{err: errors.New("kernel source unavailable")}}}
	readKernel(context.Background(), store, reader, strings.Repeat("e", 32), 42, func(uint64) time.Time { return time.Now().UTC() })
	if !strings.Contains(store.Status().LastKernelError, "kernel source unavailable") || store.Status().KernelEnabled {
		t.Fatal("kernel failure was not reported")
	}
	if result, err := store.Append([]Entry{testEntry(1)}); err != nil || result.Accepted != 1 {
		t.Fatalf("unavailable kernel logs stopped module logs: %+v %v", result, err)
	}
}
