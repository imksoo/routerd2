//go:build linux

package network

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openNetworkJournalParent(path string, create bool) (*os.File, string, error) {
	if !secretFilePath(path) || path == "/" {
		return nil, "", errors.New("network journal needs a clean absolute file path")
	}
	dir, err := openProtectedNetworkDirectory(filepath.Dir(path), create)
	if err != nil {
		return nil, "", err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(dir.Fd()), &st); err == nil && (st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0) {
		err = errors.New("network journal directory must be worker-owned without group/other writes")
	}
	if err != nil {
		dir.Close()
		return nil, "", err
	}
	return dir, filepath.Base(path), nil
}

func openNetworkJournalFile(dir *os.File, name string, lock bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if lock {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err == nil {
		err = checkNetworkJournalFile(st)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func checkNetworkJournalFile(st unix.Stat_t) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		return errors.New("network journal/lock must be an owned 0600 singly-linked regular file")
	}
	return nil
}

func writeNetworkJournal(dir *os.File, name string, raw []byte) (bool, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if err := checkNetworkJournalFile(st); err != nil {
			return false, err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return false, err
	}
	var nonce [16]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return false, err
	}
	temporary := ".network-state-" + hex.EncodeToString(nonce[:])
	fd, err := unix.Openat(int(dir.Fd()), temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), temporary)
	defer f.Close()
	defer unix.Unlinkat(int(dir.Fd()), temporary, 0)
	n, err := f.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return false, err
	}
	if err := unix.Renameat(int(dir.Fd()), temporary, int(dir.Fd()), name); err != nil {
		return false, err
	}
	return true, dir.Sync()
}
