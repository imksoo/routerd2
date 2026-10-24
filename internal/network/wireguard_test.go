//go:build linux

package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeWireGuardKernel struct {
	*fakeKernel
	devices       map[int]wireGuardDevice
	failAfterPeer int
}

func fakeWireGuard() *fakeWireGuardKernel {
	return &fakeWireGuardKernel{fakeKernel: fake(), devices: map[int]wireGuardDevice{}, failAfterPeer: -1}
}

func cloneWireGuardDevice(device wireGuardDevice) wireGuardDevice {
	out := device
	out.Peers = append([]wireGuardNativePeer(nil), device.Peers...)
	for i := range out.Peers {
		out.Peers[i].AllowedIPs = append([]string(nil), device.Peers[i].AllowedIPs...)
	}
	return out
}

func (f *fakeWireGuardKernel) linkAt(index int) (InterfaceState, error) {
	for _, link := range f.links {
		if link.Index == index && link.Kind == "wireguard" {
			return link, nil
		}
	}
	return InterfaceState{}, errLinkNotFound
}

func (f *fakeWireGuardKernel) ReadWireGuard(index int) (wireGuardDevice, error) {
	if _, err := f.linkAt(index); err != nil {
		return wireGuardDevice{}, err
	}
	device := cloneWireGuardDevice(f.devices[index])
	device.Index = index
	return device, nil
}

func (f *fakeWireGuardKernel) ReplaceWireGuard(index int, wanted wireGuardDevice) error {
	link, err := f.linkAt(index)
	if err != nil {
		return err
	}
	if link.Up {
		return errors.New("fixture rejects replacement of an UP device")
	}
	if err := f.op("wireguard-replace"); err != nil {
		return err
	}
	device := cloneWireGuardDevice(wanted)
	device.Index = index
	if f.failAfterPeer >= 0 {
		device.Peers = device.Peers[:min(f.failAfterPeer, len(device.Peers))]
		f.devices[index] = device
		return &partialMutationError{errors.New("injected interrupted WireGuard peer set")}
	}
	f.devices[index] = device
	return nil
}

func (f *fakeWireGuardKernel) SetWireGuardUp(index int, up bool) error {
	link, err := f.linkAt(index)
	if err != nil {
		return err
	}
	if err := f.op(fmt.Sprintf("wireguard-up-%t", up)); err != nil {
		return err
	}
	link.Up = up
	f.links[link.Name] = link
	if up {
		device := f.devices[index]
		if device.ListenPort == 0 {
			device.ListenPort = 50123
		}
		f.devices[index] = device
	}
	return nil
}

func fixtureWireGuardKey(seed byte) wireGuardKey {
	var key wireGuardKey
	for i := range key {
		key[i] = seed + byte(i)
	}
	clampWireGuardPrivate(&key)
	return key
}

func fixtureWireGuardPublic(seed byte) string {
	key, _ := fixtureWireGuardKey(seed).public()
	return key.encoded()
}

func fixtureWireGuardDesired(t *testing.T) Desired {
	t.Helper()
	dir := t.TempDir()
	private, psk := fixtureWireGuardKey(31), fixtureWireGuardKey(73)
	for name, key := range map[string]wireGuardKey{"private": private, "psk": psk} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(key.encoded()+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Desired{Links: []Link{{Name: "wg0", Kind: "wireguard"}}, Interfaces: []Interface{{Name: "wg0", MTU: 1420, Addresses: []string{"10.99.0.1/24"}}}, WireGuard: []WireGuard{{Interface: "wg0", PrivateKeyFile: filepath.Join(dir, "private"), Peers: []WireGuardPeer{{PublicKey: fixtureWireGuardPublic(81), PresharedKeyFile: filepath.Join(dir, "psk"), Endpoint: "192.0.2.2:51820", AllowedIPs: []string{"10.99.0.2/32", "fd00:99::2/128"}, PersistentKeepalive: 25}}}}}
}

func TestWireGuardValidation(t *testing.T) {
	d := fixtureWireGuardDesired(t)
	if err := Validate(d); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Desired)
	}{
		{"unowned", func(d *Desired) { d.Links = nil }},
		{"inline-or-invalid-key", func(d *Desired) { d.WireGuard[0].Peers[0].PublicKey = "secret-invalid-data" }},
		{"zero-public-key", func(d *Desired) { d.WireGuard[0].Peers[0].PublicKey = (wireGuardKey{}).encoded() }},
		{"hostname", func(d *Desired) { d.WireGuard[0].Peers[0].Endpoint = "vpn.example:51820" }},
		{"zone-required", func(d *Desired) { d.WireGuard[0].Peers[0].Endpoint = "[fe80::2]:51820" }},
		{"same-prefix", func(d *Desired) {
			peer := d.WireGuard[0].Peers[0]
			peer.PublicKey = fixtureWireGuardPublic(83)
			d.WireGuard[0].Peers = append(d.WireGuard[0].Peers, peer)
		}},
		{"same-key", func(d *Desired) { d.WireGuard[0].Peers = append(d.WireGuard[0].Peers, d.WireGuard[0].Peers[0]) }},
		{"host-bits", func(d *Desired) { d.WireGuard[0].Peers[0].AllowedIPs = []string{"10.99.0.2/24"} }},
		{"keepalive", func(d *Desired) { d.WireGuard[0].Peers[0].PersistentKeepalive = 65536 }},
		{"relative-secret", func(d *Desired) { d.WireGuard[0].PrivateKeyFile = "key" }},
		{"missing-config-on-up", func(d *Desired) { d.WireGuard = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := cloneDesired(d)
			tc.mutate(&copy)
			err := Validate(copy)
			if err == nil || strings.Contains(err.Error(), "secret-invalid-data") {
				t.Fatalf("invalid config was accepted or key data leaked: %v", err)
			}
		})
	}
	// Nested prefixes have an intentional longest-prefix interpretation.
	d.WireGuard[0].Peers = append(d.WireGuard[0].Peers, WireGuardPeer{PublicKey: fixtureWireGuardPublic(83), AllowedIPs: []string{"10.99.0.0/24"}})
	if err := Validate(d); err != nil {
		t.Fatal("nested distinct prefixes should be valid:", err)
	}
}

