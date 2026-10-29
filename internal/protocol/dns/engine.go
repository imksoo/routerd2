// SPDX-License-Identifier: BSD-3-Clause

package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd/pkg/dnsresolver"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
)

type options struct {
	resource        string
	socketPath      string
	stateFile       string
	eventFile       string
	listenInterface string
}

type daemon struct {
	opts      options
	config    dnsresolver.RuntimeConfig
	startedAt time.Time
	phase     string
	health    string

	mu             sync.Mutex
	cond           *sync.Cond
	events         []daemonapi.DaemonEvent
	nextCursor     uint64
	runCtx         context.Context
	ready          func()
	fatal          chan error
	querySlots     chan struct{}
	queryOverloads atomic.Uint64
	queryLogDrops  atomic.Uint64
	queryLogErrors atomic.Uint64
	eventErrors    atomic.Uint64
	eventMu        sync.Mutex

	stateMu         sync.RWMutex
	inputMu         sync.Mutex
	zones           *zoneTable
	sources         []runtimeSource
	listenerSources [][]runtimeSource
	listenSources   []dnsconfig.ListenSource
	staticListeners map[string]listenBinding
	report          func(context.Context, dnsconfig.Snapshot) error
	apiReady        chan struct{}
	listeners       map[string]*boundListener
	generation      uint64
	cache           map[string]cacheEntry
	cacheBytes      int
	queryLogMu      sync.Mutex
	queryLog        *logstore.DNSQueryLog

	// Native lease snapshots are serialized by inputMu. Receipt deadlines
	// retain monotonic time and are never extended by a duplicate sequence.
	nativeInput   *dnsconfig.Input
	inputRejected atomic.Uint64
}

type runtimeSource struct {
	Spec api.DNSResolverSourceSpec
	Pool *upstreamPool
}

type listenBinding struct {
	address  netip.AddrPort
	listener int
	until    lifetime.Deadline
}

type boundListener struct {
	listenBinding
	UDP      *dns.Server
	TCP      *dns.Server
	Stopping atomic.Bool
}

type cacheEntry struct {
	Message    []byte
	Stored     time.Time
	Expires    time.Time
	Generation uint64
}

type resolveResult struct {
	Response     *dns.Msg
	Upstream     string
	CacheHit     bool
	ResponseCode string
}

type eventsResponse struct {
	Cursor string                  `json:"cursor,omitempty"`
	Events []daemonapi.DaemonEvent `json:"events"`
}

