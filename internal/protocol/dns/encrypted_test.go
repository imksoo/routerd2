// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	mdns "github.com/miekg/dns"
	"github.com/quic-go/quic-go"
)

func fixtureTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := server.TLS.Certificates[0]
	root := server.Certificate()
	server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, roots
}

func TestDNSOverTLSNativeExchange(t *testing.T) {
	serverTLS, roots := fixtureTLS(t)
	l, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	defer l.Close()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			c, err := l.Accept()
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				return err
			}
			var length uint16
			if err := binary.Read(c, binary.BigEndian, &length); err != nil {
				return err
			}
			wire := make([]byte, int(length))
			if _, err := io.ReadFull(c, wire); err != nil {
				return err
			}
			var query mdns.Msg
			if err := query.Unpack(wire); err != nil {
				return err
			}
			response, err := failoverSuccessfulReply(&query).Pack()
			if err != nil {
				return err
			}
			if err := binary.Write(c, binary.BigEndian, uint16(len(response))); err != nil {
				return err
			}
			_, err = c.Write(response)
			return err
		}()
	}()
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{Upstreams: []string{"tls://" + l.Addr().String()}}, upstreamPoolConfig{RootCAs: roots, QueryTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	q := new(mdns.Msg)
	q.SetQuestion("tls.example.", mdns.TypeA)
	wire, _ := q.Pack()
	reply, err := pool.Exchange(context.Background(), wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateUpstreamResponse(wire, reply); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestDNSOverQUICZeroIDRequestFINAndResponseFIN(t *testing.T) {
	serverTLS, roots := fixtureTLS(t)
	serverTLS.MinVersion = tls.VersionTLS13
	serverTLS.NextProtos = []string{"doq"}
	l, err := quic.ListenAddr("127.0.0.1:0", serverTLS, &quic.Config{})
	if err != nil {
		skipIfListenNotPermitted(t, err)
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			conn, err := l.Accept(ctx)
			if err != nil {
				return err
			}
			defer func() { <-ctx.Done(); _ = conn.CloseWithError(0, "") }()
			stream, err := conn.AcceptStream(ctx)
			if err != nil {
				return err
			}
			// Deliberately wait for request FIN before parsing or replying. A client
			// which only sends FIN after reading the response deadlocks this test.
			frame, err := io.ReadAll(io.LimitReader(stream, maxDNSMessageSize+3))
			if err != nil {
				return err
			}
			if len(frame) < 14 || len(frame) != int(binary.BigEndian.Uint16(frame[:2]))+2 {
				return errors.New("invalid DoQ request framing")
			}
			var query mdns.Msg
			if err := query.Unpack(frame[2:]); err != nil {
				return err
			}
			if query.Id != 0 {
				return fmt.Errorf("DoQ request ID = %d", query.Id)
			}
			response, err := failoverSuccessfulReply(&query).Pack()
			if err != nil {
				return err
			}
			if err := binary.Write(stream, binary.BigEndian, uint16(len(response))); err != nil {
				return err
			}
			if _, err := stream.Write(response); err != nil {
				return err
			}
			return stream.Close()
		}()
	}()
	pool, err := newUpstreamPool(api.DNSResolverSourceSpec{Upstreams: []string{"quic://" + l.Addr().String()}}, upstreamPoolConfig{RootCAs: roots, QueryTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	q := new(mdns.Msg)
	q.SetQuestion("quic.example.", mdns.TypeA)
	q.Id = 12345
	wire, _ := q.Pack()
	reply, err := pool.Exchange(ctx, wire, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateUpstreamResponse(wire, reply); err != nil {
		t.Fatal(err)
	}
	if q.Id != 12345 {
		t.Fatal("DoQ changed caller query ID")
	}
	cancel()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
