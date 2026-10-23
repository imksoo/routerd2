//go:build linux

package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The kernel ABI describes one complete record per read and independent read
// positions for each open file. EPIPE indicates overwritten records; it does
// not mean that this reader should clear the kernel ring or reopen the device.
// See https://www.kernel.org/doc/Documentation/ABI/testing/dev-kmsg .
type kernelReader interface {
	Read([]byte) (int, error)
	Wait(context.Context) error
	Close() error
}

type nativeKernelReader struct{ fd int }

func (r *nativeKernelReader) Read(b []byte) (int, error) { return unix.Read(r.fd, b) }
func (r *nativeKernelReader) Close() error               { return unix.Close(r.fd) }
func (r *nativeKernelReader) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 200)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			if fds[0].Revents&unix.POLLNVAL != 0 {
				return errors.New("kernel log descriptor is no longer valid")
			}
			return nil // Read handles data, EPIPE, and terminal errors.
		}
	}
}

func captureKernel(ctx context.Context, store *Store) {
	fd, err := unix.Open("/dev/kmsg", unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		store.kernelError(fmt.Errorf("open kernel log: %w", err))
		return
	}
	reader := &nativeKernelReader{fd: fd}
	defer reader.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFCHR {
		store.kernelError(errors.New("kernel log path is not a character device"))
		return
	}
	epoch, err := kernelEpoch()
	if err != nil {
		store.kernelError(err)
		return
	}
	readKernel(ctx, store, reader, epoch, os.Getpid(), kernelWallTime)
}

// A stable boot-specific identity allows the store's producer receipt to skip
// records replayed after a logging-worker restart. A new kernel boot receives
// an independent producer epoch, even if its sequence starts at zero again.
func kernelEpoch() (string, error) {
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("read kernel boot identity: %w", err)
	}
	identity := strings.TrimSpace(string(bootID))
	if len(identity) != 36 {
		return "", errors.New("kernel boot identity has an unexpected format")
	}
	key := sha256.Sum256([]byte("routerd2/kernel/" + identity))
	return hex.EncodeToString(key[:16]), nil
}

// The original monotonic timestamp is retained as an attribute. The wall
// timestamp is an estimate using the current clock offset and can be adjusted
// by NTP; ordering and loss detection always use the kernel sequence instead.
func kernelWallTime(microseconds uint64) time.Time {
	now := time.Now().UTC()
	var uptime unix.Timespec
	if unix.ClockGettime(unix.CLOCK_MONOTONIC, &uptime) != nil || microseconds > uint64(^uint64(0)>>1)/1000 {
		return now
	}
	return now.Add(time.Duration(int64(microseconds)*1000 - uptime.Nano()))
}

func readKernel(ctx context.Context, store *Store, reader kernelReader, epoch string, pid int, stamp func(uint64) time.Time) {
	store.mu.Lock()
	store.status.KernelEnabled = true
	store.mu.Unlock()
	defer func() {
		store.mu.Lock()
		store.status.KernelEnabled = false
		store.mu.Unlock()
	}()
	buffer := make([]byte, 64<<10)
	for ctx.Err() == nil {
		n, err := reader.Read(buffer)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			if err := reader.Wait(ctx); err != nil {
				if ctx.Err() == nil {
					store.kernelError(err)
				}
				return
			}
			continue
		}
		if errors.Is(err, unix.EPIPE) {
			store.mu.Lock()
			store.status.KernelOverruns++
			store.mu.Unlock()
			continue
		}
		if err != nil || n == 0 {
			if err == nil {
				err = io.EOF
			}
			store.kernelError(fmt.Errorf("read kernel log: %w", err))
			return
		}
		entry, err := parseKernelRecord(buffer[:n], epoch, pid, stamp)
		if err == nil {
			_, err = store.Append([]Entry{entry})
		}
		store.mu.Lock()
		if err != nil {
			store.status.KernelRejected++
			store.status.LastKernelError = boundedString(err.Error(), 1024)
		} else {
			store.status.KernelReceived++
		}
		store.mu.Unlock()
	}
}

func (s *Store) kernelError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.KernelEnabled = false
	s.status.LastKernelError = boundedString(err.Error(), 1024)
}

func parseKernelRecord(record []byte, epoch string, pid int, stamp func(uint64) time.Time) (Entry, error) {
	header, body, ok := strings.Cut(string(record), ";")
	if !ok {
		return Entry{}, errors.New("kernel log record is missing its header")
	}
	fields := strings.Split(header, ",")
	if len(fields) < 4 {
		return Entry{}, errors.New("kernel log record has an incomplete header")
	}
	priority, err := strconv.ParseUint(fields[0], 10, 16)
	if err != nil {
		return Entry{}, errors.New("kernel log priority is invalid")
	}
	sequence, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || sequence == ^uint64(0) {
		return Entry{}, errors.New("kernel log sequence is invalid")
	}
	micros, err := strconv.ParseUint(fields[2], 10, 64)
	if err != nil {
		return Entry{}, errors.New("kernel log timestamp is invalid")
	}
	message, extra, _ := strings.Cut(strings.TrimSuffix(body, "\n"), "\n")
	level := "ERROR"
	switch priority & 7 {
	case 4:
		level = "WARN"
	case 5, 6:
		level = "INFO"
	case 7:
		level = "DEBUG"
	}
	attrs := map[string]any{
		"kernel_sequence": sequence, "kernel_monotonic_us": micros,
		"syslog_facility": priority >> 3, "syslog_priority": priority & 7,
		"kernel_flags": boundedString(fields[3], 32),
	}
	for _, line := range strings.Split(extra, "\n") {
		if !strings.HasPrefix(line, " ") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, " "), "=")
		// Device identity is useful and bounded. Arbitrary injected metadata
		// must not silently become trusted, unredacted structured attributes.
		if ok && (key == "SUBSYSTEM" || key == "DEVICE") {
			attrs["kernel_"+strings.ToLower(key)] = boundedString(value, 256)
		}
	}
	// Preserve the kernel's \xNN escapes. Decoding them would introduce terminal
	// controls or ambiguous multiline content in SSH log views.
	return validateEntry(Entry{
		Timestamp: stamp(micros), Module: "kernel", PID: pid, Level: level,
		Message: boundedString(message, 4096), Attributes: attrs,
		ProducerEpoch: epoch, ProducerSequence: sequence + 1,
	})
}