func normalizeDaemonExit(ctx context.Context, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func newDaemon(opts options, config dnsresolver.RuntimeConfig) (*daemon, error) {
	d := &daemon{
		opts:       opts,
		config:     config,
		apiReady:   make(chan struct{}),
		startedAt:  time.Now().UTC(),
		phase:      daemonapi.PhaseStarting,
		health:     daemonapi.HealthUnknown,
		cache:      map[string]cacheEntry{},
		listeners:  map[string]*boundListener{},
		fatal:      make(chan error, 1),
		querySlots: make(chan struct{}, 256),
	}
	var err error
	d.listenSources, err = dnsconfig.ListenSources(config)
	if err != nil {
		return nil, err
	}
	d.staticListeners = make(map[string]listenBinding)
	for i, listener := range config.Spec.Listen {
		for _, value := range listener.Addresses {
			address, err := netip.ParseAddrPort(net.JoinHostPort(value, fmt.Sprint(listener.Port)))
			if err != nil {
				return nil, err
			}
			d.staticListeners[address.String()] = listenBinding{listener: i, address: address}
		}
	}
	d.cond = sync.NewCond(&d.mu)
	d.zones = newZoneTable(config.Zones)
	sources, err := buildRuntimeSources(config)
	if err != nil {
		return nil, err
	}
	d.sources = sources
	for _, listener := range config.Spec.Listen {
		d.listenerSources = append(d.listenerSources, sourcesForListen(sources, listener.Sources))
	}
	return d, nil
}

func buildRuntimeSources(config dnsresolver.RuntimeConfig) ([]runtimeSource, error) {
	var sources []runtimeSource
	for _, source := range config.Spec.Sources {
		runtime := runtimeSource{Spec: source}
		if source.Kind == "forward" || source.Kind == "upstream" {
			timeout, _ := time.ParseDuration(firstNonEmpty(source.Healthcheck.Timeout, "3s"))
			queryTimeout, _ := time.ParseDuration(firstNonEmpty(source.QueryTimeout, source.Healthcheck.Timeout, "3s"))
			interval, _ := time.ParseDuration(firstNonEmpty(source.Healthcheck.Interval, "15s"))
			pool, err := newUpstreamPool(source, upstreamPoolConfig{
				ProbeInterval: interval,
				ProbeTimeout:  timeout,
				QueryTimeout:  queryTimeout,
				FailThreshold: source.Healthcheck.FailThreshold,
				PassThreshold: source.Healthcheck.PassThreshold,
			})
			if err != nil {
				return nil, err
			}
			runtime.Pool = pool
		}
		sources = append(sources, runtime)
	}
	return sources, nil
}

func (d *daemon) Run(ctx context.Context) (result error) {
	runCtx, cancel := context.WithCancel(ctx)
	var apiDone chan error
	apiStopped := false
	defer func() {
		cancel()
		d.inputMu.Lock()
		d.shutdownDNSServers()
		d.inputMu.Unlock()
		d.closeQueryLog()
		if apiDone != nil && !apiStopped {
			// localapi bounds graceful shutdown to two seconds and then closes.
			if err := <-apiDone; err != nil && result == nil {
				result = err
			}
		}
		if err := d.setState(daemonapi.PhaseStopped, daemonapi.HealthUnknown); err != nil {
			if result == nil || errors.Is(result, ctx.Err()) {
				result = err
			} else {
				result = errors.Join(result, err)
			}
		}
		d.publish(daemonapi.EventDaemonStopped, daemonapi.SeverityInfo, "Stopped", "DNS resolver module stopped", nil)
	}()
	d.publish(daemonapi.EventDaemonStarted, daemonapi.SeverityInfo, "Started", "DNS resolver module started", nil)
	if d.config.Spec.QueryLog.Enabled {
		queryLog, err := openQueryLog(d.config.Spec.QueryLog.Path)
		if err != nil {
			return fmt.Errorf("open DNS query log: %w", err)
		}
		d.queryLogMu.Lock()
		d.queryLog = queryLog
		d.queryLogMu.Unlock()
	}
	for _, source := range d.sources {
		if source.Pool != nil {
			source.Pool.Start(runCtx, d.publish)
		}
	}
	d.inputMu.Lock()
	d.runCtx = runCtx
	clock, err := boottime.Read()
	if err == nil {
		var addresses []dnsconfig.ListenAddress
		if d.nativeInput != nil {
			addresses = d.nativeInput.Listeners
		}
		err = d.updateListeners(addresses, clock)
	}
	d.inputMu.Unlock()
	if err != nil {
		return err
	}
	apiReady := d.apiReady
	apiDone = make(chan error, 1)
	go func() { apiDone <- localapi.Serve(runCtx, d.opts.socketPath, d.routes(), func() { close(apiReady) }) }()
	select {
	case err := <-apiDone:
		apiStopped = true
		return unexpectedAPIExit(runCtx, err)
	case err := <-d.fatal:
		return err
	case <-runCtx.Done():
		return runCtx.Err()
	case <-apiReady:
	}
	d.inputMu.Lock()
	err = d.reportListening()
	d.inputMu.Unlock()
	if err != nil {
		return err
	}
	d.publish(daemonapi.EventDaemonReady, daemonapi.SeverityInfo, "Ready", "DNS resolver private API is ready", nil)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-d.fatal:
		return err
	case err := <-apiDone:
		apiStopped = true
		return unexpectedAPIExit(runCtx, err)
	}
}

func unexpectedAPIExit(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("DNS private API stopped unexpectedly")
}

