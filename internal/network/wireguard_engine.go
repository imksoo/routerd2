package network

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// The durable intent contains references and public peer data only. The live
// kernel readback containing actual private keys never enters this structure.
type wireGuardRecorded struct {
	PrivateKeyID  string                  `json:"private_key_id,omitempty"`
	PublicKey     string                  `json:"public_key,omitempty"`
	ListenPort    int                     `json:"listen_port"`
	AutomaticPort bool                    `json:"automatic_port,omitempty"`
	FirewallMark  uint32                  `json:"firewall_mark"`
	Peers         []wireGuardRecordedPeer `json:"peers,omitempty"`
}

type wireGuardRecordedPeer struct {
	PublicKey      string   `json:"public_key"`
	PresharedKeyID string   `json:"preshared_key_id,omitempty"`
	AllowedIPs     []string `json:"allowed_ips,omitempty"`
	Endpoint       string   `json:"endpoint,omitempty"`
	// A previous interrupted SET changed the declaration, so a live endpoint
	// may be an uncommitted bootstrap value rather than authenticated roaming.
	// Preserve this uncertainty across another interrupted rollback.
	EndpointUncertain   bool `json:"endpoint_uncertain,omitempty"`
	PersistentKeepalive int  `json:"persistent_keepalive"`
}

type ownedWireGuard struct {
	Interface   string             `json:"interface"`
	Index       int                `json:"index"`
	Last        wireGuardRecorded  `json:"last"`
	Pending     *wireGuardRecorded `json:"pending,omitempty"`
	Quarantined bool               `json:"quarantined"`
}

func (e *Engine) wireGuardRecord(name string) *ownedWireGuard {
	for i := range e.state.WireGuard {
		if e.state.WireGuard[i].Interface == name {
			return &e.state.WireGuard[i]
		}
	}
	return nil
}

func clearWireGuardDevice(device *wireGuardDevice) {
	clear(device.PrivateKey[:])
	for i := range device.Peers {
		clear(device.Peers[i].PresharedKey[:])
	}
}

func (p *apply) materializeWireGuard(want WireGuard) (wireGuardRecorded, error) {
	id, err := p.resolveWireGuardSecret(want.PrivateKeyFile, "private", want.GeneratePrivateKey)
	if err != nil {
		return wireGuardRecorded{}, err
	}
	key, err := p.e.loadWireGuardSecret(id)
	if err != nil {
		return wireGuardRecorded{}, err
	}
	public, err := key.public()
	clear(key[:])
	if err != nil {
		return wireGuardRecorded{}, err
	}
	out := wireGuardRecorded{PrivateKeyID: id, PublicKey: public.encoded(), ListenPort: want.ListenPort, AutomaticPort: want.ListenPort == 0, FirewallMark: want.FirewallMark}
	if old := p.e.wireGuardRecord(want.Interface); out.AutomaticPort && old != nil && old.Last.AutomaticPort {
		out.ListenPort = old.Last.ListenPort
	}
	for _, peer := range want.Peers {
		if peer.PublicKey == out.PublicKey {
			return out, errors.New("a WireGuard interface cannot have its own public key as a peer")
		}
		entry := wireGuardRecordedPeer{PublicKey: peer.PublicKey, AllowedIPs: append([]string(nil), peer.AllowedIPs...), Endpoint: peer.Endpoint, PersistentKeepalive: peer.PersistentKeepalive}
		if peer.PresharedKeyFile != "" {
			entry.PresharedKeyID, err = p.resolveWireGuardSecret(peer.PresharedKeyFile, "preshared", false)
			if err != nil {
				return out, err
			}
		}
		out.Peers = append(out.Peers, entry)
	}
	return out, nil
}

func wireGuardPeerMap(record wireGuardRecorded) map[string]wireGuardRecordedPeer {
	peers := make(map[string]wireGuardRecordedPeer, len(record.Peers))
	for _, peer := range record.Peers {
		peers[peer.PublicKey] = peer
	}
	return peers
}

func wireGuardPrivateMatches(key wireGuardKey, id string) bool {
	if id == "" {
		return key.zero()
	}
	return !key.zero() && wireGuardSecretID(key) == id
}

