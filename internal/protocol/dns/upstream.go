// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	resolvercfg "github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

const (
	upstreamHealthy = "Healthy"
	upstreamFailing = "Failing"
	upstreamDown    = "Down"
	upstreamProbing = "Probing"

	maxDNSMessageSize = 65535
)

var errTruncatedUpstreamResponse = errors.New("DNS upstream response is truncated")

type upstreamPoolConfig struct {
	ProbeInterval time.Duration
	ProbeTimeout  time.Duration
	QueryTimeout  time.Duration
	FailThreshold int
	PassThreshold int
	RootCAs       *x509.CertPool // nil uses the platform trust store; injectable for protocol tests.
}

type upstreamPool struct {
	mu            sync.Mutex
	upstreams     []*dnsUpstream
	config        upstreamPoolConfig
	client        *http.Client
	spec          api.DNSResolverSourceSpec
	bootstrap     []bootstrapServer
	resolver      *net.Resolver
	nextBootstrap atomic.Uint64
}

type dnsUpstream struct {
	Provider    string            `json:"provider,omitempty"`
	ValidUntil  lifetime.Deadline `json:"validUntil,omitzero"`
	Index       int               `json:"index"`
	URL         string            `json:"url"`
	Scheme      string            `json:"scheme"`
	Address     string            `json:"address"`
	ServerName  string            `json:"serverName,omitempty"`
	Phase       string            `json:"phase"`
	LastError   string            `json:"lastError,omitempty"`
	LastSuccess time.Time         `json:"lastSuccess,omitempty"`
	LastFailure time.Time         `json:"lastFailure,omitempty"`
	Successes   int               `json:"successes,omitempty"`
	Failures    int               `json:"failures,omitempty"`
}

type bootstrapServer struct {
	Address    string
	Provider   string
	ValidUntil lifetime.Deadline
}

// BOOTTIME is checked after a read too: a guest can suspend while DNS IO waits.
type bootstrapConn struct {
	net.Conn
	until lifetime.Deadline
}

func (c *bootstrapConn) current() error {
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if !c.until.Live(clock) {
		return errors.New("DNS bootstrap observation expired")
	}
	return nil
}

func (c *bootstrapConn) Read(b []byte) (int, error) {
	if err := c.current(); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(b)
	if expired := c.current(); expired != nil {
		return 0, expired
	}
	return n, err
}

func (c *bootstrapConn) Write(b []byte) (int, error) {
	if err := c.current(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

// net.Resolver chooses DNS datagram framing through net.PacketConn. Keep that
// method set when bounding a UDP socket; hiding it adds a TCP length on UDP.
type bootstrapPacketConn struct {
	*bootstrapConn
	packet net.PacketConn
}

func (c *bootstrapPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if err := c.current(); err != nil {
		return 0, nil, err
	}
	n, address, err := c.packet.ReadFrom(b)
	if expired := c.current(); expired != nil {
		return 0, nil, expired
	}
	return n, address, err
}

func (c *bootstrapPacketConn) WriteTo(b []byte, address net.Addr) (int, error) {
	if err := c.current(); err != nil {
		return 0, err
	}
	return c.packet.WriteTo(b, address)
}

func newUpstreamPool(spec api.DNSResolverSourceSpec, cfg upstreamPoolConfig) (*upstreamPool, error) {
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = 15 * time.Second
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = 3 * time.Second
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = cfg.ProbeTimeout
	}
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = 3
	}
	if cfg.PassThreshold <= 0 {
		cfg.PassThreshold = 2
	}
	var upstreams []*dnsUpstream
	for i, value := range spec.Upstreams {
		upstream, err := parseDNSUpstream(i, value)
		if err != nil {
			return nil, err
		}
		upstreams = append(upstreams, upstream)
	}
	p := &upstreamPool{upstreams: upstreams, config: cfg, spec: spec}
	for _, value := range spec.BootstrapResolver {
		address := strings.TrimSpace(value)
		if _, _, err := net.SplitHostPort(address); err != nil {
			address = net.JoinHostPort(address, "53")
		}
		p.bootstrap = append(p.bootstrap, bootstrapServer{Address: address})
	}
	if len(spec.BootstrapResolver) != 0 || len(spec.BootstrapResolverFrom) != 0 {
		p.resolver = &net.Resolver{PreferGo: true, Dial: p.dialBootstrap}
	}
	// HTTP can keep a dial alive after its request expires. Bound the whole dial,
	// including bootstrap resolution, so later queries can resolve afresh.
	p.client = &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: cfg.QueryTimeout, Resolver: p.resolver, Control: interfaceControl(spec.ViaInterface)}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs}, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}
	return p, nil
}

