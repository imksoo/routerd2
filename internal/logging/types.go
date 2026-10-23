// Package logging provides bounded, asynchronous local log delivery and a
// private structured-log service. An ingest acknowledgement means accepted in
// memory, not fsynced: diagnostics must never stall the router's control loops.
package logging

import (
	"context"
	"io"
	"log/slog"
	"time"
)

const (
	SocketName     = "logging.sock"
	MaxEntryBytes  = 7 << 10
	MaxRecordBytes = 8 << 10
	MaxBatch       = 32
	MaxQuery       = 200
)

type Options struct {
	RuntimeDir    string
	StateDir      string
	ReleaseID     string
	MaxFileBytes  int64
	MaxFiles      int
	MemoryEntries int
	DiskQueueSize int
	SyncInterval  time.Duration
	// CaptureKernel reads /dev/kmsg without consuming other readers' records.
	// Enable it only in appliance mode; host/container logs belong to the host.
	CaptureKernel bool
}

// Entry is supplied by a producer. ProducerSequence counts attempted records,
// including locally dropped ones, so later delivery can reveal a missing range.
// Callers must never put credentials, command arguments, or configuration
// contents into Message. Sensitive attribute names are redacted defensively.
type Entry struct {
	Timestamp        time.Time      `json:"timestamp"`
	Module           string         `json:"module"`
	PID              int            `json:"pid"`
	Generation       uint64         `json:"generation"`
	Level            string         `json:"level"`
	Message          string         `json:"message"`
	Attributes       map[string]any `json:"attributes,omitempty"`
	ProducerEpoch    string         `json:"producer_epoch"`
	ProducerSequence uint64         `json:"producer_sequence"`
	ProducerDropped  uint64         `json:"producer_dropped,omitempty"`
}

type Record struct {
	Entry
	Version     int    `json:"version"`
	Epoch       string `json:"epoch"`
	Sequence    uint64 `json:"sequence"`
	ProducerGap uint64 `json:"producer_gap,omitempty"`
}

type Cursor struct {
	Epoch    string `json:"epoch"`
	Sequence uint64 `json:"sequence"`
}

type IngestRequest struct {
	Entries []Entry `json:"entries"`
}

type IngestResult struct {
	Accepted   int    `json:"accepted"`
	Duplicates int    `json:"duplicates"`
	Cursor     Cursor `json:"cursor"`
	Degraded   bool   `json:"degraded"`
}

type Query struct {
	Cursor   Cursor
	Limit    int
	Module   string
	MinLevel string
	Tail     bool
}

type QueryResult struct {
	Records        []Record `json:"records"`
	Cursor         Cursor   `json:"cursor"`
	More           bool     `json:"more"`
	Gap            bool     `json:"gap"`
	Reset          bool     `json:"reset"`
	GapReasons     []string `json:"gap_reasons,omitempty"`
	OldestSequence uint64   `json:"oldest_sequence"`
	LatestSequence uint64   `json:"latest_sequence"`
	Status         Status   `json:"status"`
}

type Status struct {
	Epoch                string     `json:"epoch"`
	ReleaseID            string     `json:"release_id"`
	LatestSequence       uint64     `json:"latest_sequence"`
	OldestSequence       uint64     `json:"oldest_sequence"`
	MemoryEntries        int        `json:"memory_entries"`
	MemoryLimit          int        `json:"memory_limit"`
	MemoryEvicted        uint64     `json:"memory_evicted"`
	Accepted             uint64     `json:"accepted"`
	Rejected             uint64     `json:"rejected"`
	ProducerGaps         uint64     `json:"producer_gaps"`
	ProducerStateEvicted uint64     `json:"producer_state_evicted"`
	DiskEnabled          bool       `json:"disk_enabled"`
	DiskDegraded         bool       `json:"disk_degraded"`
	DiskRecoveryRequired bool       `json:"disk_recovery_required"`
	DiskDropped          uint64     `json:"disk_dropped"`
	DiskWritten          uint64     `json:"disk_written"`
	DiskErrors           uint64     `json:"disk_errors"`
	CorruptRecords       uint64     `json:"corrupt_records"`
	RecoveredGaps        uint64     `json:"recovered_sequence_gaps"`
	DiskQueueDepth       int        `json:"disk_queue_depth"`
	DiskQueueLimit       int        `json:"disk_queue_limit"`
	CurrentFileBytes     int64      `json:"current_file_bytes"`
	LastDiskError        string     `json:"last_disk_error,omitempty"`
	LastDiskSuccess      *time.Time `json:"last_disk_success,omitempty"`
	KernelEnabled        bool       `json:"kernel_enabled"`
	KernelReceived       uint64     `json:"kernel_received"`
	KernelRejected       uint64     `json:"kernel_rejected"`
	KernelOverruns       uint64     `json:"kernel_overruns"`
	LastKernelError      string     `json:"last_kernel_error,omitempty"`
}

type HandlerOptions struct {
	RuntimeDir string
	Module     string
	PID        int
	Generation uint64
	QueueSize  int
	Timeout    time.Duration
	Level      slog.Leveler
	// Fallback runs only on the delivery goroutine. A blocked console can stall
	// that goroutine, but never Handle; its bounded queue then drops visibly.
	// nil disables fallback. Close remains bounded by its supplied context.
	Fallback io.Writer
}

type HandlerStats struct {
	ProducerEpoch   string `json:"producer_epoch"`
	Produced        uint64 `json:"produced"`
	Enqueued        uint64 `json:"enqueued"`
	Delivered       uint64 `json:"delivered"`
	QueueDropped    uint64 `json:"queue_dropped"`
	Rejected        uint64 `json:"rejected"`
	DeliveryDropped uint64 `json:"delivery_dropped"`
	FallbackWritten uint64 `json:"fallback_written"`
	FallbackErrors  uint64 `json:"fallback_errors"`
	QueueDepth      int    `json:"queue_depth"`
	QueueLimit      int    `json:"queue_limit"`
}

type deliveryClient interface {
	Do(context.Context, string, string, any, any) error
}
