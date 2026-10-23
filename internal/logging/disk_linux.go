//go:build linux

package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var errStoreInUse = errors.New("log directory already has an active writer")

type diskFile struct {
	dir   string
	file  *os.File
	lock  *os.File
	size  int64
	limit int64
	files int
}

func logPath(dir string, index int) string {
	return filepath.Join(dir, "log."+strconv.Itoa(index)+".ndjson")
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err == nil {
			err = errors.New("log path is not a regular file")
		}
		return nil, err
	}
	return f, nil
}

func openDisk(o Options, recoverTail bool) (diskSink, []Record, uint64, error) {
	dir := filepath.Join(o.StateDir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, 0, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, 0, errors.New("log directory must be a real directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return nil, nil, 0, errors.New("log directory must be owned by this user")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, nil, 0, err
	}
	lock, err := openPrivate(filepath.Join(dir, "writer.lock"), unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, nil, 0, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, nil, 0, errStoreInUse
		}
		return nil, nil, 0, err
	}
	disk := &diskFile{dir: dir, lock: lock, limit: o.MaxFileBytes, files: o.MaxFiles}
	var recovered []Record
	var corrupt uint64
	if err = disk.prune(); err == nil && recoverTail {
		recovered, corrupt, err = disk.recover(o.MemoryEntries)
	} else if err == nil {
		// A previous write may have ended halfway through a record. Repair the
		// active file before appending, even when replay is unnecessary.
		_, corrupt, err = disk.readFile(0, 0)
	}
	if err == nil {
		err = disk.openCurrent()
	}
	if err != nil {
		_ = disk.Close()
		return nil, recovered, corrupt, err
	}
	return disk, recovered, corrupt, nil
}

func (d *diskFile) prune() error {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "log.") || !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "log."), ".ndjson"))
		if err != nil || name != "log."+strconv.Itoa(index)+".ndjson" || index < d.files || index >= 32 {
			continue
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected non-regular retained log path")
		}
		if err := os.Remove(filepath.Join(d.dir, name)); err != nil {
			return err
		}
	}
	return nil
}

func (d *diskFile) openCurrent() error {
	f, err := openPrivate(logPath(d.dir, 0), unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT)
	if err != nil {
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	d.file, d.size = f, info.Size()
	return nil
}

func (d *diskFile) recover(limit int) ([]Record, uint64, error) {
	var all []Record
	var corrupt uint64
	for i := d.files - 1; i >= 0; i-- {
		records, bad, err := d.readFile(i, limit)
		corrupt += bad
		if err != nil {
			return all, corrupt, err
		}
		all = append(all, records...)
		if len(all) > limit {
			all = append([]Record(nil), all[len(all)-limit:]...)
		}
	}
	return all, corrupt, nil
}

func (d *diskFile) readFile(index, keep int) ([]Record, uint64, error) {
	flags := unix.O_RDONLY
	if index == 0 {
		flags = unix.O_RDWR
	}
	f, err := openPrivate(logPath(d.dir, index), flags)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENOENT) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	// Do not scan arbitrary unbounded files after an operator changes retention.
	// An oversized old file yields memory-only degraded service, visibly.
	if info.Size() > d.limit+MaxRecordBytes {
		return nil, 0, errors.New("existing log file exceeds the configured recovery size limit")
	}
	reader := bufio.NewReaderSize(f, MaxRecordBytes+1)
	var records []Record
	start := 0
	ordered := func() []Record {
		if start == 0 {
			return records
		}
		result := append([]Record(nil), records[start:]...)
		return append(result, records[:start]...)
	}
	var complete int64
	var corrupt uint64
	for {
		line, readErr := reader.ReadSlice('\n')
		if errors.Is(readErr, bufio.ErrBufferFull) {
			return ordered(), corrupt + 1, errors.New("oversized record in retained log file")
		}
		if errors.Is(readErr, io.EOF) {
			if len(line) > 0 {
				corrupt++
				if index == 0 {
					if err := f.Truncate(complete); err != nil {
						return ordered(), corrupt, err
					}
					if err := f.Sync(); err != nil {
						return ordered(), corrupt, err
					}
				}
			}
			return ordered(), corrupt, nil
		}
		if readErr != nil {
			return ordered(), corrupt, readErr
		}
		complete += int64(len(line))
		var record Record
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil || record.Version != 1 || record.Sequence == 0 || !epochName.MatchString(record.Epoch) {
			corrupt++
			continue
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			corrupt++
			continue
		}
		entry, err := validateEntry(record.Entry)
		if err != nil {
			corrupt++
			continue
		}
		record.Entry = entry
		if keep > 0 {
			if len(records) < keep {
				records = append(records, record)
			} else {
				records[start] = record
				start = (start + 1) % keep
			}
		}
	}
}

func (d *diskFile) rotate() error {
	if err := d.file.Sync(); err != nil {
		return err
	}
	if err := d.file.Close(); err != nil {
		return err
	}
	d.file = nil
	if err := os.Remove(logPath(d.dir, d.files-1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := d.files - 2; i >= 0; i-- {
		if err := os.Rename(logPath(d.dir, i), logPath(d.dir, i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := d.openCurrent(); err != nil {
		return err
	}
	parent, err := os.Open(d.dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}

func (d *diskFile) Append(line []byte) error {
	if int64(len(line)) > d.limit || len(line) > MaxRecordBytes {
		return errors.New("log record exceeds file size limit")
	}
	if d.file == nil {
		return errors.New("log file is not open")
	}
	if d.size > 0 && d.size+int64(len(line)) > d.limit {
		if err := d.rotate(); err != nil {
			return fmt.Errorf("rotate log: %w", err)
		}
	}
	n, err := d.file.Write(line)
	d.size += int64(n)
	if err == nil && n != len(line) {
		err = io.ErrShortWrite
	}
	return err
}

func (d *diskFile) Sync() error {
	if d.file == nil {
		return errors.New("log file is not open")
	}
	return d.file.Sync()
}

func (d *diskFile) Size() int64 { return d.size }

func (d *diskFile) Close() error {
	var errs []error
	if d.file != nil {
		errs = append(errs, d.file.Close())
		d.file = nil
	}
	if d.lock != nil {
		errs = append(errs, unix.Flock(int(d.lock.Fd()), unix.LOCK_UN), d.lock.Close())
		d.lock = nil
	}
	return errors.Join(errs...)
}