func (d *daemon) openBoundListeners(ctx context.Context, addrs map[string]listenBinding) (map[string]*boundListener, error) {
	listenConfig := dnsListenConfig(d.opts.listenInterface)
	listeners := map[string]*boundListener{}
	for addr, binding := range addrs {
		bound := &boundListener{listenBinding: binding}
		handler := dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
			d.stateMu.RLock()
			deadline := bound.until
			listener := bound.listener
			d.stateMu.RUnlock()
			if deadline != (lifetime.Deadline{}) {
				clock, err := boottime.Read()
				if err != nil || !deadline.Live(clock) {
					response := new(dns.Msg)
					response.SetRcode(req, dns.RcodeRefused)
					_ = w.WriteMsg(response)
					return
				}
			}
			d.handleDNS(listener, w, req)
		})
		packetConn, err := listenConfig.ListenPacket(ctx, "udp", addr)
		if err != nil {
			shutdownBoundListeners(listeners)
			return nil, fmt.Errorf("listen udp %s: %w", addr, err)
		}
		listener, err := listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			_ = packetConn.Close()
			shutdownBoundListeners(listeners)
			return nil, fmt.Errorf("listen tcp %s: %w", addr, err)
		}
		bound.UDP = &dns.Server{PacketConn: packetConn, Net: "udp", Handler: handler}
		bound.TCP = &dns.Server{Listener: listener, Net: "tcp", Handler: handler}
		listeners[addr] = bound
		started := make(chan struct{}, 2)
		bound.UDP.NotifyStartedFunc = func() { started <- struct{}{} }
		bound.TCP.NotifyStartedFunc = func() { started <- struct{}{} }
		go d.serveDNS(bound.UDP, bound, addr, "udp")
		go d.serveDNS(bound.TCP, bound, addr, "tcp")
		for count := 0; count < 2; count++ {
			select {
			case <-started:
			case err := <-d.fatal:
				shutdownBoundListeners(listeners)
				return nil, err
			case <-ctx.Done():
				shutdownBoundListeners(listeners)
				return nil, ctx.Err()
			}
		}
	}
	return listeners, nil
}

func (d *daemon) serveDNS(server *dns.Server, bound *boundListener, addr, network string) {
	err := server.ActivateAndServe()
	if bound.Stopping.Load() || d.runCtx != nil && d.runCtx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("listener stopped unexpectedly")
	}
	err = fmt.Errorf("DNS %s listener %s: %w", network, addr, err)
	d.publish(daemonapi.EventDaemonCrashed, daemonapi.SeverityError, "ServeFailed", err.Error(), map[string]string{"listen": addr, "network": network})
	select {
	case d.fatal <- err:
	default:
	}
}

func (d *daemon) shutdownDNSServers() {
	d.stateMu.Lock()
	listeners := d.listeners
	d.listeners = map[string]*boundListener{}
	d.stateMu.Unlock()
	shutdownBoundListeners(listeners)
}

func shutdownBoundListeners(listeners map[string]*boundListener) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, listener := range listeners {
		listener.Stopping.Store(true)
		_ = listener.UDP.ShutdownContext(ctx)
		_ = listener.TCP.ShutdownContext(ctx)
	}
}

func (d *daemon) handleDNS(listener int, w dns.ResponseWriter, req *dns.Msg) {
	select {
	case d.querySlots <- struct{}{}:
		defer func() { <-d.querySlots }()
	default:
		d.queryOverloads.Add(1)
		response := new(dns.Msg)
		response.SetRcode(req, dns.RcodeServerFailure)
		_ = w.WriteMsg(response)
		return
	}
	started := time.Now()
	result, err := d.resolve(listener, req)
	resp := result.Response
	if err != nil {
		resp = new(dns.Msg)
		resp.SetRcode(req, dns.RcodeServerFailure)
		result.Response = resp
		result.ResponseCode = dns.RcodeToString[resp.Rcode]
	}
	_ = w.WriteMsg(resp)
	if err != nil && len(req.Question) > 0 {
		client := w.RemoteAddr().String()
		if host, _, splitErr := net.SplitHostPort(client); splitErr == nil {
			client = host
		}
		d.publish("routerd.dns.query.failed", daemonapi.SeverityWarning, "DNSResolutionFailed", err.Error(), map[string]string{
			"source": result.Upstream, "qname": req.Question[0].Name,
			"qtype": dns.TypeToString[req.Question[0].Qtype], "clientAddress": client,
		})
	}
	d.recordQuery(w.RemoteAddr().String(), req, result, time.Since(started))
}

