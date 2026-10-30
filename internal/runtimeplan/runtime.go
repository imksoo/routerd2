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
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6serverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/health"
	"github.com/imksoo/routerd2/internal/protocol/ntpconfig"
	"github.com/imksoo/routerd2/internal/protocol/pppoeconfig"
	"github.com/imksoo/routerd2/internal/protocol/ra"
	"github.com/imksoo/routerd2/internal/protocol/raclient"
	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
	"github.com/imksoo/routerd2/internal/protocol/vrrp"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
	"github.com/imksoo/routerd2/internal/supervisor"
)

type Network interface {
	AddressSets(context.Context) (network.AddressSetSnapshot, error)
	Apply(context.Context, uint64, network.Desired) error
	SourceAddress(context.Context, string, string) (network.SourceObservation, error)
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
	encoded  []byte // canonical child-file bytes, owned by the immutable protocol plan
	dhcp6    *dhcp6config.Config
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
	source    config.Document
	resources map[string]resourceprogram.Resource
	plan      resourceprogram.Plan
	digest    string
	revision  uint64
	protocols protocolPlan
}

type appliedProjection struct {
	untilNanos         uint64           // earliest original network deadline in the acknowledged projection
	network            *network.Desired // immutable completed projection, shared read-only with Apply
	at                 time.Time
	sourceEpoch        uint64
	observationVersion uint64
}

type supervisorObservation struct {
	modules       map[string]supervisor.ModuleStatus
	ready         bool
	bootID        string
	observedNanos uint64
	untilNanos    uint64
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
	guard                supervisorObservation
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
	egressSelections     map[string]resourceprogram.EgressSelection
	hostSources          map[string]network.SourceObservation
	vrrpTracks           map[string][]vrrpTrackState
	liveRA               map[string]ra.Config
	liveDHCP6            map[string]dhcp6config.RouterFlags
	delegatedDNS         map[string][]delegatedDNSAddress
	aftrInputs           map[string]aftr.Input
	dhcpServers          map[string]dhcpserverconfig.Snapshot
	routeIssues          []RouteIssue
	bgpApplied           map[string]bgpProjectionStatus
	phase, lastError     string
}

func New(dir, stateDir string, n Network, supervisorInstance string) *Runtime {
	return &Runtime{reconcileWake: make(chan struct{}, 1), waitWake: make(chan struct{}, 1), op: semaphore.NewWeighted(1), supervisorInstance: supervisorInstance, dir: dir, stateDir: stateDir, network: n, manager: managerClient{localapi.NewClient(filepath.Join(dir, supervisor.SocketName))}, bindings: map[string]binding{}, records: map[string]serviceconfig.Update{}, liveRA: map[string]ra.Config{}, dhcpServers: map[string]dhcpserverconfig.Snapshot{}, phase: "starting"}
}

var errApplicationPending = errors.New("accepted protocol observations await network application")