func wireGuardPeerMatches(live wireGuardNativePeer, saved wireGuardRecordedPeer) bool {
	return live.PublicKey.encoded() == saved.PublicKey && wireGuardPrivateMatches(live.PresharedKey, saved.PresharedKeyID) && int(live.PersistentKeepalive) == saved.PersistentKeepalive && reflect.DeepEqual(live.AllowedIPs, saved.AllowedIPs)
}

func wireGuardMatches(live wireGuardDevice, saved wireGuardRecorded, autoPort bool) bool {
	if !wireGuardPrivateMatches(live.PrivateKey, saved.PrivateKeyID) || live.PublicKey.zero() != (saved.PublicKey == "") || !live.PublicKey.zero() && live.PublicKey.encoded() != saved.PublicKey || live.FirewallMark != saved.FirewallMark {
		return false
	}
	if int(live.ListenPort) != saved.ListenPort && !(autoPort && saved.AutomaticPort && saved.ListenPort == 0) {
		return false
	}
	if len(live.Peers) != len(saved.Peers) {
		return false
	}
	peers := wireGuardPeerMap(saved)
	for _, peer := range live.Peers {
		previous, exists := peers[peer.PublicKey.encoded()]
		if !exists || !wireGuardPeerMatches(peer, previous) {
			return false
		}
	}
	return true
}

// Interrupted SET operations may leave known fields/peer subsets from the
// persisted before-image and intent. This is narrower than treating a matching
// interface alias as permission to erase arbitrary third-party peers or keys.
func wireGuardOwned(live wireGuardDevice, saved ownedWireGuard) bool {
	if live.Index != saved.Index {
		return false
	}
	if saved.Pending == nil {
		return wireGuardMatches(live, saved.Last, false)
	}
	if !saved.Quarantined {
		return false
	}
	a, b := saved.Last, *saved.Pending
	if !wireGuardPrivateMatches(live.PrivateKey, a.PrivateKeyID) && !wireGuardPrivateMatches(live.PrivateKey, b.PrivateKeyID) {
		return false
	}
	public, err := live.PrivateKey.public()
	if err != nil || !public.equal(live.PublicKey) {
		return false
	}
	if live.FirewallMark != a.FirewallMark && live.FirewallMark != b.FirewallMark {
		return false
	}
	if int(live.ListenPort) != a.ListenPort && int(live.ListenPort) != b.ListenPort && !(b.AutomaticPort && b.ListenPort == 0) {
		return false
	}
	before, after := wireGuardPeerMap(a), wireGuardPeerMap(b)
	for _, peer := range live.Peers {
		key := peer.PublicKey.encoded()
		old, had := before[key]
		next, has := after[key]
		if !had && !has {
			return false
		}
		if !wireGuardPrivateMatches(peer.PresharedKey, old.PresharedKeyID) && !wireGuardPrivateMatches(peer.PresharedKey, next.PresharedKeyID) && !peer.PresharedKey.zero() {
			return false
		}
		if int(peer.PersistentKeepalive) != old.PersistentKeepalive && int(peer.PersistentKeepalive) != next.PersistentKeepalive && peer.PersistentKeepalive != 0 {
			return false
		}
		prefixes := map[string]bool{}
		for _, prefix := range append(append([]string(nil), old.AllowedIPs...), next.AllowedIPs...) {
			prefixes[prefix] = true
		}
		for _, prefix := range peer.AllowedIPs {
			if !prefixes[prefix] {
				return false
			}
		}
	}
	return true
}

func recordedWireGuardLive(live wireGuardDevice, old ownedWireGuard) wireGuardRecorded {
	out := wireGuardRecorded{ListenPort: int(live.ListenPort), FirewallMark: live.FirewallMark, AutomaticPort: old.Last.AutomaticPort}
	if !live.PrivateKey.zero() {
		out.PrivateKeyID, out.PublicKey = wireGuardSecretID(live.PrivateKey), live.PublicKey.encoded()
	}
	declarations := wireGuardPeerMap(old.Last)
	pending := map[string]wireGuardRecordedPeer{}
	if old.Pending != nil {
		pending = wireGuardPeerMap(*old.Pending)
	}
	for _, peer := range live.Peers {
		key := peer.PublicKey.encoded()
		declaration := declarations[key]
		entry := wireGuardRecordedPeer{PublicKey: key, AllowedIPs: append([]string(nil), peer.AllowedIPs...), Endpoint: declaration.Endpoint, EndpointUncertain: declaration.EndpointUncertain, PersistentKeepalive: int(peer.PersistentKeepalive)}
		if next, exists := pending[key]; exists && next.Endpoint != declaration.Endpoint {
			entry.EndpointUncertain = true
		}
		if !peer.PresharedKey.zero() {
			entry.PresharedKeyID = wireGuardSecretID(peer.PresharedKey)
		}
		out.Peers = append(out.Peers, entry)
	}
	return out
}

