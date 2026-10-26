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
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/network"
	"github.com/imksoo/routerd2/internal/protocol/aftr"
	"github.com/imksoo/routerd2/internal/protocol/bfd"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcp4"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6pd"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
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
	ProjectionGeneration uint64           `json:"projection_generation"`
	RouteIssueCount      int              `json:"route_issue_count"`
	RouteIssues          []RouteIssue     `json:"route_issues,omitempty"`
	Revision             uint64           `json:"revision"`
	Phase                string           `json:"phase"`
	LastError            string           `json:"last_error,omitempty"`
	Instances            []InstanceStatus `json:"instances"`
}
type InstanceStatus struct {
	SupervisorInstance string           `json:"supervisor_instance,omitempty"`
	BFD                *BFDHealthStatus `json:"bfd_health,omitempty"`
	SnapshotBytes      int              `json:"snapshot_bytes,omitempty"`
	SnapshotTruncated  bool             `json:"snapshot_truncated,omitempty"`
	OwnershipExpired   bool             `json:"ownership_expired,omitempty"`
	Name               string           `json:"name"`
	Role               string           `json:"role"`
	ConfigDigest       string           `json:"config_digest"`
	Generation         uint64           `json:"generation,omitempty"`
	ProducerEpoch      string           `json:"producer_epoch,omitempty"`
	Sequence           uint64           `json:"sequence,omitempty"`
	Snapshot           json.RawMessage  `json:"snapshot,omitempty"`
}
type configuration struct {
	source   config.Document
	plan     resourceprogram.Plan
	digest   string
	revision uint64
	vrrp     map[string]vrrp.Config
}

type appliedProjection struct {
	untilNanos         uint64 // earliest original network deadline in the acknowledged projection
	hash               string
	at                 time.Time
	sourceEpoch        uint64
	observationVersion uint64
}

type Runtime struct {
	config             configuration
	applied            appliedProjection
	reconcileWake      chan struct{}
	waitWake           chan struct{}
	supervisorInstance string              // immutable authority inherited by the control worker
	op                 *semaphore.Weighted // module-set reconciliation; never held by protocol callbacks
	mu                 sync.Mutex          // observations and projection metadata; released during kernel IO
	// BFD packet observations and recipient queries use their own short lock.
	// Configuration takes mu -> bfdMu, never the opposite order.
	bfdMu                sync.Mutex
	bfdSources           map[string]bfdBinding
	bfdRecipients        map[string]bfdRecipient
	dir, stateDir        string
	network              Network
	manager              moduleManager
	sourceEpoch          uint64
	resourceStates       map[string]resourceprogram.ResourceState
	guardStatus          map[string]supervisor.ModuleStatus
	guardUntil           time.Time
	guardBootID          string
	guardUntilNanos      uint64
	guardGrants          map[string]guardGrant
	conditionTimeline    resourceprogram.ConditionTimeline
	ruleContinuity       map[string]ruleContinuity
	ruleIncarnation      string
	derivedContinuity    map[string]derivedContinuity
	projectionGeneration uint64
	observationVersion   uint64
	configured           bool
	initialized          bool
	bindings             map[string]binding
	records              map[string]serviceconfig.Update
	applications         map[string]*pendingApplication
	vrrpOwners           map[string]vrrpOwnership
	vrrpTracks           map[string][]vrrpTrackState
	liveRA               map[string]ra.Config
	aftrInputs           map[string]aftr.Input
	dhcpServers          map[string]dhcpserverconfig.Snapshot
	routeIssues          []RouteIssue
	bgpApplied           map[string]bgpProjectionStatus
	phase, lastError     string
}

