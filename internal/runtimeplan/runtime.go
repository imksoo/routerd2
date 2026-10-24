// Package runtimeplan reconciles a transactional configuration and live protocol
// facts. Protocol workers own sockets; this coordinator alone composes their
// kernel intent and sends it to the single network worker.
package runtimeplan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type Network interface {
	Apply(context.Context, uint64, network.Desired) error
}
type moduleManager interface {
	Reconcile(context.Context, []supervisor.DynamicModule) error
	Status(context.Context) (supervisor.Status, error)
}
type managerClient struct{ c *localapi.Client }

func (m managerClient) Reconcile(ctx context.Context, v []supervisor.DynamicModule) error {
	return m.c.Do(ctx, "POST", "/v1/modules/reconcile", supervisor.ReconcileModules{Modules: v}, nil)
}
func (m managerClient) Status(ctx context.Context) (s supervisor.Status, err error) {
	err = m.c.Do(ctx, "GET", "/v1/status", nil, &s)
	return
}

type binding struct {
	instance serviceconfig.Instance
	digest   string
}
type Status struct {
	RouteIssueCount int              `json:"route_issue_count"`
	RouteIssues     []RouteIssue     `json:"route_issues,omitempty"`
	Revision        uint64           `json:"revision"`
	Phase           string           `json:"phase"`
	LastError       string           `json:"last_error,omitempty"`
	Instances       []InstanceStatus `json:"instances"`
}
type InstanceStatus struct {
	BFD               *BFDHealthStatus `json:"bfd_health,omitempty"`
	SnapshotBytes     int              `json:"snapshot_bytes,omitempty"`
	SnapshotTruncated bool             `json:"snapshot_truncated,omitempty"`
	OwnershipExpired  bool             `json:"ownership_expired,omitempty"`
	Name              string           `json:"name"`
	Role              string           `json:"role"`
	ConfigDigest      string           `json:"config_digest"`
	Generation        uint64           `json:"generation,omitempty"`
	ProducerEpoch     string           `json:"producer_epoch,omitempty"`
	Sequence          uint64           `json:"sequence,omitempty"`
	Snapshot          json.RawMessage  `json:"snapshot,omitempty"`
}
type Runtime struct {
	op sync.Mutex // module-set reconciliation; never held by protocol callbacks
	mu sync.Mutex // complete projection and protocol acceptance are serialized
	// BFD observations and recipient grants never acquire mu: the network
	// writer can hold it across a long apply. Configuration takes mu -> bfdMu;
	// no path may acquire these locks in the opposite order.
	bfdMu            sync.Mutex
	bfdSources       map[string]bfdBinding
	bfdRecipients    map[string]bfdRecipient
	dir, stateDir    string
	network          Network
	manager          moduleManager
	current          config.Native
	revision         uint64
	configured       bool
	initialized      bool
	bindings         map[string]binding
	records          map[string]serviceconfig.Update
	liveRA           map[string]ra.Config
	aftrInputs       map[string]aftr.Input
	installed        []supervisor.DynamicModule
	appliedHash      string
	lastNetworkApply time.Time
	leases           map[string]time.Time
	routeIssues      []RouteIssue
	phase, lastError string
}

func New(dir, stateDir string, n Network) *Runtime {
	return &Runtime{dir: dir, stateDir: stateDir, network: n, manager: managerClient{localapi.NewClient(filepath.Join(dir, supervisor.SocketName))}, bindings: map[string]binding{}, records: map[string]serviceconfig.Update{}, liveRA: map[string]ra.Config{}, leases: map[string]time.Time{}, phase: "starting"}
}

// CheckReady queries the process manager at confirmation time. A previously
// successful configuration cannot be confirmed while one of its required
// protocol processes has since crashed or entered restart backoff.
func (r *Runtime) CheckReady(ctx context.Context) error {
	s, err := r.manager.Status(ctx)
	if err != nil {
		return err
	}
	if !s.Ready {
		return errors.New("required routerd modules are not ready")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.routeIssues) > 0 {
		return errors.New("some learned routes have unresolved next hops; inspect show services")
	}
	byName := map[string]supervisor.ModuleStatus{}
	for _, module := range s.Modules {
		byName[module.Name] = module
	}
	for name, binding := range r.bindings {
		module, ok := byName[name]
		if !ok || !module.Ready || module.ConfigDigest != binding.digest {
			return fmt.Errorf("protocol %s is not ready for the committed configuration", name)
		}
	}
	return nil
}
func (r *Runtime) failure(err error) error {
	r.mu.Lock()
	r.phase = "degraded"
	r.lastError = err.Error()
	r.mu.Unlock()
	return err
}

