//go:build linux

package statefs

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadPrivate reads one singly-linked owner-only 0400/0600 regular file. Missing
// files remain ENOENT; FIFO/device/symlink inputs cannot block before fstat.
func ReadPrivate(ctx context.Context, dir *os.File, name string, maximum int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00\r\n") || maximum < 1 || maximum > 1<<30 {
		return nil, errors.New("invalid private state read bound or filename")
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "private-state")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || (st.Mode&07777 != 0400 && st.Mode&07777 != 0600) || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || st.Size <= 0 || st.Size > maximum {
		return nil, errors.New("private state must be an owned 0400/0600 singly-linked bounded nonempty regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: f}, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		clear(raw)
		if err != nil {
			return nil, err
		}
		return nil, errors.New("private state exceeds its read bound")
	}
	if err := ctx.Err(); err != nil {
		clear(raw)
		return nil, err
	}
	return raw, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
