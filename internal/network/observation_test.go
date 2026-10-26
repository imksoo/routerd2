//go:build linux

package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/lifetime"
)

func observationFixture(t *testing.T, k kernel) *Engine {
	t.Helper()
	e, err := newEngine(Options{StatePath: filepath.Join(t.TempDir(), "network.json")}, k)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	e.state.Revision, e.state.AttemptedRevision, e.state.Status = 7, 7, "applied"
	return e
}

func encodedObservation(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil || len(b)+1 > ObservationResponseLimit {
		t.Fatalf("unbounded response: bytes=%d error=%v", len(b), err)
	}
	return b
}

func TestObservationSmallSnapshotRemainsComplete(t *testing.T) {
	s := Snapshot{Revision: 3, AttemptedRevision: 3, Status: "applied", Interfaces: []InterfaceState{{Name: "wan0", Index: 9, Up: true}}, Routes: []RouteState{{Route: Route{Destination: "192.0.2.0/24", Interface: "wan0", Table: 254}}}}
	v, err := BoundSnapshot(s)
	if err != nil || !v.Complete || v.Truncated || !reflect.DeepEqual(s, v.Snapshot) || !validObservationID(v.ProjectionID) || v.ObservedAt.IsZero() {
		t.Fatalf("small snapshot contract: %+v %v", v, err)
	}
	encodedObservation(t, v)
	s.ObservationErrors = []string{"interface vanished during observation"}
	v, err = BoundSnapshot(s)
	if err != nil || v.Complete || v.Truncated || v.Counts.ObservationErrors != 1 {
		t.Fatal("observation failure was reported as complete", v, err)
	}
}

func TestObservationLargeSnapshotCountsAndEscapedByteBudget(t *testing.T) {
	s := Snapshot{Revision: 9, AttemptedRevision: 9, Status: "applied", LastError: strings.Repeat("<日本語>", 2000)}
	for i := range 16000 {
		r := Route{Destination: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Interface: "wan0", Table: 254}
		s.Desired.Routes = append(s.Desired.Routes, r)
		s.Routes = append(s.Routes, RouteState{Route: r, Protocol: 99, Type: 1})
	}
	l := InterfaceState{Name: "wan0", Index: 9}
	for i := range 300 {
		l.Addresses = append(l.Addresses, AddressState{Address: fmt.Sprintf("2001:db8::%x/64", i+1)})
	}
	s.Interfaces = []InterfaceState{l}
	d := WireGuardState{Interface: "wg0"}
	for i := range 300 {
		p := WireGuardPeerState{PublicKey: fmt.Sprintf("peer-%d", i)}
		for j := range 100 {
			p.AllowedIPs = append(p.AllowedIPs, fmt.Sprintf("2001:db8:%x:%x::/64", i, j))
		}
		d.Peers = append(d.Peers, p)
	}
	s.WireGuard = []WireGuardState{d}
	s.ObservationErrors = []string{strings.Repeat("\x00<&", 5000)}
	before, _ := json.Marshal(s)
	v, err := BoundSnapshot(s)
	if err != nil {
		t.Fatal(err)
	}
	if v.Complete || !v.Truncated || !v.Truncation.Desired || !v.Truncation.Routes || !v.Truncation.Addresses || !v.Truncation.WireGuardPeers || !v.Truncation.WireGuardAllowedIPs || !v.Truncation.LastError || !v.Truncation.ObservationErrors {
		t.Fatal("omissions lack explicit markers", v.Truncation)
	}
	if v.Counts.Routes != 16000 || v.Counts.Addresses != 300 || v.Counts.WireGuardPeers != 300 || v.Counts.WireGuardAllowedIPs != 30000 || v.Counts.ObservationErrors != 1 {
		t.Fatal("summary lost exact counts", v.Counts)
	}
	encodedObservation(t, v)
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) {
		t.Fatal("summary modified the underlying observed data")
	}
	// A very large escaped attribute cannot defeat the byte cap even if the
	// number of observed objects is one. All dropped sections remain explicit.
	s = Snapshot{Interfaces: []InterfaceState{{Name: "wan0", Alias: strings.Repeat("\x00", ObservationResponseLimit)}}}
	v, err = BoundSnapshot(s)
	if err != nil || !v.Truncated || !v.Truncation.Interfaces || v.Counts.Interfaces != 1 || len(v.Interfaces) != 0 {
		t.Fatal("single oversized attribute escaped the fallback", err)
	}
	encodedObservation(t, v)
}

