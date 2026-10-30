package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
)

var errLinkNotFound = errors.New("link not found")

// JournalVersion is the persisted network ownership/BOOTTIME contract.
const JournalVersion = 27

type settingKey struct {
	Kind      string `json:"kind"`
	Interface string `json:"interface,omitempty"`
}
type ownedSetting struct {
	Key       settingKey `json:"key"`
	Before    int        `json:"before"`
	Last      int        `json:"last"`
	Index     int        `json:"index,omitempty"`
	Attempted *int       `json:"attempted,omitempty"`
}
type ownedAddress struct {
	Interface string          `json:"interface"`
	Address   string          `json:"address"`
	Index     int             `json:"index,omitempty"`
	Options   AddressOptions  `json:"options,omitzero"`
	Attempted *AddressOptions `json:"attempted,omitempty"`
}
type journal struct {
	Version           int                      `json:"version"`
	Revision          uint64                   `json:"revision"`
	AttemptedRevision uint64                   `json:"attempted_revision"`
	Desired           Desired                  `json:"desired"`
	Attempted         *Desired                 `json:"attempted,omitempty"`
	Status            string                   `json:"status"`
	LastError         string                   `json:"last_error,omitempty"`
	Addresses         []ownedAddress           `json:"addresses,omitempty"`
	Routes            []ownedRoute             `json:"routes,omitempty"`
	PolicyRules       []PolicyRule             `json:"policy_rules,omitempty"`
	Links             []ownedLink              `json:"links,omitempty"`
	Attributes        []ownedAttribute         `json:"attributes,omitempty"`
	Settings          []ownedSetting           `json:"settings,omitempty"`
	FirewallOwned     bool                     `json:"firewall_owned"`
	VRRPGuardOwned    bool                     `json:"vrrp_guard_owned"`
	WireGuard         []ownedWireGuard         `json:"wireguard,omitempty"`
	WireGuardSecrets  []wireGuardSecretBinding `json:"wireguard_secrets,omitempty"`
}

// kernel is private: protocol modules use Desired, never netlink objects or
// shell fragments. A fake implementation exercises ownership without root.
type kernel interface {
	AnnounceLink(linkAnnouncement) error
	Interface(string) (InterfaceState, error)
	Interfaces() ([]InterfaceState, error)
	CreateLink(Link, string) error
	UpdateLink(Link, string) error
	DeleteLink(string) error
	WriteAttribute(settingKey, string) error
	AddAddress(string, string, AddressOptions) error
	UpdateAddress(string, string, AddressOptions) error
	DeleteAddress(string, string, AddressOptions) error
	Routes(int) ([]RouteState, error)
	AddRoute(ownedRoute) error
	UpdateRoute(ownedRoute) error
	DeleteRoute(Route) error
	PolicyRules() ([]PolicyRuleState, error)
	AddPolicyRule(PolicyRule) error
	DeletePolicyRule(PolicyRule) error
	ReadSetting(settingKey) (int, error)
	WriteSetting(settingKey, int) error
	Firewall() (FirewallState, error)
	ReplaceFirewall(*Firewall) error
	VRRPGuard() (FirewallState, error)
	ReplaceVRRPGuard([]VRRPGuard) error
}

type Engine struct {
	mu              sync.Mutex
	opts            Options
	kernel          kernel
	state           journal
	lock            *os.File
	directory       *os.File
	journalName     string
	journalPresent  bool
	authorityWake   chan struct{}
	addressSets     map[string]resolvedAddressSet
	addressSetsWake chan struct{}
}

// New locks the ownership journal and reads it. It makes no kernel changes.
// Call Close when the worker stops. Kernel operations use the worker's network
// namespace; the process must not switch namespaces while the engine is active.
func New(opts Options) (*Engine, error) {
	k, err := newKernel()
	if err != nil {
		return nil, err
	}
	return newEngine(opts, k)
}

func newEngine(opts Options, k kernel) (*Engine, error) {
	dir, name, err := openNetworkJournalParent(opts.StatePath, true)
	if err != nil {
		return nil, err
	}
	f, err := openNetworkJournalFile(dir, name+".lock", true)
	if err != nil {
		dir.Close()
		return nil, err
	}
	fail := func(err error) (*Engine, error) { f.Close(); dir.Close(); return nil, err }
	if err := lockJournal(f); err != nil {
		return fail(fmt.Errorf("network journal already in use or cannot be locked: %w", err))
	}
	state, inspection, err := readNetworkJournal(context.Background(), dir, name)
	if err != nil {
		return fail(err)
	}
	if !inspection.Present {
		state = journal{Version: JournalVersion, Status: "unapplied"}
	}
	if state.Status == "applying" {
		state.Status = "degraded"
		state.LastError = "previous apply was interrupted; reconcile the committed configuration"
	}
	e := &Engine{opts: opts, kernel: k, lock: f, directory: dir, journalName: name, journalPresent: inspection.Present, state: state, authorityWake: make(chan struct{}, 1), addressSetsWake: make(chan struct{}, 1)}
	e.prepareAddressSets(state.Desired.Firewall)

	return e, nil
}