func (d *daemon) resolve(listener int, req *dns.Msg) (resolveResult, error) {
	config, zones, generation := d.runtimeSnapshot()
	if req.Opcode != dns.OpcodeQuery {
		resp := new(dns.Msg)
		resp.SetRcode(req, dns.RcodeNotImplemented)
		return resolveResult{Response: resp, ResponseCode: dns.RcodeToString[resp.Rcode]}, nil
	}
	if len(req.Question) != 1 {
		resp := new(dns.Msg)
		resp.SetRcode(req, dns.RcodeFormatError)
		return resolveResult{Response: resp, ResponseCode: dns.RcodeToString[resp.Rcode]}, nil
	}
	cacheKey := dnsCacheKey(fmt.Sprint(listener), req)
	if config.Spec.Cache.Enabled {
		if cached, ok := d.cacheGet(cacheKey, generation); ok {
			var msg dns.Msg
			if err := msg.Unpack(cached); err == nil {
				msg.Id = req.Id
				return resolveResult{Response: &msg, CacheHit: true, Upstream: "cache", ResponseCode: dns.RcodeToString[msg.Rcode]}, nil
			}
		}
	}
	question := req.Question[0]
	for _, source := range d.listenerSources[listener] {
		if !sourceMatches(source.Spec.Match, question.Name) {
			continue
		}
		switch source.Spec.Kind {
		case "zone":
			if resp, ok := zones.Answer(req, source.Spec.ZoneRef); ok {
				return resolveResult{Response: resp, Upstream: sourceName(source.Spec), ResponseCode: dns.RcodeToString[resp.Rcode]}, nil
			}
		case "forward", "upstream":
			result := resolveResult{Upstream: sourceName(source.Spec)}
			if source.Pool == nil {
				return result, fmt.Errorf("DNS source %q has no upstream pool", result.Upstream)
			}
			upstreamReq := req.Copy()
			if source.Spec.DNSSECValidate {
				upstreamReq.SetEdns0(1232, true)
			}
			wire, err := upstreamReq.Pack()
			if err != nil {
				return result, err
			}
			queryCtx := d.runCtx
			if queryCtx == nil {
				queryCtx = context.Background()
			}
			out, err := source.Pool.Exchange(queryCtx, wire, d.publish)
			if err != nil {
				// Failover is limited to this source's explicitly configured
				// upstreams. Falling through could leak a private query to a
				// less-specific public source, or invent an NXDOMAIN response.
				return result, fmt.Errorf("DNS source %q: %w", result.Upstream, err)
			}
			var resp dns.Msg
			if err := resp.Unpack(out); err != nil {
				return result, err
			}
			if source.Spec.DNSSECValidate && !resp.AuthenticatedData {
				d.publish("routerd.dns.resolver.dnssec.failed", daemonapi.SeverityWarning, "DNSSECValidationFailed", question.Name, map[string]string{"source": source.Spec.Name, "qname": question.Name})
				return result, fmt.Errorf("DNS source %q: DNSSEC validation failed", result.Upstream)
			}
			if config.Spec.Cache.Enabled {
				d.cacheSet(cacheKey, out, dnsMessageTTL(&resp, config.Spec.Cache), config.Spec.Cache.MaxEntries, generation)
			}
			return resolveResult{Response: &resp, Upstream: sourceName(source.Spec), ResponseCode: dns.RcodeToString[resp.Rcode]}, nil
		}
	}
	resp := new(dns.Msg)
	resp.SetRcode(req, dns.RcodeNameError)
	return resolveResult{Response: resp, ResponseCode: dns.RcodeToString[resp.Rcode]}, nil
}

func (d *daemon) runtimeSnapshot() (dnsresolver.RuntimeConfig, *zoneTable, uint64) {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return d.config, d.zones, d.generation
}

func (d *daemon) listenerCount() int {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	return len(d.listeners)
}

