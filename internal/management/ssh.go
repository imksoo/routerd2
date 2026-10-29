package management

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/statefs"
	"golang.org/x/crypto/ssh"
)

func ServerConfig(opts runtimeconfig.SSH, signer ssh.Signer) (*ssh.ServerConfig, error) {
	allowed := make([]ssh.PublicKey, 0, len(opts.AuthorizedKeys))
	for i, line := range opts.AuthorizedKeys {
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("authorized key %d: %w", i, err)
		}
		if len(options) > 0 || len(bytes.TrimSpace(rest)) > 0 {
			return nil, fmt.Errorf("authorized key %d: supply one plain public key per entry, without options", i)
		}
		allowed = append(allowed, key)
	}
	if len(allowed) == 0 {
		return nil, errors.New("no authorized SSH keys")
	}
	s := &ssh.ServerConfig{MaxAuthTries: 3, ServerVersion: "SSH-2.0-routerd2", PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != opts.User {
			return nil, errors.New("authentication denied")
		}
		for _, a := range allowed {
			if bytes.Equal(a.Marshal(), key.Marshal()) {
				return &ssh.Permissions{}, nil
			}
		}
		return nil, errors.New("authentication denied")
	}}
	s.AddHostKey(signer)
	return s, nil
}

func Serve(ctx context.Context, opts runtimeconfig.SSH, runtimeDir, stateDir string, ready func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.Listen == "" || len(opts.AuthorizedKeys) == 0 {
		slog.Info("SSH disabled", "reason", "listener and authorized keys are required")
		ready()
		<-ctx.Done()
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := statefs.OpenDirectory(stateDir, true)
	if err != nil {
		return err
	}
	if err := dir.Close(); err != nil {
		return err
	}
	signer, err := hostKey(ctx, filepath.Join(stateDir, HostKeyName), nil)
	if err != nil {
		return err
	}
	cfg, err := ServerConfig(opts, signer)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	defer l.Close()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 32)
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-closed:
		}
	}()
	defer func() { cancel(); wg.Wait() }()
	slog.Info("SSH management listening", "address", l.Addr().String(), "host_key", ssh.FingerprintSHA256(signer.PublicKey()))
	ready()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			handle(ctx, c, cfg, cli.Session{RuntimeDir: runtimeDir})
		}()
	}
}

func handle(ctx context.Context, raw net.Conn, cfg *ssh.ServerConfig, session cli.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer raw.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-done:
		}
	}()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn, chans, reqs, err := ssh.NewServerConn(raw, cfg)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = raw.SetDeadline(time.Time{})
	go func() {
		for r := range reqs {
			_ = r.Reply(r.Type == "keepalive@openssh.com", nil)
		}
	}()
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	active := make(chan struct{}, 4)
	for next := range chans {
		if next.ChannelType() != "session" {
			_ = next.Reject(ssh.UnknownChannelType, "only management sessions are supported")
			continue
		}
		select {
		case active <- struct{}{}:
		default:
			_ = next.Reject(ssh.ResourceShortage, "too many sessions")
			continue
		}
		ch, requests, err := next.Accept()
		if err != nil {
			<-active
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-active }(); serveSession(ctx, ch, requests, session) }()
	}
}

func serveSession(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request, session cli.Session) {
	defer ch.Close()
	for r := range requests {
		switch r.Type {
		case "pty-req":
			// The built-in terminal implements editing itself. No OS PTY or shell is allocated.
			_ = r.Reply(true, nil)
		case "window-change":
			_ = r.Reply(true, nil)
		case "exec":
			var command struct{ Command string }
			if err := ssh.Unmarshal(r.Payload, &command); err != nil || len(command.Command) > 4096 {
				_ = r.Reply(false, nil)
				return
			}
			_ = r.Reply(true, nil)
			go ssh.DiscardRequests(requests)
			err := session.Execute(ctx, command.Command, ch, ch)
			exit(ch, err)
			return
		case "shell":
			if len(r.Payload) != 0 {
				_ = r.Reply(false, nil)
				continue
			}
			_ = r.Reply(true, nil)
			go ssh.DiscardRequests(requests)
			exit(ch, session.Interactive(ctx, ch))
			return
		default:
			_ = r.Reply(false, nil)
		}
	}
}

func exit(ch ssh.Channel, err error) {
	var code uint32
	if err != nil {
		code = 1
		_, _ = fmt.Fprintln(ch.Stderr(), err)
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{code}))
}