func (e *Engine) nativeWireGuard(saved wireGuardRecorded, old ownedWireGuard, live wireGuardDevice) (wireGuardDevice, error) {
	out := wireGuardDevice{Index: live.Index, ListenPort: uint16(saved.ListenPort), FirewallMark: saved.FirewallMark}
	var err error
	if saved.PrivateKeyID != "" {
		out.PrivateKey, err = e.loadWireGuardSecret(saved.PrivateKeyID)
		if err != nil {
			return out, err
		}
		out.PublicKey, err = out.PrivateKey.public()
		if err != nil {
			clearWireGuardDevice(&out)
			return out, err
		}
	}
	declarations := wireGuardPeerMap(old.Last)
	pending := map[string]wireGuardRecordedPeer{}
	if old.Pending != nil {
		pending = wireGuardPeerMap(*old.Pending)
	}
	observed := map[string]wireGuardNativePeer{}
	for _, peer := range live.Peers {
		observed[peer.PublicKey.encoded()] = peer
	}
	for _, peer := range saved.Peers {
		key, err := parseWireGuardKey(peer.PublicKey)
		if err != nil {
			clearWireGuardDevice(&out)
			return out, err
		}
		entry := wireGuardNativePeer{PublicKey: key, AllowedIPs: append([]string(nil), peer.AllowedIPs...), Endpoint: peer.Endpoint, PersistentKeepalive: uint16(peer.PersistentKeepalive)}
		// Roaming endpoints and passive peers are observations, not drift. Carry
		// the last authenticated endpoint into a replacement only when the
		// declarative endpoint did not change. Static -> passive clears it by
		// recreating this peer without WGPEER_A_ENDPOINT.
		previous, exists := declarations[peer.PublicKey]
		preserveEndpoint := exists && !previous.EndpointUncertain && previous.Endpoint == peer.Endpoint
		if next, exists := pending[peer.PublicKey]; exists && next.Endpoint != previous.Endpoint {
			preserveEndpoint = false
		}
		if preserveEndpoint {
			if current, present := observed[peer.PublicKey]; present && current.Endpoint != "" {
				entry.Endpoint = current.Endpoint
			}
		}
		if peer.PresharedKeyID != "" {
			entry.PresharedKey, err = e.loadWireGuardSecret(peer.PresharedKeyID)
			if err != nil {
				clearWireGuardDevice(&out)
				return out, err
			}
		}
		out.Peers = append(out.Peers, entry)
	}
	return out, nil
}

func (p *apply) preflightWireGuard(d Desired) error {
	if len(d.WireGuard) == 0 && len(p.e.state.WireGuard) == 0 {
		return nil
	}
	k, supported := p.e.kernel.(wireGuardKernel)
	if !supported {
		return p.failure("WireGuard backend", errors.New("native WireGuard is unavailable"))
	}
	for _, record := range p.e.state.WireGuard {
		link, err := p.e.kernel.Interface(record.Interface)
		if errors.Is(err, errLinkNotFound) {
			continue
		}
		if err != nil {
			return p.failure("WireGuard link read", err)
		}
		if !p.e.ownsLink(link) || link.Index != record.Index || link.Kind != "wireguard" {
			return p.failure("WireGuard ownership", fmt.Errorf("%s no longer has the recorded owned link identity", record.Interface))
		}
		live, err := k.ReadWireGuard(link.Index)
		if err != nil {
			clearWireGuardDevice(&live)
			return p.failure("WireGuard ownership read", err)
		}
		owned := wireGuardOwned(live, record)
		clearWireGuardDevice(&live)
		if !owned {
			return p.failure("WireGuard ownership", fmt.Errorf("%s key, port, mark or peer configuration changed outside routerd2", record.Interface))
		}
	}
	// Resolve every key and validate self-peering before mutating ANY device.
	p.wireGuardWanted = map[string]wireGuardRecorded{}
	for _, wanted := range d.WireGuard {
		material, err := p.materializeWireGuard(wanted)
		if err != nil {
			return p.failure("WireGuard secret", fmt.Errorf("%s: %w", wanted.Interface, err))
		}
		p.wireGuardWanted[wanted.Interface] = material
	}
	return nil
}