// ApplyConfig accepts only a source ResourceProgram. Compilation is pure and
// completes before process reconciliation, instance-file writes, or kernel I/O.
// Protocol facts can change its projection, but cannot replace operator intent.
func (r *Runtime) ApplyConfig(ctx context.Context, revision uint64, source config.Document) error {
	plan, err := config.Compile(source)
	if err != nil {
		return err
	}
	doc := plan.Native
	r.op.Lock()
	defer r.op.Unlock()
	if !r.initialized {
		// Control-worker recovery is deliberately explicit: quiesce previous
		// dynamic workers, then reacquire their protocol state. Continuing an
		// old master/lease without an accepted producer is not recovery.
		if err := r.manager.Reconcile(ctx, nil); err != nil {
			return r.failure(err)
		}
		if err := r.wait(ctx, nil, nil, true); err != nil {
			return r.failure(err)
		}
		r.initialized = true
	}
	r.mu.Lock()
	retained, err := r.retainedLocked(doc)
	var p projection
	if err == nil {
		p, err = r.projectLocked(doc, retained, time.Now())
	}
	r.mu.Unlock()
	if err != nil {
		return r.failure(err)
	}
	_, modules, err := r.prepare(p.instances)
	if err != nil {
		return r.failure(err)
	}
	if err := r.quiesceChanged(ctx, modules); err != nil {
		return r.failure(err)
	}
	r.mu.Lock()
	// Unchanged producers may renew while changed workers are stopping. Take
	// their latest acknowledged facts at cutover; never restore an older lease.
	retained, err = r.retainedLocked(doc)
	if err == nil {
		p, err = r.projectLocked(doc, retained, time.Now())
	}
	var bindings map[string]binding
	if err == nil {
		bindings, modules, err = r.prepare(p.instances)
	}
	var bfdSources map[string]bfdBinding
	var bfdRecipients map[string]bfdRecipient
	if err == nil {
		bfdSources, bfdRecipients, err = compileBFDCache(bindings)
	}
	if err != nil {
		r.mu.Unlock()
		return r.failure(err)
	}
	r.current = doc
	r.revision = revision
	r.configured = true
	r.records = retained
	r.bindings = bindings
	r.installBFDCacheLocked(bfdSources, bfdRecipients)
	for name, input := range r.aftrInputs {
		if b, ok := bindings[name]; !ok || b.digest != input.ConfigDigest {
			delete(r.aftrInputs, name)
		}
	}
	r.liveRA = p.liveRA
	r.routeIssues = p.routeIssues
	r.phase = "applying"
	r.lastError = ""
	err = r.applyNetworkLocked(ctx, p.network)
	r.mu.Unlock()
	if err != nil {
		return r.failure(err)
	}
	if err := r.manager.Reconcile(ctx, modules); err != nil {
		return r.failure(err)
	}
	r.installed = modules
	if err := r.wait(ctx, modules, nil, false); err != nil {
		return r.failure(err)
	}
	r.mu.Lock()
	r.phase = "ready"
	r.lastError = ""
	r.mu.Unlock()
	return nil
}

// Caller holds mu. Configuration equality includes all protocol-owned paths.
func (r *Runtime) retainedLocked(doc config.Native) (map[string]serviceconfig.Update, error) {
	retained := map[string]serviceconfig.Update{}
	effective := r.effectiveRecordsLocked(time.Now())
	for _, service := range doc.Services {
		instance, err := service.Instance(r.dir, r.stateDir)
		if err != nil {
			return nil, err
		}
		_, digest, err := encodeInstance(instance)
		if err != nil {
			return nil, err
		}
		if old, ok := r.bindings[instance.Name]; ok && old.digest == digest {
			if value, ok := effective[instance.Name]; ok {
				retained[instance.Name] = value
			}
		}
	}
	return retained, nil
}