// CheckReady queries the process manager at confirmation time. A previously
// successful configuration cannot be confirmed while one of its required
// protocol processes has since crashed or entered restart backoff.
func (r *Runtime) CheckReady(ctx context.Context) error {
	r.mu.Lock()
	epoch, revision := r.sourceEpoch, r.config.revision
	var namedSets []string
	if r.applied.network != nil && r.applied.network.Firewall != nil {
		firewall := r.applied.network.Firewall
		for _, set := range firewall.AddressSets {
			if len(set.Names) == 0 {
				continue
			}
			for _, redirect := range firewall.LocalRedirects {
				if redirect.DestinationSet == set.Name {
					namedSets = append(namedSets, set.Name)
					break
				}
			}
		}
	}
	r.mu.Unlock()
	var membership network.AddressSetSnapshot
	if len(namedSets) > 0 {
		var err error
		membership, err = r.network.AddressSets(ctx)
		if err != nil {
			return err
		}
	}
	s, err := r.manager.Status(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(namedSets) > 0 {
		if epoch != r.sourceEpoch || revision != membership.Revision {
			return errApplicationPending
		}
		for _, name := range namedSets {
			found := false
			for _, set := range membership.Sets {
				if set.Name == name {
					found = !set.ResolvedAt.IsZero() && set.Error == ""
					break
				}
			}
			if !found {
				return fmt.Errorf("address set %s membership is unresolved or stale; inspect network address sets", name)
			}
		}
	}
	r.installGuardStatusLocked(s, nil)
	if !r.guard.ready {
		return errors.New("required routerd modules are not ready")
	}
	if err := r.checkAppliedLocked(); err != nil {
		return err
	}

	if len(r.routeIssues) > 0 {
		return errors.New("some learned routes have unresolved next hops; inspect show services")
	}
	for name, binding := range r.bindings {
		module, ok := r.guard.modules[name]
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
		p, err := r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
		if err != nil {
			return err
		}
		if len(p.resources) != len(r.resourceStates) {
			return errApplicationPending
		}
		for id, state := range p.resources {
			if state.Active && state.Ready != resourceprogram.True {
				return fmt.Errorf("resource %s is not ready: %s", id, state.Reason)
			}
			applied, ok := r.resourceStates[id]
			// Selection must be applied; freshness alone may advance while readiness
			// is checked. The acknowledged network deadline is checked above.
			state.ActiveUntil = applied.ActiveUntil
			if !ok || state != applied {
				return errApplicationPending
			}
		}
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	if r.guard.bootID != clock.BootID || uint64(clock.BoottimeNanoseconds) >= r.guard.untilNanos {
		return errors.New("supervisor status validity expired")
	}
	return nil
}

// A notification counter also advances for history and unchanged heartbeats.
// Readiness depends on their effects, not on silence from every producer.
func (r *Runtime) checkAppliedLocked() error {
	if r.applied.network == nil || r.applied.sourceEpoch != r.sourceEpoch {
		return errApplicationPending
	}
	if r.applied.observationVersion != r.observationVersion {
		p, err := r.projectProgramLocked(r.config, r.observationsLocked(), time.Now())
		if err != nil {
			return err
		}
		if !networkCovered(*r.applied.network, p.network) || len(p.instances) != len(r.bindings) || len(p.resources) != len(r.resourceStates) {
			return errApplicationPending
		}
		for _, b := range p.instances {
			if r.bindings[b.instance.Name].digest != b.digest {
				return errApplicationPending
			}
		}
		for id, state := range p.resources {
			applied, ok := r.resourceStates[id]
			state.ActiveUntil = applied.ActiveUntil
			if !ok || state != applied {
				return errApplicationPending
			}
		}
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
	next.resources = make(map[string]resourceprogram.Resource, len(source.Resources))
	next.protocols, err = prepareProtocols(doc, r.dir, r.stateDir)
	if err != nil {
		return err
	}
	for _, resource := range source.Resources {
		next.resources[resource.ID] = resource
		if resource.Service != nil && resource.Service.DHCP6Server != nil {
			name := resource.Service.ModuleName()
			server := next.protocols.dhcp6Servers[name]
			server.resourceID = resource.ID
			next.protocols.dhcp6Servers[name] = server
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
	// Every worker variant belongs to this source, including inactive services
	// and both HA roles. Prepare once before retiring any current worker.
	if err := r.prepare(next.protocols); err != nil {
		return r.failure(err)
	}
	for {
		if err := ctx.Err(); err != nil {
			return r.failure(err)
		}
		r.mu.Lock()
		retained = r.retainedLocked(next.protocols.instances)
		p, err = r.projectProgramLocked(next, retained, time.Now())
		r.mu.Unlock()
		if err != nil {
			return r.failure(err)
		}
		instances = selectInstances(p.instances, nil)
		bindings = instances.bindings
		bfdSources, bfdRecipients, err = compileBFDCache(bindings)
		if err != nil {
			return r.failure(err)
		}
		ceiling := make(map[string]supervisor.DynamicModule, len(instances.modules))
		for _, module := range instances.modules {
			ceiling[module.Name] = module
		}
		r.mu.Lock()
		r.drainDependenciesLocked(ceiling)
		r.mu.Unlock()
		if err := r.wait(ctx, ceiling); err != nil {
			return r.failure(err)
		}
		r.mu.Lock()
		// Preserve renewals received during preparation and stopping. Only a
		// changed instance set needs new files and another stop pass.
		retained = r.retainedLocked(next.protocols.instances)
		latest, err := r.projectProgramLocked(next, retained, time.Now())
		if err != nil {
			r.mu.Unlock()
			return r.failure(err)
		}
		if !sameInstances(latest.instances, p.instances) {
			r.mu.Unlock()
			continue
		}
		break // cut over while still holding mu
	}
	previousGrants := r.guardGrants
	if r.config.revision != revision || r.config.digest != sourceDigest {
		if r.sourceEpoch == math.MaxUint64 {
			r.mu.Unlock()
			return r.failure(errors.New("source cutover epoch exhausted"))
		}
		for name := range r.applications {
			r.finishApplicationLocked(name, ErrStale)
		}
		r.sourceEpoch++
		r.egressSelections = nil
		r.hostSources = nil
		r.guardGrants = nil
		r.vrrpTracks = nil
		r.guard = supervisorObservation{}
	}
	r.config = next
	r.configured = true
	r.records = retained
	r.bindings = bindings
	r.guardGrants = make(map[string]guardGrant, len(retained))
	for name := range retained {
		if grant, ok := previousGrants[name]; ok {
			grant.revision = revision
			r.guardGrants[name] = grant
		}
	}
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
func (r *Runtime) retainedLocked(expected map[string]binding) map[string]serviceconfig.Update {
	retained := map[string]serviceconfig.Update{}
	effective := r.observationsLocked()
	for name, desired := range expected {
		if old, ok := r.bindings[name]; ok && old.digest == desired.digest {
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

func encodeInstance(v serviceconfig.Instance) (binding, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return binding{}, err
	}
	b = append(b, '\n')
	sum := sha256.Sum256(b)
	bound := binding{instance: v, digest: hex.EncodeToString(sum[:]), encoded: b}
	if v.Role == "dhcp6" {
		var cfg dhcp6config.Config
		if err := strict(v.Config, &cfg); err != nil {
			return binding{}, err
		}
		bound.dhcp6 = &cfg
	}
	return bound, nil
}

// An instance set selects the current workers from the source's prepared plan.
type instanceSet struct {
	bindings map[string]binding
	modules  []supervisor.DynamicModule
}

func selectInstances(instances []binding, ceiling map[string]supervisor.DynamicModule) instanceSet {
	set := instanceSet{bindings: make(map[string]binding, len(instances))}
	for _, bound := range instances {
		instance := bound.instance
		if ceiling != nil {
			if _, ok := ceiling[instance.Name]; !ok {
				continue
			}
		}
		module := supervisor.DynamicModule{Name: instance.Name, Role: instance.Role, ConfigDigest: bound.digest}
		if ceiling != nil && ceiling[instance.Name] != module {
			continue
		}
		set.bindings[instance.Name] = bound
		set.modules = append(set.modules, module)
	}
	return set
}

func (r *Runtime) prepare(plan protocolPlan) error {
	write := func(bound binding) error {
		dir := filepath.Join(r.dir, "instances", bound.instance.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		return config.AtomicWrite(filepath.Join(dir, bound.digest+".json"), bound.encoded)
	}
	for _, bound := range plan.instances {
		if err := write(bound); err != nil {
			return err
		}
	}
	for _, ha := range plan.leaseHA {
		if err := write(ha.serving); err != nil {
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
	matching := r.applied.sourceEpoch == r.sourceEpoch && reflect.DeepEqual(r.applied.network, &d)
	if matching && time.Since(r.applied.at) < 5*time.Second {
		return nil
	}
	// A refresh does not invalidate the completed projection's live authority.
	// Readiness compares its effects with current intent until this call ends.
	revision := r.config.revision
	r.mu.Unlock()
	err := r.network.Apply(ctx, revision, d)
	r.mu.Lock()
	if err != nil {
		r.applied.network = nil
		r.bgpApplied = nil
		return err
	}
	if !matching {
		r.projectionGeneration++
	}
	r.applied.network = &d
	r.applied.at = time.Now()
	return nil
}

var ErrStale = errors.New("stale protocol producer or configuration")

// Observe acknowledges a validated observation. Kernel application belongs to
// the serialized coordinator, independently of whether the source has guards.
func (r *Runtime) Observe(ctx context.Context, u serviceconfig.Update) error {
	_, err := r.observe(ctx, u)
	return err
}

func (r *Runtime) observe(ctx context.Context, u serviceconfig.Update) (*pendingApplication, error) {
	var effect *pendingApplication
	// This check precedes both the main mutex and the independent BFD path.
	// Old counters cannot seed an empty cache after a supervisor restart.
	if !serviceconfig.ValidSupervisorInstance(r.supervisorInstance) || u.SupervisorInstance != r.supervisorInstance {
		return nil, ErrStale
	}
	if u.Generation == 0 || u.Sequence == 0 || len(u.ProducerEpoch) != 32 {
		return nil, errors.New("invalid producer generation, sequence or epoch")
	}
	if b, err := hex.DecodeString(u.ProducerEpoch); err != nil || len(b) != 16 {
		return nil, errors.New("invalid producer epoch")
	}
	if u.Role == "bfd" {
		return nil, r.acceptBFD(ctx, u)
	}
	var producerStatus *supervisor.Status
	if u.Role == "ntp" || u.Role == "bgp" || u.Role == "dhcp4" || u.Role == "dhcp6" || u.Role == "ra" || u.Role == "vrrp" || u.Role == "dhcp_server" || u.Role == "dhcp6_server" || u.Role == "pppoe" || u.Role == "aftr" {
		// A restarted control has no remembered generations. Authenticate even
		// its first publication against the current process-tree registry;
		// initial publication precedes worker Ready, so Ready is not required.
		status, err := r.manager.Status(ctx)
		if err != nil {
			return nil, err
		}
		matched := false
		if status.InstanceID == r.supervisorInstance {
			for _, module := range status.Modules {
				if module.Name == u.Name && module.Role == u.Role && module.Generation == u.Generation && module.ConfigDigest == u.ConfigDigest {
					matched = true
					break
				}
			}
		}
		if !matched {
			return nil, ErrStale
		}
		producerStatus = &status
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.bindings[u.Name]
	if !r.configured || !ok || b.instance.Role != u.Role || b.digest != u.ConfigDigest {
		return nil, ErrStale
	}
	if old, ok := r.records[u.Name]; ok {
		if u.Generation < old.Generation || (u.Generation == old.Generation && u.ProducerEpoch != old.ProducerEpoch) {
			return nil, ErrStale
		}
		if u.Generation == old.Generation {
			if u.Sequence < old.Sequence {
				return nil, ErrStale
			}
			if u.Sequence == old.Sequence {
				if bytes.Equal(u.Snapshot, old.Snapshot) {
					effect := r.applications[u.Name]
					if effect == nil && (u.Role == "vrrp" || u.Role == "bgp") {
						effect = r.queueApplicationLocked(u)
					}
					return effect, nil
				}
				return nil, errors.New("protocol sequence reused with different snapshot")
			}
		}
	}
	if producerStatus != nil {
		r.installGuardStatusLocked(*producerStatus, nil)
	}

	var server dhcpserverconfig.Snapshot
	var lease dhcp6config.Snapshot
	var advertisement ra.Snapshot
	if u.Role == "dhcp_server" {
		var err error
		server, err = r.validateDHCPServerSnapshotLocked(b, u)
		if err != nil {
			return nil, err
		}
	} else if u.Role == "aftr" {
		if err := r.validateAFTRSnapshotLocked(b, u.Snapshot); err != nil {
			return nil, err
		}
	} else if u.Role == "dhcp6" {
		if err := strict(u.Snapshot, &lease); err != nil {
			return nil, err
		}
		if err := dhcp6config.ValidateSnapshot(*b.dhcp6, lease); err != nil {
			return nil, err
		}
	} else if u.Role == "ra" {
		var cfg ra.Config
		if err := strict(b.instance.Config, &cfg); err != nil {
			return nil, err
		}
		if err := strict(u.Snapshot, &advertisement); err != nil {
			return nil, err
		}
		if cfg.Resource != advertisement.Resource || cfg.Interface != advertisement.Interface {
			return nil, errors.New("RA snapshot identity mismatch")
		}
		if advertisement.Generation == 0 || advertisement.SentGeneration > advertisement.Generation {
			return nil, errors.New("RA snapshot has an invalid send generation")
		}
	} else if err := validateSnapshot(b.instance, u.Snapshot); err != nil {
		return nil, err
	}
	previous := r.records[u.Name]
	if u.Role == "vrrp" {
		if err := validateVRRPTransition(previous, u); err != nil {
			return nil, err
		}
	}
	if u.Role == "dhcp4" {
		if err := validateDHCP4Transition(previous, u); err != nil {
			return nil, err
		}
	}
	if u.Role == "bgp" {
		if err := validateBGPTransition(previous, u); err != nil {
			return nil, err
		}
	}
	candidateRecords := r.observationsLocked()
	candidateRecords[u.Name] = u
	if _, err := r.projectLocked(r.config.protocols, r.config.plan.Native, candidateRecords, time.Now(), resourceprogram.FactSnapshot{}, nil, nil); err != nil {
		return nil, err
	}
	if u.Role == "dhcp_server" {
		r.dhcpServers[u.Name] = server
	}
	if (u.Role == "dhcp6" || u.Role == "ra") && r.guardGrants == nil {
		r.guardGrants = make(map[string]guardGrant)
	}
	if u.Role == "dhcp6" {
		r.guardGrants[u.Name] = guardGrant{dhcp6: &lease}
	} else if u.Role == "ra" {
		r.guardGrants[u.Name] = guardGrant{revision: r.config.revision, generation: u.Generation, sequence: u.Sequence, epoch: u.ProducerEpoch, ra: &advertisement}
	} else {
		r.recordGuardGrantLocked(u)
	}
	r.records[u.Name] = u
	if u.Role == "vrrp" || u.Role == "bgp" {
		effect = r.queueApplicationLocked(u)
	} else if u.Role == "dhcp6" {
		if lease.State == "stopped" {
			effect = r.queueApplicationLocked(u)
		}
	} else if u.Role == "ra_client" {
		var snapshot raclient.Snapshot
		if err := json.Unmarshal(u.Snapshot, &snapshot); err != nil {
			return nil, err
		}
		if !snapshot.Active {
			effect = r.queueApplicationLocked(u)
		}
	}
	r.observationVersion++
	// Observe flaps when accepted; a periodic projection must not hide them.
	if _, err := r.conditionFactsLocked(r.config); err != nil {
		return nil, err
	}
	r.wakeCoordinatorLocked()
	return effect, nil
}

// Apply waits for protocol effects that require kernel application completion.
func (r *Runtime) Apply(ctx context.Context, u serviceconfig.Update) (serviceconfig.ApplicationReceipt, error) {
	if u.Role != "vrrp" && u.Role != "bgp" && u.Role != "dhcp6" && u.Role != "ra_client" {
		return serviceconfig.ApplicationReceipt{}, errors.New("application requests support BGP, VRRP and DHCPv6/RA withdrawal")
	}
	if u.Role == "dhcp6" {
		var snapshot dhcp6config.Snapshot
		if err := strict(u.Snapshot, &snapshot); err != nil {
			return serviceconfig.ApplicationReceipt{}, err
		}
		if snapshot.State != "stopped" {
			return serviceconfig.ApplicationReceipt{}, errors.New("DHCPv6 application request must withdraw assignments")
		}
	}
	if u.Role == "ra_client" {
		var snapshot raclient.Snapshot
		if err := strict(u.Snapshot, &snapshot); err != nil {
			return serviceconfig.ApplicationReceipt{}, err
		}
		if snapshot.Active {
			return serviceconfig.ApplicationReceipt{}, errors.New("RA application request must withdraw learned state")
		}
	}
	effect, err := r.observe(ctx, u)
	if err != nil {
		return serviceconfig.ApplicationReceipt{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if effect == nil {
		return serviceconfig.ApplicationReceipt{}, ErrStale
	}
	if u.Role == "vrrp" || u.Role == "bgp" {
		return r.waitApplicationLocked(ctx, effect)
	}
	return r.waitEffectLocked(ctx, effect)
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
	case "dns":
		var cfg dnsconfig.Config
		var snapshot dnsconfig.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		if snapshot.Resource != cfg.Runtime.Resource {
			return errors.New("DNS snapshot identity mismatch")
		}
		return dnsconfig.ValidateSnapshot(snapshot)
	case "ntp":
		var snapshot ntpconfig.Snapshot
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		if !boottime.ValidBootID(snapshot.BootID) || snapshot.ObservedNanos == 0 || snapshot.ExpiresNanos <= snapshot.ObservedNanos {
			return errors.New("NTP observation requires a finite boot interval")
		}
		return nil
	case "tailscale":
		var cfg tailscaleconfig.Config
		var snapshot tailscaleconfig.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		if snapshot.Resource != cfg.Resource || snapshot.Interface != cfg.Interface {
			return errors.New("Tailscale observation identity mismatch")
		}
		if !boottime.ValidBootID(snapshot.BootID) || snapshot.ObservedNanos == 0 || snapshot.ExpiresNanos <= snapshot.ObservedNanos {
			return errors.New("Tailscale observation requires a finite boot interval")
		}
		return nil
	case "pppoe":
		var cfg pppoeconfig.Config
		var snapshot pppoeconfig.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &snapshot); err != nil {
			return err
		}
		return pppoeconfig.ValidateSnapshot(cfg, snapshot)
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
	case "dhcp6_server":
		var cfg dhcp6serverconfig.Config
		var s dhcp6serverconfig.Snapshot
		if err := strict(instance.Config, &cfg); err != nil {
			return err
		}
		if err := strict(b, &s); err != nil {
			return err
		}
		if s.Resource != cfg.Resource || s.Interface != cfg.Interface {
			return errors.New("DHCPv6 server snapshot identity mismatch")
		}
		_, err := dhcp6config.ParseDUID(s.ServerDUID)
		return err
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
	r.ntpSourceRoutes(mux)
	r.lifecycleRoutes(mux)
	r.observationRoutes(mux)
	r.aftrRoutes(mux)
	r.dhcp6ServerRoutes(mux)
	r.tailscaleRoutes(mux)
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
	mux.HandleFunc("GET /v1/services/{name}/dns-input", func(w http.ResponseWriter, req *http.Request) {
		projection, err := r.DNSInput(req.PathValue("name"))
		if err != nil {
			localapi.Fail(w, 409, err)
			return
		}
		localapi.JSON(w, 200, projection)
	})
	mux.HandleFunc("GET /v1/services/{name}/dhcp6-input", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		_, ok := r.config.protocols.dhcp6RA[req.PathValue("name")]
		flags := r.liveDHCP6[req.PathValue("name")]
		r.mu.Unlock()
		if !ok {
			localapi.Fail(w, 404, errors.New("no RA-controlled DHCPv6 service"))
			return
		}
		localapi.JSON(w, 200, flags)
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
	err := r.CheckReady(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case err == nil:
		r.phase, r.lastError = "ready", ""
	case errors.Is(err, errApplicationPending):
		r.phase, r.lastError = "applying", ""
	default:
		r.phase, r.lastError = "degraded", err.Error()
	}
}
