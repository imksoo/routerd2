package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

var errLinkNotFound = errors.New("link not found")

const tunnelAlias = "routerd2:ds-lite:v1"

// JournalVersion is the persisted network ownership/BOOTTIME contract.
const JournalVersion = 2

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
	Routes            []Route                  `json:"routes,omitempty"`
	PolicyRules       []PolicyRule             `json:"policy_rules,omitempty"`
	Tunnels           []DSLite                 `json:"tunnels,omitempty"`
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
	AddRoute(Route) error
	DeleteRoute(Route) error
	PolicyRules() ([]PolicyRuleState, error)
	AddPolicyRule(PolicyRule) error
	DeletePolicyRule(PolicyRule) error
	CreateDSLite(DSLite) error
	UpdateDSLite(DSLite) error
	DeleteDSLite(string) error
	ReadSetting(settingKey) (int, error)
	WriteSetting(settingKey, int) error
	Firewall() (FirewallState, error)
	ReplaceFirewall(*Firewall) error
	VRRPGuard() (FirewallState, error)
	ReplaceVRRPGuard([]VRRPGuard) error
}

type Engine struct {
	mu             sync.Mutex
	opts           Options
	kernel         kernel
	state          journal
	lock           *os.File
	directory      *os.File
	journalName    string
	journalPresent bool
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
	e := &Engine{opts: opts, kernel: k, lock: f, directory: dir, journalName: name, journalPresent: inspection.Present, state: state}

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
	if err := Validate(Desired{Routes: s.Routes, DSLite: s.Tunnels}); err != nil {
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

// Apply accepts older and equal revisions so the control plane can reconcile a
// committed revision after a failed candidate or worker restart. The caller is
// responsible for revision concurrency. A failure never implies rollback.
func (e *Engine) Apply(ctx context.Context, revision uint64, desired Desired) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lock == nil {
		return Snapshot{}, errors.New("network engine is closed")
	}
	if err := Validate(desired); err != nil {
		return e.snapshotLocked(ctx), err
	}
	if err := ctx.Err(); err != nil {
		return e.snapshotLocked(ctx), err
	}
	now := time.Now()
	d := projectRouteExpiry(projectAddressExpiry(normalized(cloneDesired(desired)), now), now)
	e.state.AttemptedRevision, e.state.Attempted = revision, &d
	e.state.Status, e.state.LastError = "applying", ""
	if err := e.save(); err != nil {
		return e.snapshotLocked(ctx), &ApplyError{Stage: "journal", Err: err}
	}
	p := &apply{e: e, ctx: ctx}
	err := p.reconcile(d)
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
	return e.snapshotLocked(ctx), err
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
		for _, name := range firewallInterfaces(d.Firewall) {
			names[name] = true
		}
		for _, r := range d.PolicyRules {
			tables[r.Table] = true
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
		for _, t := range d.DSLite {
			names[t.Name] = true
			names[t.Underlay] = true
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
	for _, t := range e.state.Tunnels {
		names[t.Name] = true
	}
	for _, l := range e.state.Links {
		names[l.Definition.Name] = true
	}
	for _, a := range e.state.Attributes {
		names[a.Key.Interface] = true
	}
	for _, r := range e.state.Routes {
		tables[routeTable(r)] = true
	}
	for _, r := range e.state.PolicyRules {
		tables[r.Table] = true
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
func routeMatches(live RouteState, want Route) bool {
	observed := live.Route
	if !routeLifetimeMatches(observed.ValidUntil, want.ValidUntil) {
		return false
	}
	observed.ValidUntil = want.ValidUntil
	return sameRoute(observed, want) && live.Protocol == RouteProtocol && live.Type == 1 && live.Source == "" && live.Multipath == (len(live.NextHops) > 0) && !live.Advanced
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
}

// A native multi-attribute update may fail after changing some attributes.
// Propagate that uncertainty instead of reporting an untouched kernel.
type partialMutationError struct{ error }

func (p *apply) failure(stage string, err error) error {
	return &ApplyError{Stage: stage, Partial: p.changed > 0, Err: err}
}
func (p *apply) change(stage string, fn func() error, record func()) error {
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
	for _, l := range d.Interfaces {
		wantedSettings[settingKey{Kind: "up", Interface: l.Name}] = 1
		if l.MTU != 0 {
			wantedSettings[settingKey{Kind: "mtu", Interface: l.Name}] = l.MTU
		}
		if l.IPv6AcceptRA != nil {
			wantedSettings[settingKey{Kind: "accept_ra", Interface: l.Name}] = *l.IPv6AcceptRA
		}
		if l.IPv6Autoconf != nil {
			wantedSettings[settingKey{Kind: "autoconf", Interface: l.Name}] = boolInt(*l.IPv6Autoconf)
		}
	}
	for _, t := range d.DSLite {
		wantedSettings[settingKey{Kind: "up", Interface: t.Name}] = 1
	}
	if d.Forwarding.IPv4 != nil {
		wantedSettings[settingKey{Kind: "forwarding4"}] = boolInt(*d.Forwarding.IPv4)
	}
	if d.Forwarding.IPv6 != nil {
		wantedSettings[settingKey{Kind: "forwarding6"}] = boolInt(*d.Forwarding.IPv6)
	}
	// Keep the kernel RA/SLAAC writers disabled while removing the client-owned
	// routes and addresses. Restore these settings after reconciliation finishes.
	earlySettings := make(map[settingKey]int, len(wantedSettings))
	for key, value := range wantedSettings {
		earlySettings[key] = value
	}
	for _, old := range p.e.state.Settings {
		if raSetting(old.Key) {
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
	if err := p.removeAddresses(d); err != nil {
		return err
	}
	if err := p.links(d.Links); err != nil {
		return err
	}
	if err := p.wireGuard(d); err != nil {
		return err
	}
	tunnels := map[string]bool{}
	for _, t := range d.DSLite {
		tunnels[t.Name] = true
	}
	for _, l := range d.Interfaces {
		if !tunnels[l.Name] {
			if err := p.interfaceConfig(l); err != nil {
				return err
			}
		}
	}
	if err := p.tunnels(d.DSLite); err != nil {
		return err
	}
	for _, t := range d.DSLite {
		configured := false
		for _, l := range d.Interfaces {
			configured = configured || l.Name == t.Name
		}
		if configured {
			continue // interfaceConfig applies sysctls before UP.
		}
		if err := p.set(settingKey{Kind: "up", Interface: t.Name}, 1); err != nil {
			return err
		}
	}
	for _, l := range d.Interfaces {
		if tunnels[l.Name] {
			if err := p.interfaceConfig(l); err != nil {
				return err
			}
		}
	}
	if err := p.removePolicyRules(d.PolicyRules); err != nil {
		return err
	}
	if err := p.routes(d.Routes); err != nil {
		return err
	}
	if err := p.policyRules(d.PolicyRules); err != nil {
		return err
	}
	if err := p.removeTunnels(d); err != nil {
		return err
	}
	if err := p.removeLinks(d); err != nil {
		return err
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
	return p.restoreSettings(wantedSettings)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (p *apply) interfaceConfig(l Interface) error {
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
	if l.MTU != 0 {
		if err := p.set(settingKey{Kind: "mtu", Interface: l.Name}, l.MTU); err != nil {
			return err
		}
	}
	if err := p.set(settingKey{Kind: "up", Interface: l.Name}, 1); err != nil {
		return err
	}
	for _, a := range l.Addresses {
		if err := p.addressConfig(l, a); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) removeAddresses(d Desired) error {
	want := map[addressKey]bool{}
	for _, l := range d.Interfaces {
		for _, a := range l.Addresses {
			want[addressKey{l.Name, a}] = true
		}
	}
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

func (p *apply) routes(want []Route) error {
	for i := 0; i < len(p.e.state.Routes); {
		r := p.e.state.Routes[i]
		keep := false
		for _, v := range want {
			if sameRoute(r, v) {
				keep = true
				break
			}
		}
		if keep {
			i++
			continue
		}
		live, err := p.e.kernel.Routes(routeTable(r))
		if err != nil {
			return p.failure("route remove read", err)
		}
		found := false
		for _, v := range live {
			if routeMatches(v, r) {
				found = true
				break
			}
		}
		forget := func() { p.e.state.Routes = append(p.e.state.Routes[:i], p.e.state.Routes[i+1:]...) }
		if found {
			if err := p.change("route remove", func() error { return p.e.kernel.DeleteRoute(r) }, forget); err != nil {
				return err
			}
		} else {
			forget()
			if err := p.checkpoint("forget absent route"); err != nil {
				return err
			}
		}
	}
	for _, r := range want {
		live, err := p.e.kernel.Routes(routeTable(r))
		if err != nil {
			return p.failure("route read", err)
		}
		found := false
		for _, v := range live {
			if routeSlot(v.Route) != routeSlot(r) {
				continue
			}
			if routeMatches(v, r) && p.e.ownsRoute(v) {
				found = true
				continue
			}
			return p.failure("route ownership", fmt.Errorf("route %s table %d metric %d conflicts with an unowned or changed route", r.Destination, r.Table, r.Metric))
		}
		if found {
			continue
		}
		if err := p.change("route add", func() error { return p.e.kernel.AddRoute(r) }, func() {
			for _, v := range p.e.state.Routes {
				if sameRoute(v, r) {
					return
				}
			}
			p.e.state.Routes = append(p.e.state.Routes, r)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *apply) tunnels(want []DSLite) error {
	for _, t := range want {
		parent, err := p.e.kernel.Interface(t.Underlay)
		if err != nil {
			return p.failure("DS-Lite underlay", err)
		}
		if t.MTU == 0 {
			t.MTU = parent.MTU - 40
		}
		if t.MTU < 68 || t.MTU > 65495 || t.MTU > parent.MTU-40 {
			return p.failure("DS-Lite MTU", fmt.Errorf("%s inner MTU %d exceeds supported range or underlay MTU %d minus 40", t.Name, t.MTU, parent.MTU))
		}
		localPresent := false
		for _, a := range parent.Addresses {
			if addressIP(a.Address) == t.Local {
				localPresent = true
			}
		}
		if !localPresent {
			return p.failure("DS-Lite local address", fmt.Errorf("%s is not assigned to underlay %s", t.Local, t.Underlay))
		}
		live, err := p.e.kernel.Interface(t.Name)
		if errors.Is(err, errLinkNotFound) {
			if err := p.change("DS-Lite create", func() error { return p.e.kernel.CreateDSLite(t) }, func() { p.recordTunnel(t) }); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("DS-Lite read", err)
		}
		owned := false
		for _, old := range p.e.state.Tunnels {
			if old.Name == t.Name {
				owned = true
				break
			}
		}
		if !owned || live.Alias != tunnelAlias || live.DSLite == nil {
			return p.failure("DS-Lite ownership", fmt.Errorf("link %s exists without matching ownership", t.Name))
		}
		if *live.DSLite == t {
			continue
		}
		if err := p.change("DS-Lite update", func() error { return p.e.kernel.UpdateDSLite(t) }, func() { p.recordTunnel(t) }); err != nil {
			return err
		}
	}
	return nil
}
func (p *apply) recordTunnel(t DSLite) {
	for i := range p.e.state.Tunnels {
		if p.e.state.Tunnels[i].Name == t.Name {
			p.e.state.Tunnels[i] = t
			return
		}
	}
	p.e.state.Tunnels = append(p.e.state.Tunnels, t)
}

func (p *apply) removeTunnels(d Desired) error {
	for i := 0; i < len(p.e.state.Tunnels); {
		t := p.e.state.Tunnels[i]
		keep := false
		for _, v := range d.DSLite {
			if v.Name == t.Name {
				keep = true
			}
		}
		if keep {
			i++
			continue
		}
		for _, v := range d.Interfaces {
			if v.Name == t.Name {
				return p.failure("DS-Lite remove", fmt.Errorf("remove interface declaration %s before deleting its tunnel", t.Name))
			}
		}
		l, err := p.e.kernel.Interface(t.Name)
		forget := func() { p.e.state.Tunnels = append(p.e.state.Tunnels[:i], p.e.state.Tunnels[i+1:]...) }
		if errors.Is(err, errLinkNotFound) {
			forget()
			if err := p.checkpoint("forget absent DS-Lite tunnel"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("DS-Lite remove read", err)
		}
		if l.Alias != tunnelAlias || l.DSLite == nil {
			return p.failure("DS-Lite remove ownership", fmt.Errorf("link %s has changed ownership", t.Name))
		}
		if len(l.Addresses) != 0 {
			return p.failure("DS-Lite remove", fmt.Errorf("link %s still has addresses; refusing to delete them implicitly", t.Name))
		}
		rs, err := p.e.kernel.Routes(0)
		if err != nil {
			return p.failure("DS-Lite route references", err)
		}
		for _, r := range rs {
			if r.Interface == t.Name {
				return p.failure("DS-Lite remove", fmt.Errorf("link %s still has routes; refusing to delete them implicitly", t.Name))
			}
		}
		if err := p.change("DS-Lite remove", func() error { return p.e.kernel.DeleteDSLite(t.Name) }, forget); err != nil {
			return err
		}
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
	if want != nil {
		for _, name := range firewallInterfaces(want) {
			if _, err := p.e.kernel.Interface(name); err != nil {
				return p.failure("firewall interface", err)
			}
		}
	}
	return p.change("firewall atomic replace", func() error { return p.e.kernel.ReplaceFirewall(want) }, func() { p.e.state.FirewallOwned = want != nil })
}
