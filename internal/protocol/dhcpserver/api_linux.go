// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type observation struct {
	mu            sync.RWMutex
	epoch         string
	latest        Snapshot
	events        []Event
	eventSequence uint64
}

func (o *observation) setSnapshot(s Snapshot) { o.mu.Lock(); defer o.mu.Unlock(); o.latest = s }
func (o *observation) addEvent(event Event) Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.eventSequence++
	event.Sequence = o.eventSequence
	event.Epoch = o.epoch
	o.events = append(o.events, event)
	if len(o.events) > 256 {
		copy(o.events, o.events[len(o.events)-256:])
		o.events = o.events[:256]
	}
	return event
}
func (o *observation) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		o.mu.RLock()
		latest := o.latest
		o.mu.RUnlock()
		writeJSON(w, latest)
	})
	mux.HandleFunc("/v1/leases", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "lease mutation requires a committed service change", 405)
			return
		}
		o.mu.RLock()
		latest := o.latest
		o.mu.RUnlock()
		writeJSON(w, latest)
	})
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		var after uint64
		if value := r.URL.Query().Get("after"); value != "" {
			var err error
			after, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				http.Error(w, "invalid event cursor", 400)
				return
			}
		}
		o.mu.RLock()
		gap := r.URL.Query().Get("epoch") != "" && r.URL.Query().Get("epoch") != o.epoch
		if gap {
			after = 0
		}
		if len(o.events) > 0 && after < o.events[0].Sequence-1 {
			gap = true
		}
		events := []Event{}
		for _, e := range o.events {
			if e.Sequence > after {
				events = append(events, e)
			}
		}
		result := struct {
			Epoch    string  `json:"epoch"`
			Sequence uint64  `json:"sequence"`
			Events   []Event `json:"events"`
			Gap      bool    `json:"gap"`
		}{o.epoch, o.eventSequence, events, gap}
		o.mu.RUnlock()
		writeJSON(w, result)
	})
	return mux
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func listenPrivate(path string) (net.Listener, func(), error) {
	if err := privateParent(path); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, errors.New("DHCP private API path is not a socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, nil, errors.New("DHCP private API socket is already live")
		}
		if !errors.Is(dialErr, unix.ECONNREFUSED) {
			return nil, nil, dialErr
		}
		if err := os.Remove(path); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	l.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		_ = os.Remove(path)
		return nil, nil, err
	}
	owned, err := os.Lstat(path)
	if err != nil {
		_ = l.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = l.Close()
		if current, err := os.Lstat(path); err == nil && os.SameFile(owned, current) {
			_ = os.Remove(path)
		}
	}
	return l, cleanup, nil
}
func appendEvent(path string, event Event) error {
	b, err := json.Marshal(event)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("DHCP event path must be a private regular file")
		}
		if info.Size()+int64(len(b)) > 1<<20 {
			if old, err := os.Lstat(path + ".1"); err == nil {
				if !old.Mode().IsRegular() || old.Mode().Perm()&0077 != 0 {
					return errors.New("DHCP event archive must be a private regular file")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("DHCP event file must be private and regular")
	}
	_, err = f.Write(b)
	return err
}