func (p *apply) setWireGuardUp(name string, index int, up bool) error {
	link, err := p.e.kernel.Interface(name)
	if err != nil || link.Index != index || !p.e.ownsLink(link) || link.Kind != "wireguard" {
		return p.failure("WireGuard link ownership", fmt.Errorf("%s link identity changed before administrative update", name))
	}
	key, value := settingKey{Kind: "up", Interface: name}, boolInt(up)
	old := p.e.settingRecord(key)
	if old != nil && old.Index != 0 && old.Index != index {
		return p.failure("WireGuard setting ownership", fmt.Errorf("%s was replaced", name))
	}
	if link.Up == up {
		if old != nil {
			old.Last, old.Index, old.Attempted = value, index, nil
			return p.checkpoint("WireGuard administrative state")
		}
		return nil
	}
	if old == nil {
		p.e.state.Settings = append(p.e.state.Settings, ownedSetting{Key: key, Before: boolInt(link.Up), Last: boolInt(link.Up), Index: index})
		old = &p.e.state.Settings[len(p.e.state.Settings)-1]
	}
	old.Attempted = &value
	if err := p.checkpoint("WireGuard administrative intent"); err != nil {
		return err
	}
	return p.change("WireGuard administrative state", func() error { return p.e.kernel.(wireGuardKernel).SetWireGuardUp(index, up) }, func() {
		old.Last, old.Index, old.Attempted = value, index, nil
	})
}

