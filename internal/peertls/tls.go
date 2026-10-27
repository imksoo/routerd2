// SPDX-License-Identifier: BSD-3-Clause

// Package peertls loads the configured identities of a two-node state link.
package peertls

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd2/internal/statefs"
)

type Config struct {
	ListenAddress       netip.AddrPort `json:"listen_address"`
	PeerAddress         netip.AddrPort `json:"peer_address"`
	CertificateFile     string         `json:"certificate_file"`
	PrivateKeyFile      string         `json:"private_key_file"`
	PeerCertificateFile string         `json:"peer_certificate_file"`
}

func Validate(c Config) error {
	for _, endpoint := range []netip.AddrPort{c.ListenAddress, c.PeerAddress} {
		if !endpoint.IsValid() || endpoint.Port() == 0 || endpoint.Addr().IsUnspecified() || endpoint.Addr().IsMulticast() {
			return errors.New("state replication requires explicit local and peer unicast endpoints")
		}
	}
	if c.ListenAddress == c.PeerAddress {
		return errors.New("state replication endpoints must differ")
	}
	for _, p := range []string{c.CertificateFile, c.PrivateKeyFile, c.PeerCertificateFile} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsRune(p, 0) {
			return errors.New("state replication credentials require absolute file paths")
		}
	}
	return nil
}

func Load(ctx context.Context, c Config) (server, client *tls.Config, err error) {
	dir, err := os.Open(filepath.Dir(c.PrivateKeyFile))
	if err != nil {
		return nil, nil, err
	}
	defer dir.Close()
	key, err := statefs.ReadPrivate(ctx, dir, filepath.Base(c.PrivateKeyFile), 64<<10)
	if err != nil {
		return nil, nil, err
	}
	cert, err := os.ReadFile(c.CertificateFile)
	if err != nil {
		return nil, nil, err
	}
	identity, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, nil, err
	}
	peerPEM, err := os.ReadFile(c.PeerCertificateFile)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(peerPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("state replication requires the peer certificate")
	}
	peer, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	trusted := x509.NewCertPool()
	trusted.AddCert(peer)
	verify := func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, peer.Raw) {
			return errors.New("state replication peer certificate does not match")
		}
		return nil
	}
	server = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: trusted, VerifyConnection: verify}
	client = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, RootCAs: trusted, ServerName: c.PeerAddress.Addr().WithZone("").String(), VerifyConnection: verify}
	return server, client, nil
}
