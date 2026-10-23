package management

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"golang.org/x/crypto/ssh"
)

func key(t *testing.T) ssh.Signer {
	t.Helper()
	_, p, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ssh.NewSignerFromKey(p)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

func TestHostKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "host-key")
	a, err := HostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HostKey(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.PublicKey().Marshal(), b.PublicKey().Marshal()) {
		t.Fatal("host identity changed")
	}
}

func TestSSHAuthTypedExecAndForwardingRejection(t *testing.T) {
	clientKey, host := key(t), key(t)
	cfg, err := ServerConfig(runtimeconfig.SSH{User: "admin", AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(clientKey.PublicKey()))}}, host)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go handle(ctx, c, cfg, cli.Session{})
		}
	}()
	clientConfig := func(k ssh.Signer) *ssh.ClientConfig {
		return &ssh.ClientConfig{User: "admin", Auth: []ssh.AuthMethod{ssh.PublicKeys(k)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 3 * time.Second}
	}
	if c, err := ssh.Dial("tcp", l.Addr().String(), clientConfig(key(t))); err == nil {
		c.Close()
		t.Fatal("unauthorized key accepted")
	}
	c, err := ssh.Dial("tcp", l.Addr().String(), clientConfig(clientKey))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Output("show version")
	s.Close()
	if err != nil || !bytes.Contains(out, []byte(`"release_id"`)) {
		t.Fatalf("typed exec: %q %v", out, err)
	}
	s, err = c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Output("sh -c id"); err == nil {
		t.Fatal("accepted shell escape")
	}
	s.Close()
	if ch, _, err := c.OpenChannel("direct-tcpip", nil); err == nil {
		ch.Close()
		t.Fatal("accepted forwarding")
	}
}