func New(dir, stateDir string, n Network, supervisorInstance string) *Runtime {
	return &Runtime{reconcileWake: make(chan struct{}, 1), waitWake: make(chan struct{}, 1), op: semaphore.NewWeighted(1), supervisorInstance: supervisorInstance, dir: dir, stateDir: stateDir, network: n, manager: managerClient{localapi.NewClient(filepath.Join(dir, supervisor.SocketName))}, bindings: map[string]binding{}, records: map[string]serviceconfig.Update{}, liveRA: map[string]ra.Config{}, dhcpServers: map[string]dhcpserverconfig.Snapshot{}, phase: "starting"}
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
	if r.applied.hash == "" || r.applied.sourceEpoch != r.sourceEpoch || r.applied.observationVersion != r.observationVersion {
		return errors.New("accepted protocol observations await network application")
	}
	if r.applied.untilNanos != 0 {
		clock, err := boottime.Read()
		if err != nil {
			return err
		}
		if uint64(clock.BoottimeNanoseconds) >= r.applied.untilNanos {
			r.wakeCoordinatorLocked()
			return errors.New("applied network authority expired; current projection awaits application")
		}
	}
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
		if binding.instance.Role == "vrrp" {
			var ownership vrrp.Snapshot
			if record, exists := r.records[name]; !exists || json.Unmarshal(record.Snapshot, &ownership) != nil || ownership.State == vrrp.StateActive && !r.ownsVRRPLocked(record, ownership) {
				return fmt.Errorf("%w: %s", ErrVRRPActivationPending, name)
			}
		}
	}
	if dynamicProgram(r.config.source) {
		r.installGuardStatusLocked(s, nil)
		p, err := r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
		if err != nil {
			return err
		}
		for id, state := range p.resources {
			if state.Active && state.Ready != resourceprogram.True {
				return fmt.Errorf("resource %s ready_when is not true", id)
			}
		}
		// A new prerequisite may have released dependents since the last
		// network ACK. Confirmation waits until that projection is applied.
		if !reflect.DeepEqual(p.resources, r.resourceStates) {
			return errors.New("resource projection has not converged")
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
// The control engine owns immutable source data for the plan lifetime.
// Protocol facts can change its projection, but cannot replace operator intent.
func (r *Runtime) ApplyConfig(ctx context.Context, revision uint64, source config.Document) error {
	plan, err := config.Compile(source)
	if err != nil {
		return err
	}
	doc := plan.Native
	encoded, _ := json.Marshal(source)
	sourceSum := sha256.Sum256(encoded)
	sourceDigest := hex.EncodeToString(sourceSum[:])
	next := configuration{source: source, plan: plan, digest: sourceDigest, revision: revision}
	next.vrrp = make(map[string]vrrp.Config)
	// Static identities depend only on this compiled configuration and our
	// private paths. Prepare them once, outside the observation lock; retries
	// after worker shutdown only need to select the latest matching records.
	expected := make(map[string]string, len(doc.Services))
	for _, service := range doc.Services {
		instance, err := service.Instance(r.dir, r.stateDir)
		if err != nil {
			return err
		}
		_, digest, err := encodeInstance(instance)
		if err != nil {
			return err
		}
		expected[instance.Name] = digest
		if service.VRRP != nil {
			next.vrrp[service.ModuleName()] = service.VRRPConfig()
		}
	}
	if err := r.op.Acquire(ctx, 1); err != nil {
		return r.failure(err)
	}
	defer r.op.Release(1)
	if !r.initialized {
		// Control-worker recovery is deliberately explicit: quiesce previous
		// dynamic workers, then reacquire their protocol state. Continuing an
		// old master/lease without an accepted producer is not recovery.
		if err := r.wait(ctx, map[string]supervisor.DynamicModule{}); err != nil {
			return r.failure(err)
		}
		r.initialized = true
	}
	var retained map[string]serviceconfig.Update
	var p projection
	var bindings map[string]binding
	var instances instanceSet
	var bfdSources map[string]bfdBinding
	var bfdRecipients map[string]bfdRecipient
	for {
		if err := ctx.Err(); err != nil {
			return r.failure(err)
		}
		r.mu.Lock()
		retained = r.retainedLocked(expected)
		p, err = r.projectProgramLocked(next, retained, time.Now())
		r.mu.Unlock()
		if err != nil {
			return r.failure(err)
		}
		// Preparing immutable files and BFD bindings does not own observations.
		// Finish it before stopping workers so a write error leaves them running.
		instances, err = compileInstances(p.instances, nil)
		if err == nil {
			err = r.prepare(instances)
		}
		bindings = instances.bindings
		if err == nil {
			bfdSources, bfdRecipients, err = compileBFDCache(bindings)
		}
		if err != nil {
			return r.failure(err)
		}
		ceiling := make(map[string]supervisor.DynamicModule, len(instances.modules))
		for _, module := range instances.modules {
			ceiling[module.Name] = module
		}
		if err := r.wait(ctx, ceiling); err != nil {
			return r.failure(err)
		}
		r.mu.Lock()
		// Preserve renewals received during preparation and stopping. Only a
		// changed instance set needs new files and another stop pass.
		retained = r.retainedLocked(expected)
		latest, err := r.projectProgramLocked(next, retained, time.Now())
		if err != nil {
			r.mu.Unlock()
			return r.failure(err)
		}
		if !reflect.DeepEqual(latest.instances, p.instances) {
			r.mu.Unlock()
			continue
		}
		break // cut over while still holding mu
	}
	if r.config.revision != revision || r.config.digest != sourceDigest {
		if r.sourceEpoch == math.MaxUint64 {
			r.mu.Unlock()
			return r.failure(errors.New("source cutover epoch exhausted"))
		}
		for name := range r.applications {
			r.finishApplicationLocked(name, ErrStale)
		}
		r.sourceEpoch++
		r.guardGrants = nil
		r.vrrpTracks = nil
		r.guardStatus = nil
		r.guardUntil = time.Time{}
	}
	r.config = next
	r.configured = true
	r.records = retained
	r.bindings = bindings
	for name := range r.dhcpServers {
		if record, ok := retained[name]; !ok || record.Role != "dhcp_server" {
			delete(r.dhcpServers, name)
		}
	}
	r.installBFDCacheLocked(bfdSources, bfdRecipients)
	for name, input := range r.aftrInputs {
		if b, ok := bindings[name]; !ok || b.digest != input.ConfigDigest {
			delete(r.aftrInputs, name)
		}
	}
	r.phase, r.lastError = "applying", ""
	r.mu.Unlock()
	if err := r.wait(ctx, nil); err != nil {
		return r.failure(err)
	}
	r.reportReadiness(ctx)
	return nil
}

// Caller holds mu. Configuration equality includes all protocol-owned paths.
func (r *Runtime) retainedLocked(expected map[string]string) map[string]serviceconfig.Update {
	retained := map[string]serviceconfig.Update{}
	effective := r.observationsLocked()
	for name, digest := range expected {
		if old, ok := r.bindings[name]; ok && old.digest == digest {
			if value, ok := effective[name]; ok {
				retained[name] = value
			}
		}
	}
	return retained
}

// Caller holds mu. The envelopes and their bodies remain the original accepted
// observations; consumers evaluate eligibility without rewriting source state.
func (r *Runtime) observationsLocked() map[string]serviceconfig.Update {
	out := make(map[string]serviceconfig.Update, len(r.records))
	for name, record := range r.records {
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

// An instance set keeps canonical wire bytes only through preparation. The
// runtime retains typed bindings, not another serialized configuration cache.
type instanceSet struct {
	bindings map[string]binding
	modules  []supervisor.DynamicModule
	encoded  [][]byte
}

func compileInstances(instances []serviceconfig.Instance, ceiling map[string]supervisor.DynamicModule) (instanceSet, error) {
	set := instanceSet{bindings: make(map[string]binding, len(instances))}
	for _, instance := range instances {
		if ceiling != nil {
			if _, ok := ceiling[instance.Name]; !ok {
				continue
			}
		}
		encoded, digest, err := encodeInstance(instance)
		if err != nil {
			return instanceSet{}, err
		}
		module := supervisor.DynamicModule{Name: instance.Name, Role: instance.Role, ConfigDigest: digest}
		if ceiling != nil && ceiling[instance.Name] != module {
			continue
		}
		set.bindings[instance.Name] = binding{instance, digest}
		set.modules = append(set.modules, module)
		set.encoded = append(set.encoded, encoded)
	}
	return set, nil
}

func (r *Runtime) prepare(set instanceSet) error {
	for i, module := range set.modules {
		dir := filepath.Join(r.dir, "instances", module.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := config.AtomicWrite(filepath.Join(dir, module.ConfigDigest+".json"), set.encoded[i]); err != nil {
			return err
		}
	}
	return nil
}

// A ceiling restricts the old source while a new configuration is preparing.
// A nil ceiling runs the current source normally; an empty one drains recovery.
// Startup and shutdown both advance the same step, including effect callbacks.
func (r *Runtime) wait(ctx context.Context, ceiling map[string]supervisor.DynamicModule) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		done, err := r.reconcileStep(ctx, ceiling)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("module reconciliation: %w", ctx.Err())
		case <-ticker.C:
		case <-r.waitWake:
		}
	}
}
func (r *Runtime) applyNetworkLocked(ctx context.Context, d network.Desired) error {
	b, _ := json.Marshal(struct {
		Revision uint64
		Desired  network.Desired
	}{r.config.revision, d})
	h := sha256.Sum256(b)
	digest := hex.EncodeToString(h[:])
	if digest == r.applied.hash && time.Since(r.applied.at) < 5*time.Second {
		return nil
	}
	previousHash := r.applied.hash
	r.applied.hash = ""
	revision := r.config.revision
	r.mu.Unlock()
	err := r.network.Apply(ctx, revision, d)
	r.mu.Lock()
	if err != nil {
		r.bgpApplied = nil
		return err
	}
	if digest != previousHash {
		r.projectionGeneration++
	}
	r.applied.hash = digest
	r.applied.at = time.Now()
	return nil
}

var ErrStale = errors.New("stale protocol producer or configuration")

// Observe acknowledges a validated observation. Kernel application belongs to
// the serialized coordinator, independently of whether the source has guards.
func (r *Runtime) Observe(ctx context.Context, u serviceconfig.Update) error {
	// This check precedes both the main mutex and the independent BFD path.
	// Old counters cannot seed an empty cache after a supervisor restart.
	if !serviceconfig.ValidSupervisorInstance(r.supervisorInstance) || u.SupervisorInstance != r.supervisorInstance {
		return ErrStale
	}
	if u.Generation == 0 || u.Sequence == 0 || len(u.ProducerEpoch) != 32 {
		return errors.New("invalid producer generation, sequence or epoch")
	}
	if b, err := hex.DecodeString(u.ProducerEpoch); err != nil || len(b) != 16 {
		return errors.New("invalid producer epoch")
	}
	if u.Role == "bfd" {
		return r.acceptBFD(ctx, u)
	}
	var producerStatus *supervisor.Status
	if u.Role == "bgp" || u.Role == "dhcp4" || u.Role == "vrrp" || u.Role == "dhcp_server" {
		// A restarted control has no remembered generations. Authenticate even
		// its first publication against the current process-tree registry;
		// initial publication precedes worker Ready, so Ready is not required.
		status, err := r.manager.Status(ctx)
		if err != nil {
			return err
		}
		matched := false
		if status.InstanceID == r.supervisorInstance && !status.Stopping {
			for _, module := range status.Modules {
				if module.Name == u.Name && module.Role == u.Role && module.Generation == u.Generation && module.ConfigDigest == u.ConfigDigest {
					matched = true
					break
				}
			}
		}
		if !matched {
			return ErrStale
		}
		if u.Role == "vrrp" {
			producerStatus = &status
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if producerStatus != nil {
		r.installGuardStatusLocked(*producerStatus, nil)
	}
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
	var server dhcpserverconfig.Snapshot
	if u.Role == "dhcp_server" {
		var err error
		server, err = r.validateDHCPServerSnapshotLocked(b, u)
		if err != nil {
			return err
		}
	} else if u.Role == "aftr" {
		if err := r.validateAFTRSnapshotLocked(b, u.Snapshot); err != nil {
			return err
		}
	} else if err := validateSnapshot(b.instance, u.Snapshot); err != nil {
		return err
	}
	previous := r.records[u.Name]
	if u.Role == "vrrp" {
		if err := validateVRRPTransition(previous, u); err != nil {
			return err
		}
	}
	if u.Role == "dhcp4" {
		if err := validateDHCP4Transition(previous, u); err != nil {
			return err
		}
	}
	if u.Role == "bgp" {
		if err := validateBGPTransition(previous, u); err != nil {
			return err
		}
	}
	candidateRecords := r.observationsLocked()
	candidateRecords[u.Name] = u
	if _, err := r.projectLocked(r.config.plan.Native, candidateRecords, time.Now(), resourceprogram.FactSnapshot{}); err != nil {
		return err
	}
	if u.Role == "dhcp_server" {
		r.dhcpServers[u.Name] = server
	}
	r.recordGuardGrantLocked(u, time.Now())
	r.records[u.Name] = u
	r.observationVersion++
	// Observe flaps when accepted; a periodic projection must not hide them.
	if _, err := r.conditionFactsLocked(r.config, time.Now()); err != nil {
		return err
	}
	r.wakeCoordinatorLocked()
	return nil
}

// Apply waits for the current BGP FIB or VRRP VIP projection. These protocols
// require application completion before advancing their state machines.
func (r *Runtime) Apply(ctx context.Context, u serviceconfig.Update) (serviceconfig.ApplicationReceipt, error) {
	if u.Role != "vrrp" && u.Role != "bgp" {
		return serviceconfig.ApplicationReceipt{}, errors.New("application requests support only BGP and VRRP")
	}
	if err := r.Observe(ctx, u); err != nil {
		return serviceconfig.ApplicationReceipt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.records[u.Name]
	binding, bound := r.bindings[u.Name]
	if !ok || !bound || binding.digest != u.ConfigDigest || !sameUpdate(current, u) {
		return serviceconfig.ApplicationReceipt{}, ErrStale
	}
	return r.waitApplicationLocked(ctx, u)
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
	case "health":
		var cfg health.Config
		var snapshot health.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		clock, err := bfd.ReadClock()
		if err != nil {
			return err
		}
		return health.ValidateSnapshot(cfg, snapshot, clock)
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
	case "dhcp4":
		var c dhcp4.Config
		var s dhcp4.Snapshot
		if err := strict(instance.Config, &c); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		return dhcp4.ValidateSnapshot(c, s)
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
		if err := bgpconfig.ValidateObservation(s); err != nil {
			return err
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
		return vrrp.ValidateSnapshot(c, s)
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
		if u.Role == "vrrp" || u.Role == "bgp" {
			localapi.Fail(w, 422, errors.New("BGP and VRRP must use the effect application endpoint"))
			return
		}
		if err := r.Observe(req.Context(), u); err != nil {
			code := 422
			if errors.Is(err, ErrStale) {
				code = 409
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, serviceconfig.ObservationReceipt{Accepted: true})
	})
	mux.HandleFunc("POST /v1/services/effects", func(w http.ResponseWriter, req *http.Request) {
		var u serviceconfig.Update
		if err := localapi.Decode(w, req, &u); err != nil {
			localapi.Fail(w, 400, err)
			return
		}
		receipt, err := r.Apply(req.Context(), u)
		if err != nil {
			code := 422
			if errors.Is(err, ErrStale) {
				code = 409
			}
			localapi.Fail(w, code, err)
			return
		}
		localapi.JSON(w, 200, receipt)
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

// Monitor expires facts under each protocol's clock contract if a worker stops,
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
		case <-r.reconcileWake:
		}
		if !r.op.TryAcquire(1) {
			continue
		}
		err := r.reconcileDynamic(ctx)
		r.op.Release(1)
		if err != nil {
			r.failure(err)
		}
	}
}

func (r *Runtime) reconcileDynamic(ctx context.Context) error {
	if _, err := r.reconcileStep(ctx, nil); err != nil {
		return err
	}
	r.reportReadiness(ctx)
	return nil
}

// Application completion and readiness differ: an elected graceful MASTER may
// still be waiting for admission. Configuration and monitoring report the same
// current readiness rather than advertising Ready from a successful write alone.
func (r *Runtime) reportReadiness(ctx context.Context) {
	if err := r.CheckReady(ctx); err != nil {
		r.failure(err)
		return
	}
	r.mu.Lock()
	r.phase, r.lastError = "ready", ""
	r.mu.Unlock()
}
