//go:build linux && integration

package integration

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// Capture the storage actually used by a running fixture before cleanup can
// checkpoint WALs or remove files. This is observation, never quota repair.
func fixtureStorageDiagnostics(t *testing.T, root string) {
	t.Helper()
	var disk unix.Statfs_t
	if err := unix.Statfs(root, &disk); err != nil {
		t.Logf("fixture statfs: %v", err)
	} else {
		t.Logf("fixture filesystem type=%d total_bytes=%d available_bytes=%d", disk.Type, disk.Blocks*uint64(disk.Bsize), disk.Bavail*uint64(disk.Bsize))
	}
	type entry struct {
		Path             string
		Bytes, Allocated int64
	}
	files := []entry{}
	var total, allocated int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		e := entry{Path: rel, Bytes: info.Size()}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			e.Allocated = stat.Blocks * 512
		}
		total += e.Bytes
		allocated += e.Allocated
		files = append(files, e)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Bytes > files[j].Bytes })
	t.Logf("fixture file_bytes=%d allocated_bytes=%d largest_files=%v", total, allocated, files[:min(12, len(files))])
	if memory, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(memory), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") || strings.HasPrefix(line, "Shmem:") {
				t.Logf("fixture memory %s", line)
			}
		}
	}
}