func (d *daemon) recordQuery(remoteAddr string, req *dns.Msg, result resolveResult, duration time.Duration) {
	if len(req.Question) == 0 {
		return
	}
	client := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		client = host
	}
	question := req.Question[0]
	entry := logstore.DNSQuery{
		Timestamp:     time.Now().UTC(),
		ClientAddress: client,
		QuestionName:  question.Name,
		QuestionType:  dns.TypeToString[question.Qtype],
		ResponseCode:  result.ResponseCode,
		Answers:       dnsAnswerStrings(result.Response),
		Upstream:      result.Upstream,
		CacheHit:      result.CacheHit,
		Duration:      duration,
	}
	if entry.ResponseCode == "" && result.Response != nil {
		entry.ResponseCode = dns.RcodeToString[result.Response.Rcode]
	}
	// DNS answering must not queue behind disk I/O. A bounded log write and
	// explicit drop/error counters keep pressure observable without wedging DNS.
	if !d.queryLogMu.TryLock() {
		d.queryLogDrops.Add(1)
		return
	}
	defer d.queryLogMu.Unlock()
	if d.queryLog == nil {
		return
	}
	parent := d.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	if err := d.queryLog.Record(ctx, entry); err == nil {
		return
	} else {
		d.queryLogFailure(err)
	}
	if ctx.Err() != nil {
		return
	}
	// Reopen a stale/failed SQLite handle; keep the failed handle if reopening
	// fails so the next query can retry and status still exposes the log path.
	path := d.queryLog.Stats().Path
	_ = d.queryLog.Close()
	queryLog, err := openQueryLog(path)
	if err != nil {
		d.queryLogFailure(err)
		return
	}
	d.queryLog = queryLog
	if err := d.queryLog.Record(ctx, entry); err != nil {
		d.queryLogFailure(err)
	}

}

func (d *daemon) queryLogFailure(err error) {
	failures := d.queryLogErrors.Add(1)
	if failures == 1 || failures%100 == 0 {
		slog.Warn("DNS query logging failed", "resource", d.opts.resource, "error", err, "failures", failures)
	}
}

func (d *daemon) closeQueryLog() {
	d.queryLogMu.Lock()
	defer d.queryLogMu.Unlock()
	if d.queryLog == nil {
		return
	}
	_ = d.queryLog.Close()
	d.queryLog = nil
}

func sourceName(source api.DNSResolverSourceSpec) string {
	if strings.TrimSpace(source.Name) != "" {
		return source.Name
	}
	return source.Kind
}

func dnsAnswerStrings(msg *dns.Msg) []string {
	if msg == nil {
		return nil
	}
	var out []string
	for _, rr := range msg.Answer {
		switch record := rr.(type) {
		case *dns.A:
			out = append(out, record.A.String())
		case *dns.AAAA:
			out = append(out, record.AAAA.String())
		}
	}
	return out
}

func sourcesForListen(sources []runtimeSource, selected []string) []runtimeSource {
	if len(selected) == 0 {
		return sources
	}
	out := make([]runtimeSource, 0, len(selected))
	for _, name := range selected {
		for _, source := range sources {
			if source.Spec.Name == name {
				out = append(out, source)
				break
			}
		}
	}
	return out
}

func (d *daemon) cacheGet(key string, generation uint64) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.cache[key]
	if !ok || entry.Generation != generation || time.Now().After(entry.Expires) {
		d.deleteCacheLocked(key)
		return nil, false
	}
	var message dns.Msg
	if err := message.Unpack(entry.Message); err != nil {
		d.deleteCacheLocked(key)
		return nil, false
	}
	elapsed := uint32(max(0, time.Since(entry.Stored)/time.Second))
	remaining := uint32(max(0, time.Until(entry.Expires)/time.Second))
	for _, records := range [][]dns.RR{message.Answer, message.Ns, message.Extra} {
		for _, record := range records {
			if record.Header().Rrtype == dns.TypeOPT {
				continue // OPT TTL carries flags, not a cache lifetime.
			}
			ttl := record.Header().Ttl
			if elapsed >= ttl {
				ttl = 0
			} else {
				ttl -= elapsed
			}
			record.Header().Ttl = min(ttl, remaining)
		}
	}
	packed, err := message.Pack()
	return packed, err == nil
}