func (p *apply) wireGuard(d Desired) (result error) {
	if len(d.WireGuard) == 0 && len(p.e.state.WireGuard) == 0 {
		return nil
	}
	k := p.e.kernel.(wireGuardKernel)
	var active *ownedWireGuard
	defer func() {
		if result != nil && active != nil {
			result = errors.Join(result, p.quarantineWireGuard(active))
		}
	}()
	up := map[string]bool{}
	interfaces := map[string]Interface{}
	for _, i := range d.Interfaces {
		up[i.Name] = true
		interfaces[i.Name] = i
	}
	names := map[string]bool{}
	for name := range p.wireGuardWanted {
		names[name] = true
	}
	for _, old := range p.e.state.WireGuard {
		names[old.Interface] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		want := p.wireGuardWanted[name] // Omission explicitly clears keys/peers.
		link, err := p.e.kernel.Interface(name)
		if errors.Is(err, errLinkNotFound) {
			p.forgetWireGuard(name)
			if err := p.checkpoint("forget absent WireGuard device"); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return p.failure("WireGuard link read", err)
		}
		if !p.e.ownsLink(link) || link.Kind != "wireguard" {
			return p.failure("WireGuard ownership", fmt.Errorf("%s is not an owned WireGuard link", name))
		}
		old := p.e.wireGuardRecord(name)
		if old != nil && old.Index != link.Index {
			// preflight verified that the old device was absent; links() has
			// exclusively created a new, empty device with a different index.
			p.forgetWireGuard(name)
			old = nil
		}
		live, err := k.ReadWireGuard(link.Index)
		if err != nil {
			clearWireGuardDevice(&live)
			return p.failure("WireGuard read", err)
		}
		if old == nil {
			if !wireGuardMatches(live, wireGuardRecorded{}, false) {
				clearWireGuardDevice(&live)
				return p.failure("WireGuard ownership", fmt.Errorf("%s already has unowned WireGuard configuration", name))
			}
			p.e.state.WireGuard = append(p.e.state.WireGuard, ownedWireGuard{Interface: name, Index: link.Index})
			old = &p.e.state.WireGuard[len(p.e.state.WireGuard)-1]
		} else if !wireGuardOwned(live, *old) {
			clearWireGuardDevice(&live)
			return p.failure("WireGuard ownership", fmt.Errorf("%s configuration changed outside routerd2", name))
		}
		if old.Pending == nil && reflect.DeepEqual(old.Last, want) && wireGuardMatches(live, want, false) && (!up[name] || link.Up) {
			clearWireGuardDevice(&live)
			continue
		}
		native, err := p.e.nativeWireGuard(want, *old, live)
		if err != nil {
			clearWireGuardDevice(&live)
			return p.failure("WireGuard materialization", err)
		}
		// A second interruption during rollback must be recoverable from the
		// verified actual subset, not only from the original complete set.
		old.Last = recordedWireGuardLive(live, *old)
		clearWireGuardDevice(&live)
		old.Pending, old.Quarantined = &want, true
		if err := p.checkpoint("WireGuard replacement intent"); err != nil {
			clearWireGuardDevice(&native)
			return err
		}
		active = old // Durable DOWN intent authorizes cleanup even if journal I/O now fails.
		if err := p.setWireGuardUp(name, link.Index, false); err != nil {
			clearWireGuardDevice(&native)
			return err
		}
		err = p.change("WireGuard replace", func() error { return k.ReplaceWireGuard(link.Index, native) }, nil)
		clearWireGuardDevice(&native)
		if err != nil {
			return err // Intent stays durable and the device remains DOWN.
		}
		readback, err := k.ReadWireGuard(link.Index)
		matches := err == nil && wireGuardMatches(readback, want, true)
		clearWireGuardDevice(&readback)
		if !matches {
			if err == nil {
				err = errors.New("kernel readback differs from the complete desired peer set")
			}
			return p.failure("WireGuard readback", err)
		}
		if up[name] {
			// A newly created WireGuard link reaches its first UP transition
			// here, before interfaceConfig. Stop optional kernel RA/SLAAC
			// writers before that transition, just as for other link kinds.
			if err := p.interfaceRASettings(interfaces[name]); err != nil {
				return err
			}
			if err := p.setWireGuardUp(name, link.Index, true); err != nil {
				return err
			}
			readback, err = k.ReadWireGuard(link.Index)
			matches = err == nil && wireGuardMatches(readback, want, true)
			if matches && want.AutomaticPort && want.ListenPort == 0 {
				want.ListenPort = int(readback.ListenPort)
				matches = want.ListenPort != 0
			}
			clearWireGuardDevice(&readback)
			if !matches {
				if err == nil {
					err = errors.New("WireGuard post-UP readback differs")
				}
				return p.failure("WireGuard post-UP readback", err)
			}
		}
		if err := p.ctx.Err(); err != nil {
			return p.failure("WireGuard replacement canceled", err)
		}
		old.Last, old.Pending, old.Quarantined = want, nil, false
		if err := p.checkpoint("WireGuard acknowledgment"); err != nil {
			// Restore the durable pending state in memory and close the device.
			old.Pending, old.Quarantined = &want, true
			return err
		}
		active = nil
	}
	return nil
}

// The replacement intent is already durable before this cleanup is armed.
// Trying to persist another DOWN intent first would leave the device UP when
// the original error was a journal/fsync failure. Verify ownership again, issue
// DOWN independently of the canceled apply context, and only then persist the
// resulting administrative state. Unknown/foreign state is never changed.
func (p *apply) quarantineWireGuard(record *ownedWireGuard) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 5*time.Second)
	defer cancel()
	ownsLink := func(link InterfaceState) bool {
		return link.Name == record.Interface && link.Index == record.Index && link.Kind == "wireguard" && p.e.ownsLink(link)
	}
	ownsDevice := func(device wireGuardDevice) bool { return wireGuardOwned(device, *record) }
	var changed bool
	var err error
	if backend, ok := p.e.kernel.(wireGuardQuarantineKernel); ok {
		changed, err = backend.QuarantineWireGuard(ctx, record.Interface, record.Index, ownsLink, ownsDevice)
	} else {
		changed, err = quarantineWireGuardFallback(ctx, p.e.kernel, record.Interface, record.Index, ownsLink, ownsDevice)
	}
	if changed {
		p.changed++
	}
	if err != nil {
		return p.failure("WireGuard quarantine", err)
	}
	key := settingKey{Kind: "up", Interface: record.Interface}
	if old := p.e.settingRecord(key); old != nil {
		old.Last, old.Index, old.Attempted = 0, record.Index, nil
	} else {
		p.e.state.Settings = append(p.e.state.Settings, ownedSetting{Key: key, Before: 0, Last: 0, Index: record.Index})
	}
	if err := ctx.Err(); err != nil {
		return p.failure("WireGuard quarantine journal", err)
	}
	return p.checkpoint("WireGuard quarantine journal")
}