func parseDNSUpstream(index int, raw string) (*dnsUpstream, error) {
	trimmed := resolvercfg.NormalizeUpstream(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("parse DNS upstream %q: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "https", "tls", "quic", "udp", "tcp":
	default:
		return nil, fmt.Errorf("unsupported DNS upstream scheme %q", scheme)
	}
	address := ""
	serverName := parsed.Hostname()
	switch scheme {
	case "https":
		if parsed.Host == "" {
			return nil, fmt.Errorf("DoH upstream %q has no host", raw)
		}
	case "tls", "quic":
		port := parsed.Port()
		if port == "" {
			port = "853"
		}
		address = net.JoinHostPort(parsed.Hostname(), port)
	case "udp", "tcp":
		port := parsed.Port()
		if port == "" {
			port = "53"
		}
		address = net.JoinHostPort(parsed.Hostname(), port)
		serverName = ""
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("DNS upstream %q has no host", raw)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return nil, errors.New("DNS upstream URL cannot contain user information or fragments")
	}
	if override := strings.TrimSpace(parsed.Query().Get("serverName")); override != "" {
		serverName = override
	}
	return &dnsUpstream{Index: index, URL: trimmed, Scheme: scheme, Address: address, ServerName: serverName, Phase: upstreamProbing}, nil
}

// One probe cycle owns its concurrent exchanges and waits for all of them.
// Updating learned endpoints needs no extra per-endpoint background lifecycle.
func (p *upstreamPool) Start(ctx context.Context, publish func(string, string, string, string, map[string]string)) {
	go func() {
		defer p.client.CloseIdleConnections()
		ticker := time.NewTicker(p.config.ProbeInterval)
		defer ticker.Stop()
		for {
			var probes sync.WaitGroup
			for _, upstream := range p.snapshotPointers() {
				probes.Go(func() { p.probe(ctx, upstream, publish) })
			}
			probes.Wait()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// SetLearned preserves health for unchanged endpoints. Only the originating
// protocol deadline changes on a fresh observation; receipt never rebases it.
func (p *upstreamPool) setLearned(sets, bootstrap []dnsconfig.LearnedUpstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var next []*dnsUpstream
	previous := map[string]*dnsUpstream{}
	for _, upstream := range p.upstreams {
		if upstream.Provider == "" {
			next = append(next, upstream)
		} else {
			previous[upstream.Provider+"/"+upstream.URL] = upstream
		}
	}
	for _, set := range sets {
		address := net.JoinHostPort(set.Address.String(), "53")
		raw := "udp://" + address
		upstream := previous[set.Provider+"/"+raw]
		if upstream == nil {
			upstream = &dnsUpstream{URL: raw, Scheme: "udp", Address: address, Provider: set.Provider, Phase: upstreamProbing}
		}
		upstream.Index = len(next)
		upstream.ValidUntil = set.ValidUntil
		next = append(next, upstream)
	}
	p.upstreams = next
	fixed := p.bootstrap[:0]
	for _, server := range p.bootstrap {
		if server.Provider == "" {
			fixed = append(fixed, server)
		}
	}
	for _, server := range bootstrap {
		fixed = append(fixed, bootstrapServer{Address: net.JoinHostPort(server.Address.String(), "53"), Provider: server.Provider, ValidUntil: server.ValidUntil})
	}
	p.bootstrap = fixed
}

func (p *upstreamPool) queryBudget(upstream *dnsUpstream) (time.Duration, error) {
	p.mu.Lock()
	for _, current := range p.upstreams {
		if current == upstream {
			deadline := current.ValidUntil
			p.mu.Unlock()
			if deadline == (lifetime.Deadline{}) {
				return p.config.QueryTimeout, nil
			}
			clock, err := boottime.Read()
			if err != nil {
				return 0, err
			}
			remaining := deadline.Remaining(clock)
			if remaining <= 0 {
				return 0, errors.New("DNS upstream observation expired")
			}
			return min(p.config.QueryTimeout, remaining), nil
		}
	}
	p.mu.Unlock()
	return 0, errors.New("DNS upstream withdrawn")
}

func (p *upstreamPool) probe(ctx context.Context, upstream *dnsUpstream, publish func(string, string, string, string, map[string]string)) {
	budget, err := p.queryBudget(upstream)
	if err != nil {
		return
	}
	queryCtx, cancel := context.WithTimeout(ctx, min(p.config.ProbeTimeout, budget))
	defer cancel()
	_, err = p.exchangeOne(queryCtx, upstream, dnsProbeQuery())
	if ctx.Err() != nil {
		return
	}
	if _, err := p.queryBudget(upstream); err != nil {
		return
	}
	if err != nil {
		phase := p.markFailure(upstream, err)
		if publish != nil && phase == upstreamDown {
			publish("routerd.dns.upstream.down", daemonapi.SeverityWarning, "UpstreamDown", err.Error(), map[string]string{"upstream": upstream.URL, "scheme": upstream.Scheme})
		}
		return
	}
	phase := p.markSuccess(upstream)
	if publish != nil && phase == upstreamHealthy {
		publish("routerd.dns.upstream.healthy", daemonapi.SeverityInfo, "UpstreamHealthy", "DNS upstream is healthy", map[string]string{"upstream": upstream.URL, "scheme": upstream.Scheme})
	}
}

func (p *upstreamPool) Exchange(ctx context.Context, query []byte, publish func(string, string, string, string, map[string]string)) ([]byte, error) {
	var errs []string
	for _, upstream := range p.attemptOrder() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		budget, budgetErr := p.queryBudget(upstream)
		if budgetErr != nil {
			errs = append(errs, budgetErr.Error())
			continue
		}
		queryCtx, cancel := context.WithTimeout(ctx, budget)
		resp, err := p.exchangeOne(queryCtx, upstream, query)
		cancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err == nil {
			if _, err := p.queryBudget(upstream); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			p.markSuccess(upstream)
			return resp, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", upstream.URL, err))
		p.markFailure(upstream, err)
	}
	return nil, fmt.Errorf("all DNS upstreams failed: %s", strings.Join(errs, "; "))
}

func (p *upstreamPool) exchangeOne(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	var resp []byte
	var err error
	switch upstream.Scheme {
	case "https":
		resp, err = p.exchangeDoH(ctx, upstream, query)
	case "tls":
		resp, err = p.exchangeDoT(ctx, upstream, query)
	case "quic":
		resp, err = p.exchangeDoQ(ctx, upstream, query)
	case "udp":
		resp, err = p.exchangeUDP(ctx, upstream, query)
	case "tcp":
		resp, err = p.exchangeTCP(ctx, upstream, query)
	default:
		return nil, fmt.Errorf("unsupported DNS upstream scheme %q", upstream.Scheme)
	}
	if err != nil {
		return nil, err
	}
	if err := validateUpstreamResponse(query, resp); err != nil {
		if upstream.Scheme != "udp" || !errors.Is(err, errTruncatedUpstreamResponse) {
			return nil, err
		}
		// Retry a valid truncated UDP response over TCP to the same server,
		// within the original attempt deadline, before trying another provider.
		resp, err = p.exchangeTCP(ctx, upstream, query)
		if err != nil {
			return nil, fmt.Errorf("retry truncated DNS response over TCP: %w", err)
		}
		if err := validateUpstreamResponse(query, resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func validateUpstreamResponse(query, response []byte) error {
	var req, resp dns.Msg
	if err := req.Unpack(query); err != nil {
		return fmt.Errorf("invalid DNS query: %w", err)
	}
	if err := resp.Unpack(response); err != nil {
		return fmt.Errorf("invalid DNS upstream response: %w", err)
	}
	if !resp.Response || resp.Id != req.Id || resp.Opcode != req.Opcode {
		return fmt.Errorf("DNS upstream response header does not match query")
	}
	if len(req.Question) == 0 || len(resp.Question) != len(req.Question) {
		return fmt.Errorf("DNS upstream response question does not match query")
	}
	for i, question := range req.Question {
		answer := resp.Question[i]
		if !strings.EqualFold(question.Name, answer.Name) || question.Qtype != answer.Qtype || question.Qclass != answer.Qclass {
			return fmt.Errorf("DNS upstream response question does not match query")
		}
	}
	if resp.Truncated {
		return errTruncatedUpstreamResponse
	}
	// NXDOMAIN and NOERROR (including NODATA and filtered answers) are final
	// DNS answers, not evidence that another provider should be queried.
	if resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError {
		return fmt.Errorf("DNS upstream returned failure rcode %s (%d)", dns.RcodeToString[resp.Rcode], resp.Rcode)
	}
	return nil
}

func (p *upstreamPool) exchangeDoH(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	reused := false
	first := req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
	resp, err := p.client.Do(first)
	var transportError *net.OpError
	if err != nil && reused && ctx.Err() == nil && (errors.As(err, &transportError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		// A reused TLS connection can be reset while a DNS query is in flight.
		// Queries are safe to repeat. Rewind once within the original deadline;
		// the HTTP transport discards the broken connection. Fresh-dial failure,
		// HTTP failure and DNS rcodes do not enter another retry cycle.
		fresh := req.Clone(ctx)
		fresh.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		resp, err = p.client.Do(fresh)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("DoH upstream returned %s", resp.Status)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/dns-message" {
		return nil, fmt.Errorf("DoH upstream returned unsupported media type %q", resp.Header.Get("Content-Type"))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessageSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDNSMessageSize {
		return nil, fmt.Errorf("DoH upstream response too large")
	}
	return body, nil
}

func (p *upstreamPool) exchangeDoT(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	dialer := tls.Dialer{NetDialer: &net.Dialer{Resolver: p.resolver, Control: interfaceControl(p.spec.ViaInterface)}, Config: &tls.Config{ServerName: upstream.ServerName, MinVersion: tls.VersionTLS12, RootCAs: p.config.RootCAs}}
	conn, err := dialer.DialContext(ctx, "tcp", upstream.Address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return exchangeLengthPrefixed(ctx, conn, query)
}

func (p *upstreamPool) exchangeDoQ(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	resolver := p.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	host, port, err := net.SplitHostPort(upstream.Address)
	if err != nil {
		return nil, err
	}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(addrs[0].IP.String(), port))
	if err != nil {
		return nil, err
	}
	listenConfig := net.ListenConfig{Control: interfaceControl(p.spec.ViaInterface)}
	packetConn, err := listenConfig.ListenPacket(ctx, udpNetworkForIP(addrs[0].IP), "")
	if err != nil {
		return nil, err
	}
	defer packetConn.Close()
	conn, err := quic.Dial(ctx, packetConn, udpAddr, &tls.Config{ServerName: upstream.ServerName, NextProtos: []string{"doq"}, MinVersion: tls.VersionTLS13, RootCAs: p.config.RootCAs}, &quic.Config{})
	if err != nil {
		return nil, err
	}
	defer conn.CloseWithError(0, "")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.CancelRead(0)
	defer stream.CancelWrite(0)
	return exchangeDoQStream(ctx, stream, query)
}

func (p *upstreamPool) exchangeUDP(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	dialer := net.Dialer{Resolver: p.resolver, Control: interfaceControl(p.spec.ViaInterface)}
	conn, err := dialer.DialContext(ctx, "udp", upstream.Address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := setExchangeDeadline(ctx, conn)
	defer stop()
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, maxDNSMessageSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (p *upstreamPool) exchangeTCP(ctx context.Context, upstream *dnsUpstream, query []byte) ([]byte, error) {
	dialer := net.Dialer{Resolver: p.resolver, Control: interfaceControl(p.spec.ViaInterface)}
	conn, err := dialer.DialContext(ctx, "tcp", upstream.Address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return exchangeLengthPrefixed(ctx, conn, query)
}

func (p *upstreamPool) dialBootstrap(ctx context.Context, network, _ string) (net.Conn, error) {
	clock, err := boottime.Read()
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	live := make([]bootstrapServer, 0, len(p.bootstrap))
	for _, server := range p.bootstrap {
		if server.ValidUntil.Live(clock) {
			live = append(live, server)
		}
	}
	p.mu.Unlock()
	if len(live) == 0 {
		return nil, errors.New("no live DNS bootstrap resolver")
	}
	server := live[(p.nextBootstrap.Add(1)-1)%uint64(len(live))]
	budget := min(p.config.QueryTimeout, server.ValidUntil.Remaining(clock))
	call, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	conn, err := (&net.Dialer{Control: interfaceControl(p.spec.ViaInterface)}).DialContext(call, network, server.Address)
	if err != nil {
		return nil, err
	}
	if server.ValidUntil == (lifetime.Deadline{}) {
		return conn, nil
	}
	bounded := &bootstrapConn{Conn: conn, until: server.ValidUntil}
	if packet, ok := conn.(net.PacketConn); ok {
		return &bootstrapPacketConn{bootstrapConn: bounded, packet: packet}, nil
	}
	return bounded, nil
}

func udpNetworkForIP(ip net.IP) string {
	if ip.To4() != nil {
		return "udp4"
	}
	return "udp6"
}

func interfaceControl(name string) func(network, address string, c syscall.RawConn) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return bindToDeviceControl(name)
}

type deadlineConn interface {
	io.Reader
	io.Writer
	SetDeadline(time.Time) error
}

func exchangeLengthPrefixed(ctx context.Context, conn deadlineConn, query []byte) ([]byte, error) {
	stop := setExchangeDeadline(ctx, conn)
	defer stop()
	if len(query) > maxDNSMessageSize {
		return nil, fmt.Errorf("DNS query too large")
	}
	if err := binary.Write(conn, binary.BigEndian, uint16(len(query))); err != nil {
		return nil, err
	}
	if _, err := io.Copy(conn, bytes.NewReader(query)); err != nil {
		return nil, err
	}
	var length uint16
	if err := binary.Read(conn, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	resp := make([]byte, int(length))
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// RFC 9250 section 4.2: one DNS message per stream, ID zero, and the
// request FIN must precede waiting for a server which defers work until FIN.
func exchangeDoQStream(ctx context.Context, stream interface {
	deadlineConn
	Close() error
}, query []byte) ([]byte, error) {
	if len(query) < 12 || len(query) > maxDNSMessageSize {
		return nil, errors.New("invalid DoQ query length")
	}
	id := binary.BigEndian.Uint16(query[:2])
	wire := append([]byte(nil), query...)
	wire[0], wire[1] = 0, 0
	stop := setExchangeDeadline(ctx, stream)
	defer stop()
	if err := binary.Write(stream, binary.BigEndian, uint16(len(wire))); err != nil {
		return nil, err
	}
	if _, err := io.Copy(stream, bytes.NewReader(wire)); err != nil {
		return nil, err
	}
	if err := stream.Close(); err != nil {
		return nil, err
	}
	var length uint16
	if err := binary.Read(stream, binary.BigEndian, &length); err != nil {
		return nil, err
	}
	if length < 12 {
		return nil, errors.New("invalid DoQ response length")
	}
	response := make([]byte, int(length))
	if _, err := io.ReadFull(stream, response); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(response[:2]) != 0 {
		return nil, errors.New("DoQ response has a non-zero message ID")
	}
	var trailing [1]byte
	n, err := stream.Read(trailing[:])
	if n != 0 || err != io.EOF {
		return nil, fmt.Errorf("DoQ response did not end with FIN: %w", err)
	}
	binary.BigEndian.PutUint16(response[:2], id)
	return response, nil
}

func setExchangeDeadline(ctx context.Context, conn deadlineConn) func() bool {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// A parent may be canceled before its deadline, including one with no
	// deadline at all. Interrupt an in-flight socket read/write in that case.
	return context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
}

func (p *upstreamPool) attemptOrder() []*dnsUpstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*dnsUpstream, 0, len(p.upstreams))
	out = append(out, p.upstreams...)
	sort.SliceStable(out, func(i, j int) bool {
		return upstreamRank(out[i].Phase) < upstreamRank(out[j].Phase)
	})
	return out
}

func upstreamRank(phase string) int {
	switch phase {
	case upstreamHealthy:
		return 0
	case upstreamProbing, upstreamFailing:
		return 1
	case upstreamDown:
		return 2
	default:
		return 1
	}
}

func (p *upstreamPool) snapshotPointers() []*dnsUpstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*dnsUpstream, len(p.upstreams))
	copy(out, p.upstreams)
	return out
}

func (p *upstreamPool) markSuccess(upstream *dnsUpstream) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	upstream.Successes++
	upstream.Failures = 0
	upstream.LastError = ""
	upstream.LastSuccess = time.Now().UTC()
	if upstream.Phase == upstreamHealthy || upstream.Successes >= p.config.PassThreshold {
		upstream.Phase = upstreamHealthy
	} else {
		upstream.Phase = upstreamProbing
	}
	return upstream.Phase
}

func (p *upstreamPool) markFailure(upstream *dnsUpstream, err error) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	upstream.Failures++
	upstream.Successes = 0
	upstream.LastError = err.Error()
	upstream.LastFailure = time.Now().UTC()
	if upstream.Failures >= p.config.FailThreshold {
		upstream.Phase = upstreamDown
	} else {
		upstream.Phase = upstreamFailing
	}
	return upstream.Phase
}

func (p *upstreamPool) Snapshot() []dnsUpstream {
	clock, clockErr := boottime.Read()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]dnsUpstream, 0, len(p.upstreams))
	for _, upstream := range p.upstreams {
		value := *upstream
		if value.Provider != "" {
			if clockErr != nil {
				value.Phase = "Unknown"
				value.LastError = clockErr.Error()
			} else if !value.ValidUntil.Live(clock) {
				value.Phase = "Expired"
			}
		}
		out = append(out, value)
	}
	return out
}

func (p *upstreamPool) bootstrapSummary() string {
	clock, clockErr := boottime.Read()
	p.mu.Lock()
	defer p.mu.Unlock()
	var parts []string
	for _, server := range p.bootstrap {
		phase := "Current"
		if server.Provider != "" {
			if clockErr != nil {
				phase = "Unknown"
			} else if !server.ValidUntil.Live(clock) {
				phase = "Expired"
			}
		}
		parts = append(parts, server.Address+":"+phase)
	}
	return strings.Join(parts, ",")
}

func (p *upstreamPool) Summary() string {
	var parts []string
	for _, upstream := range p.Snapshot() {
		parts = append(parts, fmt.Sprintf("%d:%s:%s", upstream.Index, upstream.URL, upstream.Phase))
	}
	return strings.Join(parts, ",")
}

func dnsProbeQuery() []byte {
	return []byte{
		0x72, 0x64, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x03, 'd', 'n', 's',
		0x07, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0x00,
		0x00, 0x01, 0x00, 0x01,
	}
}