// The local receipt deadline retains Go's monotonic clock. A backward wall
// clock step cannot prolong an HA ownership lease and an ordinary replay
// cannot create one. The worker still supplies the kernel's absolute deadline.
func (r *Runtime) effectiveRecordsLocked(now time.Time) map[string]serviceconfig.Update {
	out := make(map[string]serviceconfig.Update, len(r.records))
	for name, record := range r.records {
		if record.Role == "dhcp_server" && !now.Before(r.leases[name]) {
			var snapshot dhcpserverconfig.Snapshot
			if json.Unmarshal(record.Snapshot, &snapshot) == nil && snapshot.Active {
				snapshot.Active = false
				snapshot.ValidUntil = time.Time{}
				snapshot.Leases = nil
				record.Snapshot, _ = json.Marshal(snapshot)
			}
		}
		if record.Role == "vrrp" && !now.Before(r.leases[name]) {
			var snapshot vrrp.Snapshot
			if json.Unmarshal(record.Snapshot, &snapshot) == nil && snapshot.Active {
				snapshot.Active = false
				record.Snapshot, _ = json.Marshal(snapshot)
			}
		}
		if record.Role == "bgp" && !now.Before(r.leases[name]) {
			var snapshot bgpconfig.Snapshot
			if json.Unmarshal(record.Snapshot, &snapshot) == nil && snapshot.Active {
				snapshot.Active = false
				record.Snapshot, _ = json.Marshal(snapshot)
			}
		}
		out[name] = record
	}
	r.mergeBFDRecordsLocked(out)
	return out
}

