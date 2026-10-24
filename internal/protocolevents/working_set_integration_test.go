//go:build linux && integration

package protocolevents

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/imksoo/routerd2/internal/events"
	"golang.org/x/sys/unix"
)

// Both WALs need working space even though the relay holds one small pending
// observation and the target stays far below its logical retention limits.
// Run this same workload on bounded private tmpfs mounts to distinguish
// filesystem exhaustion from queue/journal payload capacity.
func TestHealthRelayScratchWorkingSet(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	defer func() {
		var stat unix.Statfs_t
		if unix.Statfs(root, &stat) == nil {
			t.Logf("relay scratch total_bytes=%d available_bytes=%d", stat.Blocks*uint64(stat.Bsize), stat.Bavail*uint64(stat.Bsize))
		}
	}()
	local, err := events.Open(events.Options{Directory: filepath.Join(root, "relay")})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	target, err := events.Open(events.Options{Directory: filepath.Join(root, "target")})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	box := NewOutbox(local, "wan")
	for i := 0; i < 400; i++ {
		probe := probeEvent("wan")
		probe.Message = "completed scoped TCP probe"
		probe.Attributes["network.address"] = "192.0.2.1"
		probe.Attributes["network.interface.name"] = "wan0"
		if err = box.Enqueue(ctx, probe); err != nil {
			t.Fatalf("enqueue %d, below logical capacity: %v", i, err)
		}
		if err = box.Flush(ctx, func(ctx context.Context, page []events.Event) error {
			for _, event := range page {
				if _, err := target.Append(ctx, event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("flush %d, below logical capacity: %v", i, err)
		}
	}
	rows, err := target.List(ctx, 0, 256)
	if err != nil || len(rows) != 256 {
		t.Fatal(len(rows), err)
	}
	remaining, err := target.List(ctx, rows[len(rows)-1].Cursor, 256)
	if err != nil || len(remaining) != 144 {
		t.Fatal(len(remaining), err)
	}
	t.Log("400 raw observations durably relayed with only one pending item and retained target deduplication")
}