func validateJournal(s journal) error {
	if err := validateWireGuardJournal(s); err != nil {
		return err
	}
	if err := validatePolicyRules(s.PolicyRules); err != nil {
		return err
	}
	if err := validateLinkJournal(s); err != nil {
		return err
	}
	if err := Validate(s.Desired); err != nil {
		return err
	}
	if s.Attempted != nil {
		if err := Validate(*s.Attempted); err != nil {
			return err
		}
	}
	for _, a := range s.Addresses {
		if a.Index < 0 {
			return fmt.Errorf("invalid saved address interface index")
		}
		if err := Validate(Desired{Interfaces: []Interface{{Name: a.Interface, Addresses: []string{a.Address}, AddressOptions: map[string]AddressOptions{a.Address: a.Options}}}}); err != nil {
			return err
		}
		if a.Attempted != nil {
			if err := Validate(Desired{Interfaces: []Interface{{Name: a.Interface, Addresses: []string{a.Address}, AddressOptions: map[string]AddressOptions{a.Address: *a.Attempted}}}}); err != nil {
				return err
			}
		}
	}
	if err := validateRouteJournal(s.Routes); err != nil {
		return err
	}
	for _, v := range s.Settings {
		if err := validateSettingRecord(v); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lock == nil {
		return nil
	}
	err := errors.Join(e.lock.Close(), e.directory.Close())
	e.lock = nil
	e.directory = nil
	return err
}

func cloneDesired(d Desired) Desired {
	b, _ := json.Marshal(d)
	var out Desired
	_ = json.Unmarshal(b, &out)
	return out
}

// ApplyResult acknowledges the durable projection. Kernel observations belong
// to Snapshot and are not collected as an unused side effect of applying it.
type ApplyResult struct {
	Revision          uint64 `json:"revision"`
	AttemptedRevision uint64 `json:"attempted_revision"`
	Status            string `json:"status"`
}

// Apply accepts older and equal revisions so the control plane can reconcile a
// committed revision after a failed candidate or worker restart. The caller is
// responsible for revision concurrency. A failure never implies rollback.
func (e *Engine) Apply(ctx context.Context, revision uint64, desired Desired) (ApplyResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	defer func() {
		select {
		case e.authorityWake <- struct{}{}:
		default:
		}
	}()
	if e.lock == nil {
		return ApplyResult{}, errors.New("network engine is closed")
	}
	if err := Validate(desired); err != nil {
		return ApplyResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ApplyResult{}, err
	}
	clock, err := boottime.Read()
	if err != nil {
		return ApplyResult{}, err
	}
	d := ProjectLifetimes(normalized(cloneDesired(desired)), clock)
	for _, link := range d.Links {
		if !link.Authority.Live(clock) {
			return ApplyResult{}, &ApplyError{Stage: "link authority", Err: fmt.Errorf("link %s original authority expired", link.Name)}
		}
	}
	boundLinkAddresses(&d, clock)
	e.state.AttemptedRevision, e.state.Attempted = revision, &d
	e.state.Status, e.state.LastError = "applying", ""
	if err := e.save(); err != nil {
		return ApplyResult{}, &ApplyError{Stage: "journal", Err: err}
	}
	p := &apply{e: e, ctx: ctx}
	err = p.reconcile(d)
	if err == nil {
		previousRevision, previousDesired := e.state.Revision, e.state.Desired
		e.state.Revision, e.state.Desired = revision, d
		e.state.Status, e.state.Attempted = "applied", nil
		if saveErr := e.save(); saveErr != nil {
			e.state.Revision, e.state.Desired = previousRevision, previousDesired
			e.state.Attempted = &d
			err = &ApplyError{Stage: "journal finalization", Partial: p.changed > 0, Err: saveErr}
		}
	}
	if err != nil {
		e.state.Status, e.state.LastError = "degraded", err.Error()
		if saveErr := e.save(); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("persist degraded state: %w", saveErr))
		}
	}
	if err == nil {
		e.prepareAddressSets(d.Firewall)
	}
	return ApplyResult{Revision: e.state.Revision, AttemptedRevision: e.state.AttemptedRevision, Status: e.state.Status}, err
}

func (e *Engine) Snapshot(ctx context.Context) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lock == nil {
		return Snapshot{}, errors.New("network engine is closed")
	}
	s := e.snapshotLocked(ctx)
	if len(s.ObservationErrors) != 0 {
		return s, fmt.Errorf("network observation incomplete: %v", s.ObservationErrors)
	}
	return s, nil
}