func encodeInstance(v serviceconfig.Instance) ([]byte, string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, "", err
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}
func (r *Runtime) prepare(instances []serviceconfig.Instance) (map[string]binding, []supervisor.DynamicModule, error) {
	bindings := map[string]binding{}
	modules := []supervisor.DynamicModule{}
	for _, v := range instances {
		_, digest, err := encodeInstance(v)
		if err != nil {
			return nil, nil, err
		}
		dir := filepath.Join(r.dir, "instances", v.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, nil, err
		}
		path := filepath.Join(dir, digest+".json")
		if err := config.AtomicJSON(path, v); err != nil {
			return nil, nil, err
		}
		bindings[v.Name] = binding{v, digest}
		modules = append(modules, supervisor.DynamicModule{Name: v.Name, Role: v.Role, ConfigDigest: digest})
	}
	return bindings, modules, nil
}
func (r *Runtime) quiesceChanged(ctx context.Context, next []supervisor.DynamicModule) error {
	wanted := map[string]string{}
	for _, v := range next {
		wanted[v.Name] = v.ConfigDigest
	}
	keep := []supervisor.DynamicModule{}
	removed := map[string]bool{}
	for _, v := range r.installed {
		if wanted[v.Name] == v.ConfigDigest {
			keep = append(keep, v)
		} else {
			removed[v.Name] = true
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := r.manager.Reconcile(ctx, keep); err != nil {
		return err
	}
	return r.wait(ctx, nil, removed, false)
}
func (r *Runtime) wait(ctx context.Context, wanted []supervisor.DynamicModule, removed map[string]bool, allRemoved bool) error {
	ctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		s, err := r.manager.Status(ctx)
		if err != nil {
			return err
		}
		byName := map[string]supervisor.ModuleStatus{}
		okay := true
		for _, m := range s.Modules {
			byName[m.Name] = m
			if removed[m.Name] || (allRemoved && strings.HasPrefix(m.Name, "service.")) {
				okay = false
			}
		}
		for _, v := range wanted {
			m, ok := byName[v.Name]
			if !ok || !m.Ready || m.ConfigDigest != v.ConfigDigest {
				okay = false
				if ok && m.State == "failed" {
					return fmt.Errorf("%s failed: %s", m.Name, m.LastError)
				}
			}
		}
		if okay {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("module reconciliation: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
func (r *Runtime) applyNetworkLocked(ctx context.Context, d network.Desired) error {
	b, _ := json.Marshal(struct {
		Revision uint64
		Desired  network.Desired
	}{r.revision, d})
	h := sha256.Sum256(b)
	digest := hex.EncodeToString(h[:])
	if digest == r.appliedHash && time.Since(r.lastNetworkApply) < 5*time.Second {
		return nil
	}
	if err := r.network.Apply(ctx, r.revision, d); err != nil {
		return err
	}
	r.appliedHash = digest
	r.lastNetworkApply = time.Now()
	return nil
}

var ErrStale = errors.New("stale protocol producer or configuration")

// Caller holds mu. Repeating a snapshot with a larger transport sequence is
// still a replay of the same ownership grant. It must not extend monotonic
// validity after a backward wall-clock adjustment.
func (r *Runtime) recordOwnershipLocked(u serviceconfig.Update, maximum time.Duration) error {
	var grant struct {
		Active     bool      `json:"active"`
		ValidUntil time.Time `json:"valid_until"`
	}
	_ = json.Unmarshal(u.Snapshot, &grant)
	if !grant.Active {
		delete(r.leases, u.Name)
		return nil
	}
	now := time.Now()
	remaining := grant.ValidUntil.Sub(now)
	if remaining <= 0 || remaining > maximum {
		return fmt.Errorf("invalid %s ownership deadline", u.Role)
	}
	if previous, ok := r.records[u.Name]; ok && previous.Generation == u.Generation {
		var old struct {
			Active     bool      `json:"active"`
			ValidUntil time.Time `json:"valid_until"`
		}
		if json.Unmarshal(previous.Snapshot, &old) == nil && old.Active && old.ValidUntil.Equal(grant.ValidUntil) && !r.leases[u.Name].IsZero() {
			return nil
		}
	}
	r.leases[u.Name] = now.Add(remaining)
	return nil
}

func (r *Runtime) Accept(ctx context.Context, u serviceconfig.Update) error {
	if u.Generation == 0 || u.Sequence == 0 || len(u.ProducerEpoch) != 32 {
		return errors.New("invalid producer generation, sequence or epoch")
	}
	if b, err := hex.DecodeString(u.ProducerEpoch); err != nil || len(b) != 16 {
		return errors.New("invalid producer epoch")
	}
	if u.Role == "bfd" {
		return r.acceptBFD(ctx, u)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bindings[u.Name]
	if !r.configured || !ok || b.instance.Role != u.Role || b.digest != u.ConfigDigest {
		return ErrStale
	}
	if old, ok := r.records[u.Name]; ok {
		if u.Generation < old.Generation || (u.Generation == old.Generation && u.ProducerEpoch != old.ProducerEpoch) {
			return ErrStale
		}
		if u.Generation == old.Generation {
			if u.Sequence < old.Sequence {
				return ErrStale
			}
			if u.Sequence == old.Sequence {
				if bytes.Equal(u.Snapshot, old.Snapshot) {
					return nil
				}
				return errors.New("protocol sequence reused with different snapshot")
			}
		}
	}
	if u.Role == "aftr" {
		if err := r.validateAFTRSnapshotLocked(b, u.Snapshot); err != nil {
			return err
		}
	} else if err := validateSnapshot(b.instance, u.Snapshot); err != nil {
		return err
	}
	previous, had := r.records[u.Name]
	oldLease := r.leases[u.Name]
	switch u.Role {
	case "vrrp", "bgp", "dhcp_server":
		maximum := 5 * time.Second
		if u.Role == "vrrp" {
			var c vrrp.Config
			_ = json.Unmarshal(b.instance.Config, &c)
			maximum = 4 * time.Duration(c.AdvertIntervalCentiseconds) * 10 * time.Millisecond
		}
		if err := r.recordOwnershipLocked(u, maximum); err != nil {
			return err
		}
	}
	r.records[u.Name] = u
	p, err := r.projectLocked(r.current, r.effectiveRecordsLocked(time.Now()), time.Now())
	if err == nil {
		err = r.applyNetworkLocked(ctx, p.network)
	}
	if err != nil {
		if oldLease.IsZero() {
			delete(r.leases, u.Name)
		} else {
			r.leases[u.Name] = oldLease
		}
		if had {
			r.records[u.Name] = previous
		} else {
			delete(r.records, u.Name)
		}
		old, compileErr := r.projectLocked(r.current, r.effectiveRecordsLocked(time.Now()), time.Now())
		if compileErr == nil {
			restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			compileErr = r.network.Apply(restoreCtx, r.revision, old.network)
			cancel()
		}
		r.appliedHash = ""
		r.phase = "degraded"
		r.lastError = errors.Join(err, compileErr).Error()
		return err
	}
	r.liveRA = p.liveRA
	r.routeIssues = p.routeIssues
	if u.Role == "bgp" {
		// A newly started recipient may read grants only after this first
		// publication has completed the existing network ACK boundary.
		r.acceptBFDRecipientLocked(u)
	}
	return nil
}
func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}
func validateSnapshot(instance serviceconfig.Instance, b json.RawMessage) error {
	switch instance.Role {
	case "bfd":
		var cfg bfd.Config
		var snapshot bfd.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		return bfd.ValidateSnapshot(cfg, snapshot)
	case "ra_client":
		var cfg raclient.Config
		var snapshot raclient.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		return raclient.ValidateSnapshot(cfg, snapshot)
	case "dhcp_server":
		var c dhcpserverconfig.Config
		var s dhcpserverconfig.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		return dhcpserverconfig.ValidateSnapshot(c, s)
	case "dhcp4":
		var c dhcp4.Config
		var s dhcp4.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if c.Resource != s.Resource || c.Interface != s.Interface {
			return errors.New("DHCPv4 snapshot identity mismatch")
		}
	case "dhcp6pd":
		var c dhcp6pd.Config
		var s dhcp6pd.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if c.Resource != s.Resource || c.Interface != s.Interface || c.IAID != s.IAID {
			return errors.New("DHCPv6 snapshot identity mismatch")
		}
	case "ra":
		var c ra.Config
		var s ra.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if c.Resource != s.Resource || c.Interface != s.Interface {
			return errors.New("RA snapshot identity mismatch")
		}
	case "bgp":
		var c bgpconfig.Config
		var s bgpconfig.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if s.Resource != c.Resource || len(s.Epoch) != 32 || s.Sequence == 0 {
			return errors.New("BGP snapshot identity mismatch")
		}
		if len(s.Routes) > c.MaxRoutes {
			return errors.New("BGP route limit exceeded")
		}
		if !s.Active && (!s.ValidUntil.IsZero() || len(s.Routes) > 0) {
			return errors.New("inactive BGP cannot retain route ownership")
		}
		if err := validateBFDAcknowledgements(c, s); err != nil {
			return err
		}
		for _, route := range s.Routes {
			if route.Table != c.Table || route.Metric != c.RouteMetric {
				return errors.New("BGP route changed configured table or metric")
			}
			if _, ok := c.Runtime.Peers[route.PeerAddress]; !ok {
				return errors.New("BGP selected route has an unconfigured peer")
			}
			hint := c.PeerTransports[route.PeerAddress].FIBInterface
			if hint == "" {
				hint = c.FIBInterface
			}
			if route.Interface != hint {
				return errors.New("BGP route changed configured egress interface")
			}
		}
	case "vrrp":
		var c vrrp.Config
		var s vrrp.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if c.Resource != s.Resource || c.Interface != s.Interface || c.VIPInterface != s.VIPInterface || c.VRF != s.VRF || c.Family != s.Family || c.VirtualRouterID != s.VirtualRouterID || c.Priority != s.Priority || c.AcceptMode != s.AcceptMode || c.AddressOwner != s.AddressOwner || s.VirtualMAC != vrrp.VirtualMAC(c.Family, c.VirtualRouterID).String() {
			return errors.New("VRRP snapshot identity or ownership policy mismatch")
		}
		x, _ := json.Marshal(c.Addresses)
		y, _ := json.Marshal(s.Addresses)
		if !bytes.Equal(x, y) || c.SourceAddress != "" && s.SourceAddress != c.SourceAddress {
			return errors.New("VRRP snapshot address set mismatch")
		}
		if s.Active != (s.State == vrrp.StateActive) || s.State != vrrp.StateActive && s.State != vrrp.StateBackup && s.State != vrrp.StateInitialize {
			return errors.New("invalid VRRP state")
		}
		if !s.Active && !s.ValidUntil.IsZero() {
			return errors.New("inactive VRRP state cannot retain an ownership lease")
		}
	default:
		return errors.New("unsupported protocol snapshot")
	}
	return nil
}

func (r *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	r.observationRoutes(mux)
	r.aftrRoutes(mux)
	r.bfdRoutes(mux)
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, req *http.Request) { localapi.JSON(w, 200, r.State()) })
	mux.HandleFunc("POST /v1/services/state", func(w http.ResponseWriter, req *http.Request) {
		var u serviceconfig.Update
		if err := localapi.Decode(w, req, &u); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		if err := r.Accept(req.Context(), u); err != nil {
			code := 422
			if errors.Is(err, ErrStale) {
				code = 409
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, map[string]bool{"accepted": true})
	})
	mux.HandleFunc("GET /v1/services/{name}/dns-leases", func(w http.ResponseWriter, req *http.Request) {
		projection, err := r.DNSLeases(req.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 409, err)
			return
		}
		localapi.JSON(w, 200, projection)
	})
	mux.HandleFunc("GET /v1/services/{name}/ra", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		c, ok := r.liveRA[req.PathValue("name")]
		r.mu.Unlock()
		if !ok {
			localapi.Fail(w, 404, errors.New("no current RA projection"))
			return
		}
		localapi.JSON(w, 200, c)
	})
	return mux
}

// Monitor expires facts from wall-clock deadlines even if a worker is killed,
// and creates/retires PD-derived RA instances. Configuration transactions have
// priority; no protocol callback waits for process startup while holding mu.
func (r *Runtime) Monitor(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.op.TryLock() {
				continue
			}
			err := r.reconcileDynamic(ctx)
			r.op.Unlock()
			if err != nil {
				r.failure(err)
			}
		}
	}
}
func (r *Runtime) reconcileDynamic(ctx context.Context) error {
	r.mu.Lock()
	if !r.configured {
		r.mu.Unlock()
		return nil
	}
	p, err := r.projectLocked(r.current, r.effectiveRecordsLocked(time.Now()), time.Now())
	if err == nil {
		err = r.applyNetworkLocked(ctx, p.network)
	}
	if err == nil {
		r.liveRA = p.liveRA
		r.routeIssues = p.routeIssues
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	bindings, modules, err := r.prepareIfChanged(p.instances)
	if err != nil {
		return err
	}
	if bindings == nil {
		if err := r.CheckReady(ctx); err != nil {
			return err
		}
		r.mu.Lock()
		r.phase = "ready"
		r.lastError = ""
		r.mu.Unlock()
		return nil
	}
	if err := r.quiesceChanged(ctx, modules); err != nil {
		return err
	}
	r.mu.Lock()
	bfdSources, bfdRecipients, err := compileBFDCache(bindings)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	r.bindings = bindings
	r.installBFDCacheLocked(bfdSources, bfdRecipients)
	for name, input := range r.aftrInputs {
		if b, ok := bindings[name]; !ok || b.digest != input.ConfigDigest {
			delete(r.aftrInputs, name)
		}
	}
	for name := range r.records {
		if _, ok := bindings[name]; !ok {
			delete(r.records, name)
		}
	}
	r.mu.Unlock()
	if err := r.manager.Reconcile(ctx, modules); err != nil {
		return err
	}
	r.installed = modules
	return nil // readiness is visible through the supervisor's required modules
}
func (r *Runtime) prepareIfChanged(v []serviceconfig.Instance) (map[string]binding, []supervisor.DynamicModule, error) {
	r.mu.Lock()
	same := len(v) == len(r.bindings)
	for _, instance := range v {
		_, digest, err := encodeInstance(instance)
		if err != nil {
			r.mu.Unlock()
			return nil, nil, err
		}
		if b, ok := r.bindings[instance.Name]; !ok || b.digest != digest {
			same = false
		}
	}
	r.mu.Unlock()
	if same {
		return nil, nil, nil
	}
	return r.prepare(v)
}
