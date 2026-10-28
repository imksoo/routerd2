// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type leaseEvent struct {
	Action   string    `json:"action"`
	MAC      string    `json:"mac"`
	IP       string    `json:"ip"`
	Hostname string    `json:"hostname,omitempty"`
	At       time.Time `json:"at"`
	FromFile bool      `json:"from_file,omitempty"`
}
type leaseEventReply struct {
	Error string `json:"error,omitempty"`
}
type leaseEventCall struct {
	event leaseEvent
	done  chan error
}

// SendLeaseEvent is dnsmasq's direct executable hook, using the existing
// verified Go command. It reports to its exact engine lifetime; it owns no DB.
func SendLeaseEvent(args []string) error {
	if len(args) < 3 {
		return errors.New("DHCP event requires action, MAC and address")
	}
	event := leaseEvent{Action: args[0], MAC: args[1], IP: args[2], At: time.Now().UTC(), FromFile: os.Getenv("DNSMASQ_DATA_MISSING") == "1"}
	if len(args) > 3 && args[3] != "*" {
		event.Hostname = args[3]
	}
	conn, err := net.DialTimeout("unix", os.Getenv("ROUTERD_DHCP_EVENT_SOCKET"), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(event); err != nil {
		return err
	}
	var reply leaseEventReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return nil
}

type leaseEvents struct {
	listener net.Listener
	dir      *os.File
	name     string
	requests chan leaseEventCall
	cancel   context.CancelFunc
	done     chan struct{}
	err      error // written before done closes
}

func listenLeaseEvents(ctx context.Context, cfg Config) (*leaseEvents, error) {
	dir, err := os.Open(filepath.Dir(cfg.StatePath))
	if err != nil {
		return nil, err
	}
	var epoch [12]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		dir.Close()
		return nil, err
	}
	name := "events-" + hex.EncodeToString(epoch[:]) + ".sock"
	// The pinned directory keeps the bind path below sockaddr_un's limit.
	// The engine sees the same inode at /state/<name> in its private root.
	listener, err := net.Listen("unix", "/proc/self/fd/"+strconv.Itoa(int(dir.Fd()))+"/"+name)
	if err != nil {
		dir.Close()
		return nil, err
	}
	// The worker closes this listener after draining the engine's children.
	// Parent cancellation starts that drain; it must not discard queued events.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &leaseEvents{listener: listener, dir: dir, name: name, requests: make(chan leaseEventCall), cancel: cancel, done: make(chan struct{})}
	go func() { defer close(r.done); r.err = r.serve(ctx) }()
	return r, nil
}

func (r *leaseEvents) close() {
	r.cancel()
	r.listener.Close()
	<-r.done
	r.dir.Close()
}

func (r *leaseEvents) serve(ctx context.Context) error {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return err
		}
		r.receive(ctx, conn)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (r *leaseEvents) receive(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	err := conn.SetDeadline(time.Now().Add(6 * time.Second))
	var event leaseEvent
	if err == nil {
		err = json.NewDecoder(io.LimitReader(conn, 2048)).Decode(&event)
	}
	if err == nil {
		call := leaseEventCall{event: event, done: make(chan error, 1)}
		select {
		case r.requests <- call:
		case <-ctx.Done():
			return
		}
		select {
		case err = <-call.done:
		case <-ctx.Done():
			return
		}
	}
	reply := leaseEventReply{}
	if err != nil {
		reply.Error = err.Error()
	}
	_ = json.NewEncoder(conn).Encode(reply)
}