func (e *Engine) snapshotLocked(ctx context.Context) Snapshot {
	s := Snapshot{Revision: e.state.Revision, AttemptedRevision: e.state.AttemptedRevision, Status: e.state.Status, Desired: cloneDesired(e.state.Desired), LastError: e.state.LastError, Firewall: FirewallState{Table: FirewallTable}}
	problem := func(err error) {
		if err != nil {
			s.ObservationErrors = append(s.ObservationErrors, err.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		problem(err)
		return s
	}
	names, tables := map[string]bool{}, map[int]bool{}
	collect := func(d Desired) {
		for _, wireguard := range d.WireGuard {
			names[wireguard.Interface] = true
		}
		for _, r := range d.PolicyRules {
			if !r.Blackhole {
				tables[r.Table] = true
			}
			if r.InInterface != "" {
				names[r.InInterface] = true
			}
			if r.OutInterface != "" {
				names[r.OutInterface] = true
			}
		}
		for _, l := range d.Links {
			names[l.Name] = true
			if l.Parent != "" {
				names[l.Parent] = true
			}
		}
		for _, l := range d.Interfaces {
			names[l.Name] = true
			if l.Master != nil && *l.Master != "" {
				names[*l.Master] = true
			}
		}
		for _, r := range d.Routes {
			if r.Interface != "" {
				names[r.Interface] = true
			}
			for _, h := range r.NextHops {
				names[h.Interface] = true
			}
			tables[routeTable(r)] = true
		}
	}
	collect(e.state.Desired)
	if e.state.Attempted != nil {
		collect(*e.state.Attempted)
	}
	for _, a := range e.state.Addresses {
		names[a.Interface] = true
	}
	for _, l := range e.state.Links {
		names[l.Definition.Name] = true
	}
	for _, a := range e.state.Attributes {
		names[a.Key.Interface] = true
	}
	for _, r := range e.state.Routes {
		tables[routeTable(r.Route)] = true
	}
	for _, r := range e.state.PolicyRules {
		if !r.Blackhole {
			tables[r.Table] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	for _, n := range ordered {
		l, err := e.kernel.Interface(n)
		if err != nil {
			problem(fmt.Errorf("interface %s: %w", n, err))
			continue
		}
		for i := range l.Addresses {
			a := &l.Addresses[i]
			o := e.addressRecord(addressKey{n, a.Address})
			a.Owned = o != nil && (o.Index == 0 || o.Index == l.Index) && (addressOptionsMatch(*a, o.Options, time.Now()) || o.Attempted != nil && addressOptionsMatch(*a, *o.Attempted, time.Now()))
		}
		l.Owned = e.ownsLink(l)
		s.Interfaces = append(s.Interfaces, l)
		if record := e.wireGuardRecord(n); record != nil && l.Kind == "wireguard" {
			if backend, ok := e.kernel.(wireGuardKernel); ok {
				device, err := backend.ReadWireGuard(l.Index)
				problem(err)
				if err == nil {
					s.WireGuard = append(s.WireGuard, publicWireGuardState(n, device, l.Owned && wireGuardOwned(device, *record), record.Quarantined))
				}
				clearWireGuardDevice(&device)
			} else {
				problem(errors.New("WireGuard backend is unavailable for observation"))
			}
		}
	}
	tableIDs := make([]int, 0, len(tables))
	for t := range tables {
		tableIDs = append(tableIDs, t)
	}
	sort.Ints(tableIDs)
	for _, t := range tableIDs {
		rs, err := e.kernel.Routes(t)
		problem(err)
		for _, r := range rs {
			r.Owned = e.ownsRoute(r)
			s.Routes = append(s.Routes, r)
		}
	}
	v, err := e.kernel.ReadSetting(settingKey{Kind: "forwarding4"})
	if len(e.state.PolicyRules) > 0 || len(e.state.Desired.PolicyRules) > 0 || e.state.Attempted != nil && len(e.state.Attempted.PolicyRules) > 0 {
		rules, readErr := e.kernel.PolicyRules()
		problem(readErr)
		for _, r := range rules {
			r.Owned = e.ownsPolicyRule(r)
			s.PolicyRules = append(s.PolicyRules, r)
		}
	}
	problem(err)
	s.Forwarding.IPv4 = v == 1
	v, err = e.kernel.ReadSetting(settingKey{Kind: "forwarding6"})
	problem(err)
	s.Forwarding.IPv6 = v == 1
	s.Forwarding.IPv4RPFilterAll, err = e.kernel.ReadSetting(settingKey{Kind: "rp_filter_all"})
	problem(err)
	s.Forwarding.IPv4RPFilterDefault, err = e.kernel.ReadSetting(settingKey{Kind: "rp_filter_default"})
	problem(err)
	fw, err := e.kernel.Firewall()
	problem(err)
	if err == nil {
		fw.Owned = fw.Owned && e.state.FirewallOwned
		s.Firewall = fw
	}
	guard, guardErr := e.kernel.VRRPGuard()
	problem(guardErr)
	if guardErr == nil {
		guard.Owned = guard.Owned && e.state.VRRPGuardOwned
		s.VRRPGuard = guard
	}
	return s
}

func (e *Engine) save() error {
	raw, err := encodeNetworkJournal(e.state)
	if err != nil {
		return err
	}
	published, err := writeNetworkJournal(e.directory, e.journalName, raw)
	e.journalPresent = e.journalPresent || published
	return err
}

func (e *Engine) ownsAddress(a addressKey) bool {
	for _, v := range e.state.Addresses {
		if v.key() == a {
			return true
		}
	}
	return false
}
func routeMatches(live RouteState, saved ownedRoute) bool {
	if !sameRouteTimer(live.kernelExpiry, saved.KernelExpiry) && (saved.AttemptedExpiry == nil || !sameRouteTimer(live.kernelExpiry, *saved.AttemptedExpiry)) {
		return false
	}
	// Linux RTN_UNICAST and RTN_BLACKHOLE; other route types remain unowned.
	typeID := 1
	if saved.Blackhole {
		typeID = 6
	}
	return sameKernelRoute(live.Route, saved.Route) && live.Protocol == RouteProtocol && live.Type == typeID && live.Multipath == (len(live.NextHops) > 0) && !live.Advanced
}
func (e *Engine) ownsRoute(r RouteState) bool {
	for _, v := range e.state.Routes {
		if routeMatches(r, v) {
			return true
		}
	}
	return false
}

type apply struct {
	e               *Engine
	ctx             context.Context
	changed         int
	wireGuardWanted map[string]wireGuardRecorded
	routeStates     map[string][]RouteState
}

// A native multi-attribute update may fail after changing some attributes.
// Propagate that uncertainty instead of reporting an untouched kernel.
type partialMutationError struct{ error }

func (p *apply) failure(stage string, err error) error {
	return &ApplyError{Stage: stage, Partial: p.changed > 0, Err: err}
}
func (p *apply) mutate(stage string, fn func() error) error {
	if err := p.ctx.Err(); err != nil {
		return p.failure(stage, err)
	}
	if err := fn(); err != nil {
		var partial *partialMutationError
		if errors.As(err, &partial) {
			p.changed++
		}
		return p.failure(stage, err)
	}
	p.changed++
	return nil
}

func (p *apply) change(stage string, fn func() error, record func()) error {
	if err := p.mutate(stage, fn); err != nil {
		return err
	}
	if record != nil {
		record()
	}
	if err := p.e.save(); err != nil {
		return p.failure(stage+" journal", err)
	}
	return nil
}
func (p *apply) checkpoint(stage string) error {
	if err := p.e.save(); err != nil {
		return p.failure(stage, err)
	}
	return nil
}

func (p *apply) reconcile(d Desired) error {
	if err := p.preflightFirewall(d.Firewall); err != nil {
		return err
	}
	if err := p.preflightVRRPGuards(d.VRRPGuards); err != nil {
		return err
	}
	if err := p.preflightPolicyRules(d.PolicyRules); err != nil {
		return err
	}
	if err := p.preflightLinks(d); err != nil {
		return err
	}
	if err := p.preflightWireGuard(d); err != nil {
		return err
	}
	wantedSettings := map[settingKey]int{}
	wantedAddresses := map[addressKey]bool{}
	ipv4Interfaces := map[string]bool{}
	for _, l := range d.Interfaces {
		for _, address := range l.Addresses {
			wantedAddresses[addressKey{l.Name, address}] = true
			if netip.MustParsePrefix(address).Addr().Is4() {
				ipv4Interfaces[l.Name] = true
			}
		}
		if l.AdminState != AdminUnmanaged {
			wantedSettings[settingKey{Kind: "up", Interface: l.Name}] = 1
		}
		if l.MTU != 0 {
			wantedSettings[settingKey{Kind: "mtu", Interface: l.Name}] = l.MTU
		}
		filter := l.IPv4RPFilter
		if filter == nil {
			filter = d.Forwarding.IPv4RPFilter
		}
		if filter != nil {
			wantedSettings[settingKey{Kind: "rp_filter", Interface: l.Name}] = *filter
		}
		if l.IPv6AcceptRA != nil {
			wantedSettings[settingKey{Kind: "accept_ra", Interface: l.Name}] = *l.IPv6AcceptRA
		}
		if l.IPv6Autoconf != nil {
			wantedSettings[settingKey{Kind: "autoconf", Interface: l.Name}] = boolInt(*l.IPv6Autoconf)
		}
		if l.IPv6MTU != nil {
			wantedSettings[settingKey{Kind: "ipv6_mtu", Interface: l.Name}] = *l.IPv6MTU
		}
		if l.IPv6HopLimit != nil {
			wantedSettings[settingKey{Kind: "hop_limit", Interface: l.Name}] = *l.IPv6HopLimit
		}
	}
	// Linux otherwise answers or sends ARP on a parent using an IPv4 address
	// owned by its macvlan, exposing the wrong MAC for that address.
	for _, link := range d.Links {
		if link.Kind == "macvlan" && ipv4Interfaces[link.Name] {
			for _, name := range []string{link.Parent, link.Name} {
				wantedSettings[settingKey{Kind: "arp_ignore", Interface: name}] = 1
				wantedSettings[settingKey{Kind: "arp_announce", Interface: name}] = 2
			}
		}
	}
	if d.ConnectionTimestamps {
		wantedSettings[settingKey{Kind: "conntrack_timestamp"}] = 1
	}
	if d.Forwarding.ConntrackAccounting != nil {
		wantedSettings[settingKey{Kind: "conntrack_accounting"}] = boolInt(*d.Forwarding.ConntrackAccounting)
	}
	// Creating a ct-packets expression enables accounting in the kernel.
	// Own that side effect even when no explicit sysctl resource is present.
	if d.Firewall != nil && d.Firewall.FlowObservationGroup != 0 {
		wantedSettings[settingKey{Kind: "conntrack_accounting"}] = 1
	}
	if d.Forwarding.ConntrackTCPLiberal != nil {
		wantedSettings[settingKey{Kind: "conntrack_tcp_liberal"}] = boolInt(*d.Forwarding.ConntrackTCPLiberal)
	}
	if d.Forwarding.IPv4 != nil {
		wantedSettings[settingKey{Kind: "forwarding4"}] = boolInt(*d.Forwarding.IPv4)
	}
	if d.Forwarding.IPv6 != nil {
		wantedSettings[settingKey{Kind: "forwarding6"}] = boolInt(*d.Forwarding.IPv6)
	}
	if d.Forwarding.IPv4RPFilter != nil {
		wantedSettings[settingKey{Kind: "rp_filter_all"}] = *d.Forwarding.IPv4RPFilter
		wantedSettings[settingKey{Kind: "rp_filter_default"}] = *d.Forwarding.IPv4RPFilter
	}
	if d.Forwarding.IPv4ECMPHash == "source" {
		wantedSettings[settingKey{Kind: "multipath_hash_fields4"}] = 1
		wantedSettings[settingKey{Kind: "multipath_hash_policy4"}] = 3
	}
	// Keep RA/SLAAC writers disabled and shared VIP ARP restricted until the
	// corresponding addresses have been removed. Connection accounting and start
	// times must likewise outlive their observation rules. Restore them afterwards.
	earlySettings := make(map[settingKey]int, len(wantedSettings))
	for key, value := range wantedSettings {
		earlySettings[key] = value
	}
	for _, old := range p.e.state.Settings {
		if raSetting(old.Key) || old.Key.Kind == "arp_ignore" || old.Key.Kind == "arp_announce" || old.Key.Kind == "conntrack_accounting" || old.Key.Kind == "conntrack_timestamp" {
			earlySettings[old.Key] = old.Last
		}
	}
	if err := p.restoreSettings(earlySettings); err != nil {
		return err
	}
	// Existing interfaces may already be UP. Stop their competing writers
	// before changing addresses, links or routes. Newly created links receive
	// these settings in interfaceConfig, before their first UP transition.
	for _, l := range d.Interfaces {
		if l.IPv6AcceptRA == nil && l.IPv6Autoconf == nil {
			continue
		}
		if _, err := p.e.kernel.Interface(l.Name); errors.Is(err, errLinkNotFound) {
			continue
		} else if err != nil {
			return p.failure("RA setting interface", err)
		}
		if err := p.interfaceRASettings(l); err != nil {
			return err
		}
	}
	if err := p.restoreAttributes(d); err != nil {
		return err
	}
	// Explicit disables run before any network policy changes; enables run only
	// after the new firewall transaction has been acknowledged.
	for _, kind := range []string{"forwarding4", "forwarding6"} {
		key := settingKey{Kind: kind}
		if v, ok := wantedSettings[key]; ok && v == 0 {
			if err := p.set(key, v); err != nil {
				return err
			}
		}
	}
	// Linux clears RTA_PREFSRC when its address is removed but retains the
	// route. Withdraw obsolete owned routes first, while their full identity
	// still matches the journal.
	if err := p.removeRoutes(d.Routes); err != nil {
		return err
	}
	if err := p.removeAddresses(wantedAddresses); err != nil {
		return err
	}
	if err := p.links(d.Links); err != nil {
		return err
	}
	if err := p.wireGuard(d); err != nil {
		return err
	}
	for _, l := range d.Interfaces {
		if err := p.interfaceAttributes(l); err != nil {
			return err
		}
	}
	if err := p.interfaceMTUs(d); err != nil {
		return err
	}
	// Set parent and macvlan restrictions before any IPv4 address is added.
	var arpSettings []settingKey
	for key := range wantedSettings {
		if key.Kind == "arp_ignore" || key.Kind == "arp_announce" {
			arpSettings = append(arpSettings, key)
		}
	}
	sort.Slice(arpSettings, func(i, j int) bool {
		if arpSettings[i].Interface != arpSettings[j].Interface {
			return arpSettings[i].Interface < arpSettings[j].Interface
		}
		return arpSettings[i].Kind < arpSettings[j].Kind
	})
	for _, key := range arpSettings {
		if err := p.set(key, wantedSettings[key]); err != nil {
			return err
		}
	}
	for _, l := range d.Interfaces {
		if err := p.interfaceConfig(l); err != nil {
			return err
		}
	}
	if err := p.addressConfigs(d.Interfaces); err != nil {
		return err
	}
	if err := p.removePolicyRules(d.PolicyRules); err != nil {
		return err
	}
	// Address removal and link changes can change connected routes and clear
	// source attributes. Route installation uses a new kernel observation.
	p.routeStates = nil
	if err := p.routes(d.Routes); err != nil {
		return err
	}
	if err := p.policyRules(d.PolicyRules); err != nil {
		return err
	}
	if err := p.removeLinks(d); err != nil {
		return err
	}
	// Capture the original value before nftables implicitly enables accounting.
	// Start times distinguish new connections that reuse the same endpoints.
	for _, kind := range []string{"conntrack_accounting", "conntrack_timestamp"} {
		key := settingKey{Kind: kind}
		if value, wanted := wantedSettings[key]; wanted {
			if err := p.set(key, value); err != nil {
				return err
			}
		}
	}
	if err := p.firewall(d.Firewall); err != nil {
		return err
	}
	if err := p.vrrpGuards(d.VRRPGuards); err != nil {
		return err
	}
	for _, kind := range []string{"forwarding4", "forwarding6"} {
		key := settingKey{Kind: kind}
		if v, ok := wantedSettings[key]; ok && v == 1 {
			if err := p.set(key, v); err != nil {
				return err
			}
		}
	}
	// Apply after ip_forward, before protocol workers can create their units.
	for _, kind := range []string{"rp_filter_all", "rp_filter_default", "conntrack_tcp_liberal", "multipath_hash_fields4", "multipath_hash_policy4"} {
		key := settingKey{Kind: kind}
		if v, ok := wantedSettings[key]; ok {
			if err := p.set(key, v); err != nil {
				return err
			}
		}
	}
	for _, iface := range d.Interfaces {
		key := settingKey{Kind: "rp_filter", Interface: iface.Name}
		if value, owned := wantedSettings[key]; owned {
			if err := p.set(key, value); err != nil {
				return err
			}
		}
	}
	return p.restoreSettings(wantedSettings)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (p *apply) interfaceAttributes(l Interface) error {
	if err := p.interfaceRASettings(l); err != nil {
		return err
	}
	for _, field := range []struct {
		kind  string
		value *string
	}{{"master", l.Master}, {"alias", l.Alias}} {
		if field.value != nil {
			if err := p.attribute(settingKey{Kind: field.kind, Interface: l.Name}, *field.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *apply) interfaceConfig(l Interface) error {
	if l.IPv6MTU != nil {
		live, err := p.e.kernel.Interface(l.Name)
		if err != nil {
			return p.failure("IPv6 MTU interface", err)
		}
		if err := p.set(settingKey{Kind: "ipv6_mtu", Interface: l.Name}, min(*l.IPv6MTU, live.MTU)); err != nil {
			return err
		}
	}
	if l.IPv6HopLimit != nil {
		if err := p.set(settingKey{Kind: "hop_limit", Interface: l.Name}, *l.IPv6HopLimit); err != nil {
			return err
		}
	}
	if l.AdminState != AdminUnmanaged {
		if err := p.set(settingKey{Kind: "up", Interface: l.Name}, 1); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) removeAddresses(want map[addressKey]bool) error {
	// Remove secondaries first. Linux may otherwise implicitly remove them when
	// deleting their primary IPv4 address; the backend refuses that operation.
	secondary := map[addressKey]bool{}
	for _, a := range p.e.state.Addresses {
		if want[a.key()] {
			continue
		}
		l, err := p.e.kernel.Interface(a.Interface)
		if err != nil && !errors.Is(err, errLinkNotFound) {
			return p.failure("address removal order", err)
		}
		for _, live := range l.Addresses {
			if live.Address == a.Address {
				secondary[a.key()] = live.Flags&1 != 0
			}
		}
	}
	sort.SliceStable(p.e.state.Addresses, func(i, j int) bool {
		return secondary[p.e.state.Addresses[i].key()] && !secondary[p.e.state.Addresses[j].key()]
	})
	for i := 0; i < len(p.e.state.Addresses); {
		a := p.e.state.Addresses[i]
		if want[a.key()] {
			i++
			continue
		}
		l, err := p.e.kernel.Interface(a.Interface)
		if err != nil && !errors.Is(err, errLinkNotFound) {
			return p.failure("address remove read", err)
		}
		if err == nil && a.Index > 0 && a.Index != l.Index {
			return p.failure("address removal ownership", fmt.Errorf("interface %s was replaced", a.Interface))
		}
		found := false
		for _, v := range l.Addresses {
			if v.Address == a.Address {
				if !addressOptionsMatch(v, a.Options, time.Now()) && (a.Attempted == nil || !addressOptionsMatch(v, *a.Attempted, time.Now())) {
					return p.failure("address removal ownership", fmt.Errorf("options for %s on %s changed outside routerd2; refusing to delete", a.Address, a.Interface))
				}
				found = true
			}
		}
		forget := func() { p.e.state.Addresses = append(p.e.state.Addresses[:i], p.e.state.Addresses[i+1:]...) }
		if found {
			if err := p.change("address remove", func() error { return p.e.kernel.DeleteAddress(a.Interface, a.Address, a.Options) }, forget); err != nil {
				return err
			}
		} else {
			forget()
			if err := p.checkpoint("forget absent address"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *apply) removeRoutes(want []Route) error {
	wanted := make(map[string]Route, len(want))
	for _, r := range want {
		wanted[routeSlot(r)] = r
	}
	for i := 0; i < len(p.e.state.Routes); {
		old := p.e.state.Routes[i]
		if next, ok := wanted[routeSlot(old.Route)]; ok && sameKernelRoute(old.Route, next) {
			i++
			continue
		}
		if err := p.removeRoute(i); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) routes(want []Route) error {
	linkBounds := make(map[string]lifetime.Deadline, len(p.e.state.Links))
	for _, link := range p.e.state.Links {
		linkBounds[link.Definition.Name] = link.Definition.Authority
	}
	wanted := make(map[string]Route, len(want))
	for _, r := range want {
		wanted[routeSlot(r)] = r
	}
	refresh := make(map[string]Route)
	for _, saved := range p.e.state.Routes {
		key := routeSlot(saved.Route)
		next := wanted[key] // removeRoutes already removed obsolete kernel identities.
		if saved.ValidUntil != next.ValidUntil || saved.AttemptedExpiry != nil {
			refresh[key] = next
		}
	}
	if err := p.refreshRoutes(refresh); err != nil {
		return err
	}
	owned := make(map[string]ownedRoute, len(p.e.state.Routes))
	for i := range p.e.state.Routes {
		key := routeSlot(p.e.state.Routes[i].Route)
		// BOOTTIME writer authority is committed together before the final ACK.
		p.e.state.Routes[i].Route = wanted[key]
		owned[key] = p.e.state.Routes[i]
	}
	var additions []Route
	for _, r := range want {
		live, err := p.readRoutes()
		if err != nil {
			return p.failure("route read", err)
		}
		key := routeSlot(r)
		found := false
		for _, v := range live[key] {
			if saved, ok := owned[key]; ok && routeMatches(v, saved) {
				found = true
				continue
			}
			return p.failure("route ownership", fmt.Errorf("route %s table %d metric %d conflicts with an unowned or changed route", r.Destination, r.Table, r.Metric))
		}
		if !found {
			additions = append(additions, r)
		}
	}
	return p.addRoutes(additions, linkBounds)
}

// A renewed RA/RIO lifetime changes the kernel timer in place. Prepare the
// complete batch before saving one intent containing both old and new timers.
// The operation owner saves the acknowledged results, including partial failure.
func (p *apply) refreshRoutes(wanted map[string]Route) error {
	if len(wanted) == 0 {
		return nil
	}
	clock, err := readRouteClock()
	if err != nil {
		return p.failure("route refresh clock", err)
	}
	type refresh struct {
		index     int
		oldTimer  routeTimer
		installed ownedRoute
		found     bool
	}
	updates := make([]refresh, 0, len(wanted))
	for i, saved := range p.e.state.Routes {
		key := routeSlot(saved.Route)
		next, ok := wanted[key]
		if !ok {
			continue
		}
		live, err := p.readRoutes()
		if err != nil {
			return p.failure("route refresh read", err)
		}
		update := refresh{index: i, oldTimer: saved.KernelExpiry}
		for _, observed := range live[key] {
			if !routeMatches(observed, saved) {
				return p.failure("route refresh ownership", errors.New("route changed outside routerd2"))
			}
			update.found = true
			// A previous ACK may have been lost. Preserve the current readback
			// before replacing its pending intent with another timer.
			update.oldTimer = observed.kernelExpiry
		}
		timer, err := newRouteTimer(next.ValidUntil, clock)
		if err != nil {
			return p.failure("route refresh lifetime", fmt.Errorf("route %s table %d metric %d: %w", next.Destination, routeTable(next), next.Metric, err))
		}
		update.installed = ownedRoute{Route: next, KernelExpiry: timer}
		updates = append(updates, update)
	}
	for i := range updates {
		update := &updates[i]
		saved := &p.e.state.Routes[update.index]
		saved.Route, saved.KernelExpiry = update.installed.Route, update.oldTimer
		saved.AttemptedExpiry = &update.installed.KernelExpiry
	}
	if err := p.checkpoint("route refresh intent"); err != nil {
		return err
	}
	for _, update := range updates {
		next := update.installed.Route
		if err := p.ctx.Err(); err != nil {
			return p.failure("route refresh", err)
		}
		// The intent write and earlier updates may outlast the original grant.
		clock, err := boottime.Read()
		if err != nil {
			return p.failure("route refresh", err)
		}
		if !next.ValidUntil.Live(clock) || next.BootID != "" && (next.BootID != clock.BootID || next.ExpiresNanos <= uint64(clock.BoottimeNanoseconds)) {
			return p.failure("route refresh", errors.New("route authority expired before refresh"))
		}
		for _, link := range p.e.state.Links {
			if routeUsesInterface(next, link.Definition.Name) && !link.Definition.Authority.Live(clock) {
				return p.failure("route refresh", errors.New("route link authority expired before refresh"))
			}
		}
		if update.found {
			err = p.e.kernel.UpdateRoute(update.installed)
		} else {
			err = p.e.kernel.AddRoute(update.installed)
		}
		if err != nil {
			return p.failure("route refresh", err)
		}
		p.changed++
		p.e.state.Routes[update.index] = update.installed
		// Preserve this operation's readback after each acknowledged refresh.
		p.routeStates[routeSlot(next)] = []RouteState{{Route: next, kernelExpiry: update.installed.KernelExpiry, Protocol: RouteProtocol, Type: 1, Multipath: len(next.NextHops) > 0}}
	}
	return nil
}

func (p *apply) addRoutes(additions []Route, linkBounds map[string]lifetime.Deadline) error {
	if len(additions) == 0 {
		return nil
	}
	clock, err := readRouteClock()
	if err != nil {
		return p.failure("route clock", err)
	}
	prepared := make([]ownedRoute, 0, len(additions))
	positions := make(map[string]int, len(p.e.state.Routes))
	for i, saved := range p.e.state.Routes {
		positions[routeSlot(saved.Route)] = i
	}
	for _, r := range additions {
		timer, err := newRouteTimer(r.ValidUntil, clock)
		if err != nil {
			return p.failure("route lifetime", fmt.Errorf("route %s table %d metric %d: %w", r.Destination, routeTable(r), r.Metric, err))
		}
		saved := ownedRoute{Route: r, KernelExpiry: timer}
		if i, ok := positions[routeSlot(r)]; ok {
			p.e.state.Routes[i] = saved
		} else {
			positions[routeSlot(r)] = len(p.e.state.Routes)
			p.e.state.Routes = append(p.e.state.Routes, saved)
		}
		prepared = append(prepared, saved)
	}
	if err := p.checkpoint("route add intent"); err != nil {
		return err
	}
	for _, saved := range prepared {
		r := saved.Route
		if err := p.ctx.Err(); err != nil {
			return p.failure("route add", err)
		}
		clock, err := boottime.Read()
		if err != nil {
			return p.failure("route authority", err)
		}
		live := r.ValidUntil.Live(clock) && (r.BootID == "" || r.BootID == clock.BootID && r.ExpiresNanos > uint64(clock.BoottimeNanoseconds)) && linkBounds[r.Interface].Live(clock)
		for _, hop := range r.NextHops {
			live = live && linkBounds[hop.Interface].Live(clock)
		}
		if !live {
			continue
		}
		if err := p.e.kernel.AddRoute(saved); err != nil {
			return p.failure("route add", err)
		}
		p.changed++
	}
	return nil
}

func (p *apply) preflightFirewall(want *Firewall) error {
	if want == nil && !p.e.state.FirewallOwned {
		return nil
	}
	live, err := p.e.kernel.Firewall()
	if err != nil {
		return p.failure("firewall read", err)
	}
	if live.Present && (!live.Owned || !p.e.state.FirewallOwned) {
		return p.failure("firewall ownership", fmt.Errorf("inet table %s exists without matching ownership journal and marker", FirewallTable))
	}
	return nil
}

func (p *apply) firewall(want *Firewall) error {
	if want == nil && !p.e.state.FirewallOwned {
		return nil
	}
	live, err := p.e.kernel.Firewall()
	if err != nil {
		return p.failure("firewall read", err)
	}
	if live.Present && (!live.Owned || !p.e.state.FirewallOwned) {
		return p.failure("firewall ownership", fmt.Errorf("inet table %s exists without matching ownership journal and marker", FirewallTable))
	}
	if want == nil && !live.Present {
		p.e.state.FirewallOwned = false
		return p.checkpoint("forget absent firewall")
	}
	if err := p.mutate("firewall atomic replace", func() error { return p.e.kernel.ReplaceFirewall(p.e.resolvedFirewall(want)) }); err != nil {
		return err
	}
	if owned := want != nil; p.e.state.FirewallOwned != owned {
		p.e.state.FirewallOwned = owned
		return p.checkpoint("firewall ownership")
	}
	return nil
}