func TestObservationPageUsesEncodedByteBudget(t *testing.T) {
	h := ObservationHeader{Status: "applied", ProjectionID: strings.Repeat("a", 64), ObservedAt: time.Now()}
	q := ObservationQuery{Section: "addresses", Interface: "wan0", Limit: 256}
	page, err := makeObservationPage(h, q, 20, func(int) (any, error) { return strings.Repeat("\x00", 20000), nil })
	if err != nil || len(page.Items) != 2 || page.NextOffset == nil || *page.NextOffset != 2 || page.Total != 20 {
		t.Fatal("escaped item budget was ignored", page, err)
	}
	encodedObservation(t, page)
	_, err = makeObservationPage(h, q, 1, func(int) (any, error) { return strings.Repeat("\x00", ObservationResponseLimit), nil })
	if !errors.Is(err, ErrObservationTooLarge) {
		t.Fatal("oversized single item was silently dropped", err)
	}
}

func TestObservationNativeCollectionsAndProjectionFence(t *testing.T) {
	ctx := context.Background()
	f := fake()
	e := observationFixture(t, f)
	e.state.Desired.Interfaces = []Interface{{Name: "wan0"}}
	e.state.Desired.Routes = []Route{{Destination: "0.0.0.0/0", Interface: "wan0", Table: 254}}
	l := f.links["wan0"]
	for i := range 300 {
		l.Addresses = append(l.Addresses, AddressState{Address: fmt.Sprintf("2001:db8::%x/64", i+1), PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)})
	}
	f.links["wan0"] = l
	e.state.Addresses = []ownedAddress{{Interface: "wan0", Address: l.Addresses[0].Address, Index: l.Index}}
	for i := range 300 {
		f.routes = append(f.routes, RouteState{Route: Route{Destination: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Interface: "wan0", Table: 254}, Protocol: RouteProtocol, Type: 1})
	}
	hops := []NextHop{}
	for i := range 300 {
		hops = append(hops, NextHop{Interface: fmt.Sprintf("n%d", i), Weight: 1})
	}
	f.routes[0].Interface, f.routes[0].NextHops = "", hops
	f.routes[0].Multipath = true
	e.state.Routes = []ownedRoute{{Route: f.routes[1].Route}}
	first, err := e.ObservationPage(ctx, ObservationQuery{Section: "interfaces"})
	if err != nil || len(first.Items) != 1 {
		t.Fatal(first, err)
	}
	var iface InterfaceObservation
	if err := json.Unmarshal(first.Items[0], &iface); err != nil || iface.AddressCount != 300 || len(iface.Addresses) != 8 || !iface.AddressesTruncated {
		t.Fatal("address preview metadata", iface, err)
	}
	var allAddresses []AddressState
	for offset := 0; ; {
		page, err := e.ObservationPage(ctx, ObservationQuery{Section: "addresses", Interface: "wan0", Offset: offset, Limit: 73, ProjectionID: first.ProjectionID})
		if err != nil || page.Total != 300 {
			t.Fatal(page, err)
		}
		encodedObservation(t, page)
		for _, item := range page.Items {
			var a AddressState
			if err := json.Unmarshal(item, &a); err != nil {
				t.Fatal(err)
			}
			allAddresses = append(allAddresses, a)
		}
		if page.NextOffset == nil {
			break
		}
		if *page.NextOffset <= offset {
			t.Fatal("pagination did not advance")
		}
		offset = *page.NextOffset
	}
	if len(allAddresses) != 300 {
		t.Fatal("address pagination lost entries", len(allAddresses))
	}
	owned := 0
	for _, a := range allAddresses {
		if a.Owned {
			owned++
		}
	}
	if owned != 1 {
		t.Fatal("address ownership did not survive pagination", owned)
	}
	seen := map[string]bool{}
	var multipath RouteObservation
	owned = 0
	for offset := 0; ; {
		page, err := e.ObservationPage(ctx, ObservationQuery{Section: "routes", Offset: offset, Limit: 47, ProjectionID: first.ProjectionID})
		if err != nil || page.Total != 300 {
			t.Fatal(page, err)
		}
		encodedObservation(t, page)
		for _, item := range page.Items {
			var route RouteObservation
			if err := json.Unmarshal(item, &route); err != nil {
				t.Fatal(err)
			}
			if seen[route.RouteID] {
				t.Fatal("duplicate stable route identifier")
			}
			seen[route.RouteID] = true
			if route.Owned {
				owned++
			}
			if route.NextHopCount > 0 {
				multipath = route
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if len(seen) != 300 || owned != 1 || multipath.NextHopCount != 300 || !multipath.NextHopsTruncated || len(multipath.NextHops) != 8 {
		t.Fatal("route pagination or nested metadata lost state", len(seen), owned, multipath.NextHopCount)
	}
	var allHops []NextHop
	for offset := 0; ; {
		page, err := e.ObservationPage(ctx, ObservationQuery{Section: "route-next-hops", RouteID: multipath.RouteID, Offset: offset, Limit: 79})
		if err != nil || page.Total != 300 {
			t.Fatal(page, err)
		}
		for _, item := range page.Items {
			var hop NextHop
			if err := json.Unmarshal(item, &hop); err != nil {
				t.Fatal(err)
			}
			allHops = append(allHops, hop)
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if len(allHops) != 300 {
		t.Fatal("nested route details were lost")
	}
	// An observed kernel change need not change projection_id. Pages are fresh
	// observations; they deliberately promise no atomic cross-page snapshot.
	l.Addresses[0].Flags = 0x40
	f.links["wan0"] = l
	changed, err := e.ObservationPage(ctx, ObservationQuery{Section: "addresses", Interface: "wan0", ProjectionID: first.ProjectionID})
	if err != nil || changed.ProjectionID != first.ProjectionID || changed.ObservedAt.Before(first.ObservedAt) {
		t.Fatal("kernel-only observation was falsely revision-fenced", err)
	}
	pending := cloneDesired(e.state.Desired)
	pending.Routes[0].Gateway = "192.0.2.9"
	e.state.Attempted = &pending
	if _, err := e.ObservationPage(ctx, ObservationQuery{Section: "routes", ProjectionID: first.ProjectionID}); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("pending intent change was not fenced", err)
	}
	if len(f.writes) != 0 {
		t.Fatal("observation mutated the kernel", f.writes)
	}
}

func TestObservationWireGuardNestedPagesAndSecretRedaction(t *testing.T) {
	ctx := context.Background()
	f := fakeWireGuard()
	e := observationFixture(t, f)
	d := fixtureWireGuardDesired(t)
	if _, err := e.Apply(ctx, 8, d); err != nil {
		t.Fatal(err)
	}
	index := f.links["wg0"].Index
	device := f.devices[index]
	device.Peers = nil
	for i := range 300 {
		var key wireGuardKey
		binary.LittleEndian.PutUint64(key[:], uint64(i+1))
		p := wireGuardNativePeer{PublicKey: key, PresharedKey: fixtureWireGuardKey(111), Endpoint: "192.0.2.7:51820", ReceivedBytes: uint64(i)}
		for j := range 300 {
			p.AllowedIPs = append(p.AllowedIPs, fmt.Sprintf("2001:db8:%x:%x::/64", i, j))
		}
		device.Peers = append(device.Peers, p)
	}
	f.devices[index] = device
	f.writes = nil
	devices, err := e.ObservationPage(ctx, ObservationQuery{Section: "wireguard"})
	if err != nil || devices.Total != 1 {
		t.Fatal(devices, err)
	}
	var public WireGuardObservation
	if err := json.Unmarshal(devices.Items[0], &public); err != nil || public.PeerCount != 300 || !public.PeersTruncated || len(public.Peers) != 0 {
		t.Fatal(public, err)
	}
	seen := map[string]bool{}
	firstKey := ""
	for offset := 0; ; {
		page, err := e.ObservationPage(ctx, ObservationQuery{Section: "wireguard-peers", Interface: "wg0", Offset: offset, Limit: 83, ProjectionID: devices.ProjectionID})
		if err != nil || page.Total != 300 {
			t.Fatal(page, err)
		}
		b := encodedObservation(t, page)
		for _, secret := range []string{device.PrivateKey.encoded(), device.Peers[0].PresharedKey.encoded()} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatal("secret leaked into paged observation")
			}
		}
		for _, item := range page.Items {
			var p WireGuardPeerObservation
			if err := json.Unmarshal(item, &p); err != nil {
				t.Fatal(err)
			}
			if seen[p.PublicKey] || p.AllowedIPCount != 300 || !p.AllowedIPsTruncated || len(p.AllowedIPs) != 8 {
				t.Fatal("peer detail was lost", p)
			}
			seen[p.PublicKey] = true
			if firstKey == "" {
				firstKey = p.PublicKey
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if len(seen) != 300 {
		t.Fatal("peer page traversal lost peers")
	}
	count := 0
	for offset := 0; ; {
		page, err := e.ObservationPage(ctx, ObservationQuery{Section: "wireguard-allowed-ips", Interface: "wg0", PublicKey: firstKey, Offset: offset, Limit: 113})
		if err != nil || page.Total != 300 {
			t.Fatal(page, err)
		}
		count += len(page.Items)
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if count != 300 || len(f.writes) != 0 {
		t.Fatal("allowed IP page traversal lost entries or mutated kernel", count, f.writes)
	}
	device.Peers[0].ReceivedBytes++
	device.Peers[0].Endpoint = "192.0.2.99:51820"
	f.devices[index] = device
	if _, err := e.ObservationPage(ctx, ObservationQuery{Section: "wireguard-peers", Interface: "wg0", ProjectionID: devices.ProjectionID}); err != nil {
		t.Fatal("roaming/counters incorrectly changed the projection fence", err)
	}
}

type sourceOnlyKernel struct {
	*fakeKernel
	reads int
}

func (k *sourceOnlyKernel) Interface(name string) (InterfaceState, error) {
	if name != "wan0" && name != "missing" {
		panic("source query touched an unrelated interface")
	}
	k.reads++
	return k.fakeKernel.Interface(name)
}
func (*sourceOnlyKernel) Routes(int) ([]RouteState, error) { panic("source query read routes") }
func (*sourceOnlyKernel) PolicyRules() ([]PolicyRuleState, error) {
	panic("source query read policy rules")
}
func (*sourceOnlyKernel) ReadSetting(settingKey) (int, error) {
	panic("source query read global settings")
}
func (*sourceOnlyKernel) Firewall() (FirewallState, error)  { panic("source query read firewall") }
func (*sourceOnlyKernel) VRRPGuard() (FirewallState, error) { panic("source query read VRRP") }

func TestObservationSourceIndependentOfFullStateAndOwnership(t *testing.T) {
	k := &sourceOnlyKernel{fakeKernel: fake()}
	e := observationFixture(t, k)
	for i := range 16000 {
		e.state.Desired.Routes = append(e.state.Desired.Routes, Route{Destination: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Interface: "other0"})
	}
	l := k.links["wan0"]
	l.Up, l.Addresses = true, []AddressState{{Address: "2001:db8::1/64", PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}
	k.links["wan0"] = l
	e.state.Addresses = []ownedAddress{{Interface: "wan0", Address: l.Addresses[0].Address, Index: l.Index}}
	value, err := e.SourceAddress(context.Background(), "wan0", "2001:db8::1")
	if err != nil || !value.Found || !value.InterfacePresent || !value.Up || value.Address == nil || !value.Address.Owned || value.ProjectionID != "" || k.reads != 1 {
		t.Fatal("minimal source observation failed", value, err, k.reads)
	}
	if len(encodedObservation(t, value)) > 1024 {
		t.Fatal("one source query grew with unrelated desired state")
	}
	e.state.Addresses[0].Index++
	value, err = e.SourceAddress(context.Background(), "wan0", "2001:db8::1")
	if err != nil || value.Address == nil || value.Address.Owned {
		t.Fatal("recreated interface inherited address ownership", value, err)
	}
	e.state.Addresses[0].Index = l.Index
	e.state.Addresses[0].Options.Peer = "2001:db8::2/128"
	value, err = e.SourceAddress(context.Background(), "wan0", "2001:db8::1")
	if err != nil || value.Address.Owned {
		t.Fatal("mismatched peer retained address ownership", value, err)
	}
	value, err = e.SourceAddress(context.Background(), "missing", "2001:db8::1")
	if err != nil || value.InterfacePresent || value.Found || value.Address != nil {
		t.Fatal("missing interface was mistaken for installed source", value, err)
	}
	before := k.reads
	if _, err := e.SourceAddress(context.Background(), "bad/name", "2001:db8::1"); err == nil || k.reads != before {
		t.Fatal("invalid source request reached kernel")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.SourceAddress(ctx, "wan0", "2001:db8::1"); !errors.Is(err, context.Canceled) || k.reads != before {
		t.Fatal("canceled source request reached kernel", err)
	}
	if len(k.writes) != 0 {
		t.Fatal("source observation mutated kernel")
	}
}

func TestObservationDesiredChunksAndFence(t *testing.T) {
	e := observationFixture(t, fake())
	for i := range 5000 {
		e.state.Desired.Routes = append(e.state.Desired.Routes, Route{Destination: fmt.Sprintf("10.%d.%d.0/24", i/256, i%256), Interface: "wan0"})
	}
	want, _ := json.Marshal(e.state.Desired)
	var got []byte
	projection := ""
	for offset := 0; ; {
		page, err := e.DesiredObservation(context.Background(), projection, offset, 10001)
		if err != nil || page.TotalBytes != len(want) || page.Offset != offset || len(page.Data) > 10001 {
			t.Fatal(page, err)
		}
		encodedObservation(t, page)
		if projection == "" {
			projection = page.ProjectionID
		}
		got = append(got, page.Data...)
		if page.NextOffset == nil {
			break
		}
		if *page.NextOffset != len(got) {
			t.Fatal("chunk gap")
		}
		offset = *page.NextOffset
	}
	if !bytes.Equal(got, want) {
		t.Fatal("complete desired projection could not be reconstructed")
	}
	e.state.Desired.Routes[0].Metric++
	if _, err := e.DesiredObservation(context.Background(), projection, 0, 0); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("desired change was not fenced", err)
	}
	if _, err := e.DesiredObservation(context.Background(), "", 0, DesiredObservationChunk+1); err == nil {
		t.Fatal("oversized desired chunk accepted")
	}
	for len(want) <= MaxDesiredObservationBytes {
		e.state.Desired.Routes = append(e.state.Desired.Routes, e.state.Desired.Routes...)
		want, _ = json.Marshal(e.state.Desired)
	}
	if _, err := e.DesiredObservation(context.Background(), "", 0, 0); !errors.Is(err, ErrObservationTooLarge) {
		t.Fatal("oversized desired projection was silently incomplete", err)
	}
}

func TestObservationIndexedOwnershipMatchesAuthoritativeSnapshot(t *testing.T) {
	f := fake()
	e := observationFixture(t, f)
	e.state.Desired = Desired{Interfaces: []Interface{{Name: "wan0"}}, Routes: []Route{{Destination: "2001:db8::/64", Interface: "wan0", Table: 254}}}
	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	definitions := []Route{
		{Destination: "2001:db8::/64", Interface: "wan0"},
		{Destination: "2001:db8:2::/64", Gateway: "fe80::2", Interface: "wan0", Table: 123, Metric: 7, ValidUntil: lifetime.Deadline{UTC: expiry}},
		{Destination: "198.51.100.0/24", Table: 254, NextHops: []NextHop{{Interface: "lan0", Gateway: "192.0.2.2", Weight: 2}, {Interface: "wan0", Gateway: "192.0.2.1", Weight: 1}}},
	}
	for _, definition := range definitions {
		saved := testOwnedRoute(t, definition)
		e.state.Routes = append(e.state.Routes, saved)
		f.routes = append(f.routes, RouteState{Route: normalizeRoute(definition), kernelExpiry: saved.KernelExpiry, Protocol: RouteProtocol, Type: 1, Multipath: len(definition.NextHops) > 0})
	}
	index := e.observationRouteOwnershipLocked()
	variants := []func(*RouteState){
		func(*RouteState) {},
		func(r *RouteState) { r.Protocol++ },
		func(r *RouteState) { r.Type++ },
		func(r *RouteState) { r.Advanced = true },
		func(r *RouteState) { r.Source = "192.0.2.99" },
		func(r *RouteState) { r.Interface = "foreign0" },
		func(r *RouteState) { r.Table++ },
		func(r *RouteState) { r.Metric++ },
		func(r *RouteState) { r.Gateway = "192.0.2.222" },
		func(r *RouteState) { r.kernelExpiry.Nanos += uint64(time.Second) },
		func(r *RouteState) { r.kernelExpiry.Nanos += uint64(3 * time.Second) },
		func(r *RouteState) { r.kernelExpiry = routeTimer{} },
		func(r *RouteState) { r.Multipath = !r.Multipath },
		func(r *RouteState) {
			if len(r.NextHops) > 1 {
				r.NextHops[0], r.NextHops[1] = r.NextHops[1], r.NextHops[0]
			}
		},
		func(r *RouteState) {
			if len(r.NextHops) > 0 {
				r.NextHops[0].Weight++
			}
		},
	}
	for _, route := range f.routes {
		for i, variant := range variants {
			copy := route
			copy.NextHops = append([]NextHop(nil), route.NextHops...)
			variant(&copy)
			if got, want := index(copy), e.ownsRoute(copy); got != want {
				t.Fatalf("ownership candidate index changed exact semantics: variant=%d got=%t want=%t route=%+v", i, got, want, copy)
			}
		}
	}
	l := f.links["wan0"]
	l.Addresses = []AddressState{{Address: "2001:db8::1/64", PreferredLifetime: ^uint32(0), ValidLifetime: ^uint32(0)}}
	f.links["wan0"] = l
	e.state.Addresses = []ownedAddress{{Interface: "wan0", Address: l.Addresses[0].Address, Index: l.Index}}
	legacy, err := e.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view, err := e.Observe(context.Background())
	if err != nil || view.Truncated || !view.Complete || !reflect.DeepEqual(view.Snapshot, legacy) {
		t.Fatal("new native observation disagrees with authoritative legacy fields", view.Snapshot, legacy, err)
	}
}

func TestObservationLargeManagedFIBKeepsSummaryAndTailPageUsable(t *testing.T) {
	f := fake()
	e := observationFixture(t, f)
	e.state.Desired.Interfaces = []Interface{{Name: "wan0"}}
	for i := range 16000 {
		route := Route{Destination: fmt.Sprintf("10.%d.%d.%d/32", i>>16, i>>8&255, i&255), Interface: "wan0", Table: 254}
		e.state.Desired.Routes = append(e.state.Desired.Routes, route)
		e.state.Routes = append(e.state.Routes, ownedRoute{Route: route})
		f.routes = append(f.routes, RouteState{Route: route, Protocol: RouteProtocol, Type: 1})
	}
	view, err := e.Observe(context.Background())
	if err != nil || !view.Truncated || view.Counts.Routes != 16000 {
		t.Fatal("large managed FIB cannot be summarized", view.Counts, err)
	}
	encodedObservation(t, view)
	for _, route := range view.Routes {
		if !route.Owned {
			t.Fatal("indexed summary lost managed ownership")
		}
	}
	page, err := e.ObservationPage(context.Background(), ObservationQuery{Section: "routes", Offset: 15000, Limit: 256, ProjectionID: view.ProjectionID})
	if err != nil || page.Total != 16000 || len(page.Items) != 256 {
		t.Fatal("large managed FIB tail page unavailable", page.Total, len(page.Items), err)
	}
	encodedObservation(t, page)
	for _, item := range page.Items {
		var route RouteObservation
		if err := json.Unmarshal(item, &route); err != nil || !route.Owned {
			t.Fatal("tail page lost owned entries", err)
		}
	}
	if len(f.writes) != 0 {
		t.Fatal("large FIB observation mutated kernel")
	}
}