func (d *daemon) cacheSet(key string, msg []byte, ttl time.Duration, maxEntries int, generation uint64) {
	if ttl <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stateMu.RLock()
	currentGeneration := d.generation
	d.stateMu.RUnlock()
	if generation != currentGeneration {
		return
	}
	d.deleteCacheLocked(key)
	charge := len(msg) + len(key) + 128
	if charge > maxCacheBytes {
		return
	}
	for d.cacheBytes+charge > maxCacheBytes || maxEntries > 0 && len(d.cache) >= maxEntries {
		for k := range d.cache {
			d.deleteCacheLocked(k)
			break
		}
	}
	d.cache[key] = cacheEntry{Message: append([]byte(nil), msg...), Stored: time.Now(), Expires: time.Now().Add(ttl), Generation: generation}
	d.cacheBytes += charge
}

const maxCacheBytes = 32 << 20

func (d *daemon) deleteCacheLocked(key string) {
	if entry, ok := d.cache[key]; ok {
		d.cacheBytes = max(0, d.cacheBytes-len(entry.Message)-len(key)-128)
		delete(d.cache, key)
	}
}

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		if d.currentHealth() != daemonapi.HealthOK {
			localapi.Fail(w, http.StatusServiceUnavailable, errors.New("not healthy"))
			return
		}
		localapi.JSON(w, http.StatusOK, map[string]bool{"healthy": true})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { localapi.JSON(w, http.StatusOK, d.status()) })
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		localapi.JSON(w, http.StatusOK, d.eventsSince(r.URL.Query().Get("since")))
	})
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		localapi.JSON(w, http.StatusOK, d.config)
	})
	return mux
}

func (d *daemon) status() daemonapi.DaemonStatus {
	d.mu.Lock()
	phase := d.phase
	startedAt := d.startedAt
	d.mu.Unlock()
	health := d.currentHealth()
	status := daemonapi.NewStatus(daemonapi.DaemonRef{Name: d.opts.resource, Kind: dnsresolver.DaemonKind, Instance: d.opts.resource})
	status.Phase = phase
	status.Health = health
	status.Since = startedAt
	status.Resources = []daemonapi.ResourceStatus{{
		Resource: daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "DNSResolver", Name: d.opts.resource},
		Phase:    phase,
		Health:   health,
		Since:    startedAt,
		Conditions: []daemonapi.Condition{{Type: "Ready", Status: conditionStatus(health == daemonapi.HealthOK), Reason: phase,
			LastTransitionTime: startedAt}},
		Observed: d.observedStatus(),
	}}
	return status
}

func (d *daemon) observedStatus() map[string]string {
	d.stateMu.RLock()
	listenerCount := len(d.listeners)
	zones := d.zones
	sources := append([]runtimeSource(nil), d.sources...)
	d.stateMu.RUnlock()
	observed := map[string]string{"inputSnapshotsRejected": fmt.Sprint(d.inputRejected.Load()), "listeners": fmt.Sprint(listenerCount), "zones": fmt.Sprint(zones.ZoneCount()), "queryOverloads": fmt.Sprint(d.queryOverloads.Load()), "queryLogDropped": fmt.Sprint(d.queryLogDrops.Load()), "queryLogErrors": fmt.Sprint(d.queryLogErrors.Load()), "eventPersistenceErrors": fmt.Sprint(d.eventErrors.Load())}
	d.mu.Lock()
	observed["cacheEntries"], observed["cacheBytes"] = fmt.Sprint(len(d.cache)), fmt.Sprint(d.cacheBytes)
	d.mu.Unlock()
	d.queryLogMu.Lock()
	queryLog := d.queryLog
	d.queryLogMu.Unlock()
	if queryLog != nil {
		stats := queryLog.Stats()
		observed["queryLogEnabled"] = "true"
		observed["queryLogPath"] = stats.Path
		observed["queryLogRecords"] = fmt.Sprintf("%d", stats.Records)
		observed["queryLogRecordErrors"] = fmt.Sprintf("%d", stats.RecordErrors)
		observed["queryLogDBBytes"] = fmt.Sprintf("%d", stats.DBBytes)
		observed["queryLogWALBytes"] = fmt.Sprintf("%d", stats.WALBytes)
		if !stats.LastRecordTime.IsZero() {
			observed["queryLogLastRecordAt"] = stats.LastRecordTime.Format(time.RFC3339Nano)
		}
	} else {
		observed["queryLogEnabled"] = "false"
	}
	var parts, bootstrap []string
	for _, source := range sources {
		if source.Pool != nil {
			parts = append(parts, source.Spec.Name+"="+source.Pool.Summary())
			bootstrap = append(bootstrap, source.Spec.Name+"="+source.Pool.bootstrapSummary())
		}
	}
	if len(parts) > 0 {
		observed["upstreams"] = strings.Join(parts, ";")
		observed["bootstrapResolvers"] = strings.Join(bootstrap, ";")
	}
	return observed
}