func TestWireGuardOwnershipSecretsRoamingAndRollback(t *testing.T) {
	ctx := context.Background()
	f := fakeWireGuard()
	path := filepath.Join(t.TempDir(), "network.json")
	e, err := newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	d := fixtureWireGuardDesired(t)
	snapshot, err := e.Apply(ctx, 1, d)
	if err != nil {
		t.Fatal(err)
	}
	index := f.links["wg0"].Index
	if !f.links["wg0"].Up || len(snapshot.WireGuard) != 1 || !snapshot.WireGuard[0].Owned || snapshot.WireGuard[0].ListenPort != 50123 {
		t.Fatalf("missing initial owned state: %+v", snapshot.WireGuard)
	}
	key, psk := fixtureWireGuardKey(31).encoded(), fixtureWireGuardKey(73).encoded()
	publicJSON, _ := json.Marshal(snapshot)
	journal, _ := os.ReadFile(path)
	for _, data := range [][]byte{publicJSON, journal, []byte(fmt.Sprintf("%+v %#v", f.devices[index], f.devices[index]))} {
		if strings.Contains(string(data), key) || strings.Contains(string(data), psk) {
			t.Fatal("secret leaked outside private store")
		}
	}
	// A changed/missing original source never changes a bound key on restart.
	if err := os.WriteFile(d.WireGuard[0].PrivateKeyFile, []byte("not the old key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d.WireGuard[0].Peers[0].PresharedKeyFile); err != nil {
		t.Fatal(err)
	}
	f.writes = nil
	if _, err := e.Apply(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("idempotent apply changed kernel: %v", f.writes)
	}
	device := f.devices[index]
	device.Peers[0].Endpoint = "192.0.2.77:60000"
	f.devices[index] = device
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	changed := cloneDesired(d)
	changed.WireGuard[0].Peers[0].PersistentKeepalive = 30
	if _, err := e.Apply(ctx, 2, changed); err != nil {
		t.Fatal(err)
	}
	if f.devices[index].Peers[0].Endpoint != "192.0.2.77:60000" {
		t.Fatal("declaratively unchanged endpoint overwrote roaming")
	}
	if f.devices[index].ListenPort != 50123 {
		t.Fatal("random listen port changed during update")
	}
	if !reflect.DeepEqual(f.writes[len(f.writes)-3:], []string{"wireguard-up-false", "wireguard-replace", "wireguard-up-true"}) {
		t.Fatalf("replacement did not stay down until complete readback: %v", f.writes)
	}
	// Versioned key paths are explicit rotation; rollback uses the old immutable
	// copy even though its source file is no longer usable.
	newKey := fixtureWireGuardKey(32)
	newPath := filepath.Join(t.TempDir(), "private-v2")
	if err := os.WriteFile(newPath, []byte(newKey.encoded()), 0600); err != nil {
		t.Fatal(err)
	}
	rotated := cloneDesired(changed)
	rotated.WireGuard[0].PrivateKeyFile = newPath
	if _, err := e.Apply(ctx, 3, rotated); err != nil {
		t.Fatal(err)
	}
	if !f.devices[index].PrivateKey.equal(newKey) {
		t.Fatal("explicit rotation did not install new key")
	}
	if _, err := e.Apply(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	if !f.devices[index].PrivateKey.equal(fixtureWireGuardKey(31)) {
		t.Fatal("rollback did not restore immutable old key")
	}
	// Static -> passive clears the old endpoint, rather than retaining it.
	passive := cloneDesired(d)
	passive.WireGuard[0].Peers[0].Endpoint = ""
	if _, err := e.Apply(ctx, 4, passive); err != nil {
		t.Fatal(err)
	}
	if f.devices[index].Peers[0].Endpoint != "" {
		t.Fatal("explicit passive endpoint did not clear bootstrap endpoint")
	}
	if _, err := e.Apply(ctx, 5, Desired{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := f.links["wg0"]; exists || len(e.state.WireGuard) != 0 {
		t.Fatal("removed WireGuard device retained ownership")
	}
}

func TestWireGuardKernelRAWritersStopBeforeFirstUp(t *testing.T) {
	f := fakeWireGuard()
	e, err := newEngine(Options{StatePath: filepath.Join(t.TempDir(), "network.json")}, f)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	d := fixtureWireGuardDesired(t)
	zero, disabled := 0, false
	d.Interfaces[0].IPv6AcceptRA, d.Interfaces[0].IPv6Autoconf = &zero, &disabled
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	positions := map[string]int{}
	for i, write := range f.writes {
		positions[write] = i
	}
	up, hasUp := positions["wireguard-up-true"]
	for _, kind := range []string{"setting-accept_ra", "setting-autoconf"} {
		position, exists := positions[kind]
		if !hasUp || !exists || position >= up {
			t.Fatalf("kernel RA/SLAAC writer remained active at first UP: %v", f.writes)
		}
	}
}

func TestWireGuardInterruptedPeerSetRecoveryAndForeignProtection(t *testing.T) {
	ctx := context.Background()
	f := fakeWireGuard()
	path := filepath.Join(t.TempDir(), "network.json")
	e, err := newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	d := fixtureWireGuardDesired(t)
	if _, err := e.Apply(ctx, 1, d); err != nil {
		t.Fatal(err)
	}
	index := f.links["wg0"].Index
	changed := cloneDesired(d)
	changed.WireGuard[0].Peers = append(changed.WireGuard[0].Peers, WireGuardPeer{PublicKey: fixtureWireGuardPublic(85), AllowedIPs: []string{"10.99.0.3/32"}})
	f.failAfterPeer = 1
	if _, err := e.Apply(ctx, 2, changed); err == nil {
		t.Fatal("partial replacement was reported as success")
	}
	if f.links["wg0"].Up || !e.wireGuardRecord("wg0").Quarantined {
		t.Fatal("partial replacement was not held down")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = newEngine(Options{StatePath: path}, f)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.failAfterPeer = -1
	if _, err := e.Apply(ctx, 1, d); err != nil {
		t.Fatal("interrupted rollback:", err)
	}
	if !f.links["wg0"].Up || len(f.devices[index].Peers) != 1 || e.wireGuardRecord("wg0").Quarantined {
		t.Fatal("rollback did not reopen the complete old peer set")
	}
	// A foreign peer must be preserved and must not authorize a DOWN transition.
	device := f.devices[index]
	public, _ := fixtureWireGuardKey(90).public()
	device.Peers = append(device.Peers, wireGuardNativePeer{PublicKey: public, AllowedIPs: []string{"198.51.100.0/24"}})
	f.devices[index] = device
	f.writes = nil
	if _, err := e.Apply(ctx, 3, d); err == nil {
		t.Fatal("foreign peer was silently adopted")
	}
	if len(f.writes) != 0 || !f.links["wg0"].Up || len(f.devices[index].Peers) != 2 {
		t.Fatal("foreign WireGuard state was changed")
	}
	// A name collision also prevents all writes, including key generation.
	foreign := fakeWireGuard()
	foreign.links["wg0"] = InterfaceState{Name: "wg0", Index: 42, Kind: "wireguard", Up: true}
	other, err := newEngine(Options{StatePath: filepath.Join(t.TempDir(), "journal")}, foreign)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Apply(ctx, 1, d); err == nil {
		t.Fatal("foreign link was adopted")
	}
	if len(foreign.writes) != 0 || !foreign.links["wg0"].Up {
		t.Fatal("foreign link was touched")
	}
}

func TestWireGuardSecretFilesAndGeneration(t *testing.T) {
	d := fixtureWireGuardDesired(t)
	file := d.WireGuard[0].PrivateKeyFile
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardSource(file, "private"); err == nil {
		t.Fatal("world-readable secret was accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "secret-link")
	if err := os.Symlink(file, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readWireGuardSource(symlink, "private"); err == nil {
		t.Fatal("symlink secret was accepted")
	}
	generated := filepath.Join(t.TempDir(), "private")
	if err := generateWireGuardSource(generated); err != nil {
		t.Fatal(err)
	}
	first, err := readWireGuardSource(generated, "private")
	if err != nil || first.zero() {
		t.Fatal("generated key could not be read", err)
	}
	if err := generateWireGuardSource(generated); err != nil {
		t.Fatal(err)
	}
	second, err := readWireGuardSource(generated, "private")
	if err != nil || !first.equal(second) {
		t.Fatal("generation replaced an existing key")
	}
}