// Inert fake kernels use the same ownership protocol without Linux sockets.
// Production linuxKernel implements the deadline-aware native method above.
func quarantineWireGuardFallback(ctx context.Context, k kernel, name string, index int, ownsLink func(InterfaceState) bool, ownsDevice func(wireGuardDevice) bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	link, err := k.Interface(name)
	if err != nil {
		return false, err
	}
	if !ownsLink(link) {
		return false, errors.New("WireGuard quarantine refused: link ownership changed")
	}
	backend := k.(wireGuardKernel)
	device, err := backend.ReadWireGuard(index)
	owned := err == nil && ownsDevice(device)
	clearWireGuardDevice(&device)
	if err != nil {
		return false, err
	}
	if !owned {
		return false, errors.New("WireGuard quarantine refused: device configuration changed outside routerd2")
	}
	if !link.Up {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	writeErr := backend.SetWireGuardUp(index, false)
	link, err = k.Interface(name)
	if err != nil || !ownsLink(link) || link.Up {
		return true, errors.Join(writeErr, err, errors.New("WireGuard quarantine could not verify the owned device DOWN"))
	}
	return true, nil
}

func (p *apply) forgetWireGuard(name string) {
	kept := p.e.state.WireGuard[:0]
	for _, record := range p.e.state.WireGuard {
		if record.Interface != name {
			kept = append(kept, record)
		}
	}
	p.e.state.WireGuard = kept
}

func validateWireGuardJournal(s journal) error {
	bindings, ids := map[string]bool{}, map[string]bool{}
	for _, binding := range s.WireGuardSecrets {
		key := binding.Kind + "\x00" + binding.Path
		if !secretFilePath(binding.Path) || binding.Kind != "private" && binding.Kind != "preshared" || !validWireGuardSecretID(binding.ID) || bindings[key] {
			return errors.New("invalid or duplicate WireGuard secret binding")
		}
		bindings[key], ids[binding.ID] = true, true
	}
	seen := map[string]bool{}
	for _, old := range s.WireGuard {
		if !validName(old.Interface) || old.Index <= 0 || seen[old.Interface] || old.Quarantined != (old.Pending != nil) {
			return errors.New("invalid WireGuard ownership record")
		}
		seen[old.Interface] = true
		definitions := []wireGuardRecorded{old.Last}
		if old.Pending != nil {
			definitions = append(definitions, *old.Pending)
		}
		for _, saved := range definitions {
			if saved.PrivateKeyID != "" && !ids[saved.PrivateKeyID] || saved.PrivateKeyID == "" && saved.PublicKey != "" {
				return errors.New("WireGuard record references an unbound private key")
			}
			if saved.PrivateKeyID != "" {
				key, err := parseWireGuardKey(saved.PublicKey)
				if err != nil || key.zero() {
					return errors.New("WireGuard record has invalid public key")
				}
			}
			device := WireGuard{Interface: old.Interface, PrivateKeyFile: "/journal-key-reference", ListenPort: saved.ListenPort, FirewallMark: saved.FirewallMark}
			for _, peer := range saved.Peers {
				if peer.PresharedKeyID != "" && !ids[peer.PresharedKeyID] || peer.PublicKey == saved.PublicKey {
					return errors.New("WireGuard record contains an unbound peer secret or self peer")
				}
				device.Peers = append(device.Peers, WireGuardPeer{PublicKey: peer.PublicKey, AllowedIPs: peer.AllowedIPs, Endpoint: peer.Endpoint, PersistentKeepalive: peer.PersistentKeepalive})
			}
			if err := validateWireGuard(Desired{Links: []Link{{Name: old.Interface, Kind: "wireguard"}}, WireGuard: []WireGuard{device}}); err != nil {
				return err
			}
		}
	}
	return nil
}