func (d *daemon) eventsSince(since string) eventsResponse {
	d.mu.Lock()
	defer d.mu.Unlock()
	var cursor uint64
	_, _ = fmt.Sscanf(since, "%d", &cursor)
	var events []daemonapi.DaemonEvent
	for _, event := range d.events {
		var eventCursor uint64
		_, _ = fmt.Sscanf(event.Cursor, "%d", &eventCursor)
		if eventCursor > cursor {
			events = append(events, event)
		}
	}
	if len(d.events) > 0 {
		return eventsResponse{Cursor: d.events[len(d.events)-1].Cursor, Events: events}
	}
	return eventsResponse{Events: events}
}

func (d *daemon) publish(eventType, severity, reason, message string, attrs map[string]string) {
	// Preserve journal order without holding the query/cache lock during I/O.
	d.eventMu.Lock()
	defer d.eventMu.Unlock()
	d.mu.Lock()
	d.nextCursor++
	event := daemonapi.NewEvent(daemonapi.DaemonRef{Name: d.opts.resource, Kind: dnsresolver.DaemonKind, Instance: d.opts.resource}, eventType, severity)
	event.Cursor = fmt.Sprint(d.nextCursor)
	event.Reason, event.Message, event.Attributes = reason, message, attrs
	event.Resource = &daemonapi.ResourceRef{APIVersion: api.NetAPIVersion, Kind: "DNSResolver", Name: d.opts.resource}
	d.events = append(d.events, event)
	if len(d.events) > 1000 {
		d.events = d.events[len(d.events)-1000:]
	}
	if d.cond != nil {
		d.cond.Broadcast()
	}
	d.mu.Unlock()
	if err := appendEvent(d.opts.eventFile, event); err != nil {
		failures := d.eventErrors.Add(1)
		if failures == 1 || failures%100 == 0 {
			slog.Warn("DNS event persistence failed", "resource", d.opts.resource, "error", err, "failures", failures)
		}
	}
}

func (d *daemon) setState(phase, health string) error {
	d.stateMu.RLock()
	listenerCount, zoneCount := len(d.listeners), d.zones.ZoneCount()
	d.stateMu.RUnlock()
	d.mu.Lock()
	d.phase, d.health = phase, health
	d.mu.Unlock()
	state := struct {
		Phase     string `json:"phase"`
		Health    string `json:"health"`
		Listeners int    `json:"listeners"`
		Zones     int    `json:"zones"`
	}{phase, health, listenerCount, zoneCount}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomicPrivate(d.opts.stateFile, append(data, '\n')); err != nil {
		return fmt.Errorf("persist DNS state: %w", err)
	}
	return nil
}

func (d *daemon) currentHealth() string {
	d.mu.Lock()
	health := d.health
	d.mu.Unlock()
	if health != daemonapi.HealthOK {
		return health
	}
	d.stateMu.RLock()
	sources := append([]runtimeSource(nil), d.sources...)
	d.stateMu.RUnlock()
	for _, source := range sources {
		if source.Pool == nil {
			continue
		}
		healthy, unknown := false, false
		entries := source.Pool.Snapshot()
		for _, endpoint := range entries {
			healthy = healthy || endpoint.Phase == upstreamHealthy
			unknown = unknown || endpoint.Phase == upstreamProbing || endpoint.Phase == "Expired" || endpoint.Phase == "Unknown"
		}
		if healthy {
			continue
		}
		if len(entries) == 0 || unknown {
			return daemonapi.HealthUnknown
		}
		return daemonapi.HealthDegraded
	}
	return health
}

func conditionStatus(ok bool) string {
	if ok {
		return daemonapi.ConditionTrue
	}
	return daemonapi.ConditionFalse
}
