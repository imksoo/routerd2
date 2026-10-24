//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/control"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"golang.org/x/sys/unix"
)

func eventCommand(args []string, out io.Writer) error {
	usage := errors.New("usage: routerd events export --output FILE [--runtime-dir DIR] [--timeout 2m] | routerd events verify FILE")
	if len(args) == 0 {
		return usage
	}
	if args[0] == "verify" {
		if len(args) != 2 {
			return usage
		}
		fd, err := unix.Open(args[1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), args[1])
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return errors.New("archive must be a regular file")
		}
		result, err := events.VerifyHistory(context.Background(), f)
		if err != nil {
			return err
		}
		return cli.WriteJSON(out, result)
	}
	if args[0] != "export" {
		return usage
	}
	f := flags("events export")
	path := f.String("output", "", "new private history archive file")
	dir := f.String("runtime-dir", "/run/routerd", "private runtime directory")
	timeout := f.Duration("timeout", 2*time.Minute, "export deadline")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *path == "" || *timeout <= 0 || *timeout > time.Hour {
		return usage
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := exportEventHistory(ctx, localapi.NewClient(filepath.Join(*dir, control.SocketName)), *path)
	if err != nil {
		return err
	}
	return cli.WriteJSON(out, result)
}

func exportEventHistory(ctx context.Context, client *localapi.Client, path string) (events.ArchiveSummary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return events.ArchiveSummary{}, err
	}
	base := filepath.Base(abs)
	if base == "." || base == "/" {
		return events.ArchiveSummary{}, errors.New("output must name a new file")
	}
	fd, err := unix.Open(filepath.Dir(abs), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return events.ArchiveSummary{}, err
	}
	dir := os.NewFile(uintptr(fd), filepath.Dir(abs))
	defer dir.Close()
	var named unix.Stat_t
	if err = unix.Fstatat(fd, base, &named, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return events.ArchiveSummary{}, fmt.Errorf("archive output exists: %w", os.ErrExist)
	} else if !errors.Is(err, unix.ENOENT) {
		return events.ArchiveSummary{}, err
	}
	var status control.EventStoreHealth
	if err = client.Do(ctx, "GET", "/v1/event-store", nil, &status); err != nil {
		return events.ArchiveSummary{}, err
	}
	if status.Capacity == nil {
		return events.ArchiveSummary{}, errors.New("journal snapshot capacity is missing")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return events.ArchiveSummary{}, err
	}
	temporary := ".routerd-history-" + hex.EncodeToString(nonce[:])
	target, err := unix.Openat(fd, temporary, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return events.ArchiveSummary{}, err
	}
	file := os.NewFile(uintptr(target), temporary)
	defer file.Close()
	defer unix.Unlinkat(fd, temporary, 0)
	result, err := events.ExportHistory(ctx, file, status.Capacity.HistoryHeader(), func(ctx context.Context, after uint64, limit int) ([]events.Record, error) {
		var page control.ObservationPage
		err := client.Do(ctx, "GET", "/v1/observations?after_cursor="+strconv.FormatUint(after, 10)+"&limit="+strconv.Itoa(limit), nil, &page)
		return page.Records, err
	})
	if err != nil {
		return events.ArchiveSummary{}, err
	}
	if err = errors.Join(file.Sync(), file.Close()); err != nil {
		return events.ArchiveSummary{}, err
	}
	if err = ctx.Err(); err != nil {
		return events.ArchiveSummary{}, err
	}
	// The complete file is atomically published without replacing a file,
	// directory or symlink created concurrently. The directory is pinned.
	if err = unix.Renameat2(fd, temporary, fd, base, unix.RENAME_NOREPLACE); err != nil {
		return events.ArchiveSummary{}, err
	}
	if err = dir.Sync(); err != nil {
		return result, fmt.Errorf("archive published but directory sync failed: %w", err)
	}
	return result, nil
}
