//go:build linux

package config

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Explicit boot selection may not treat a link or shared/foreign file as the
// configuration authority. Pin the journal and snapshots beneath the same
// already checked private directory. Never create/open a FIFO for blocking IO.
func readStartupRecord(dir *os.File) (Committed, error) {
	fd, err := unix.Openat(int(dir.Fd()), "config.json", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return Committed{}, err
	}
	f := os.NewFile(uintptr(fd), "config.json")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Committed{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size <= 0 || st.Size > MaxStoredReadSize {
		return Committed{}, errors.New("startup journal must be an owned private 0600 bounded regular file without links")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxStoredReadSize+1))
	if err != nil {
		return Committed{}, err
	}
	if len(raw) > MaxStoredReadSize {
		return Committed{}, errors.New("startup journal exceeds its byte budget")
	}
	var current Committed
	if err := decodeRecord(raw, &current); err != nil {
		return Committed{}, err
	}
	return current, validateRecord(current)
}
