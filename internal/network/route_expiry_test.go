//go:build linux

package network

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestFiniteRouteRenewalRestartAndForeignDeadline(t *testing.T) {
	k := fake()
	e, opts := engineFor(t, k)
	r := Route{Destination: "2001:db8:3::/64", Gateway: "fe80::2", Interface: "wan0", ValidUntil: time.Now().Add(time.Minute)}
	d := Desired{Routes: []Route{r}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err := newEngine(opts, k)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	k.routes[0].ValidUntil = k.routes[0].ValidUntil.Add(700 * time.Millisecond)
	k.writes = nil
	if _, err := e.Apply(context.Background(), 1, d); err != nil || len(k.writes) != 0 {
		t.Fatalf("rounded lifetime lost ownership or caused a write: %v %v", err, k.writes)
	}
	d.Routes[0].ValidUntil = r.ValidUntil.Add(time.Minute)
	if _, err := e.Apply(context.Background(), 2, d); err != nil {
		t.Fatal(err)
	}
	if len(k.routes) != 1 || !e.ownsRoute(k.routes[0]) || !k.routes[0].ValidUntil.Equal(d.Routes[0].ValidUntil) {
		t.Fatal("owned deadline renewal was not committed")
	}
	k.routes[0].ValidUntil = time.Time{} // Another owner replaced it with static.
	k.writes = nil
	if _, err := e.Apply(context.Background(), 2, d); err == nil || len(k.writes) != 0 || e.ownsRoute(k.routes[0]) {
		t.Fatalf("finite journal adopted or modified static replacement: %v %v", err, k.writes)
	}
	if _, err := e.Apply(context.Background(), 3, Desired{}); err != nil || len(k.routes) != 1 {
		t.Fatalf("foreign static replacement not preserved on removal: %v", err)
	}
}

func TestRouteExpiryValidationAndProjection(t *testing.T) {
	r := Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: time.Now().Add(time.Minute)}
	for _, edit := range []func(*Route){
		func(r *Route) { r.Destination = "192.0.2.0/24" },
		func(r *Route) { r.ValidUntil = time.Unix(-1, 0) },
		func(r *Route) { r.ValidUntil = time.Now().Add(MaxRouteLifetime + time.Hour) },
	} {
		bad := r
		edit(&bad)
		if err := Validate(Desired{Routes: []Route{bad}}); err == nil {
			t.Fatalf("accepted invalid finite route %+v", bad)
		}
	}
	k := fake()
	e, _ := engineFor(t, k)
	r.ValidUntil = time.Now().Add(-time.Second)
	d := Desired{Routes: []Route{r}}
	if _, err := e.Apply(context.Background(), 1, d); err != nil || len(k.routes) != 0 || len(e.state.Desired.Routes) != 0 {
		t.Fatalf("expired candidate installed or retained a route: %v", err)
	}
	if len(d.Routes) != 1 {
		t.Fatal("projection mutated the caller")
	}
}

func TestNativeRouteDecoderRetainsExpiryAndUnknownAttributes(t *testing.T) {
	now := time.Now()
	header := &nl.RtMsg{RtMsg: unix.RtMsg{Family: unix.AF_INET6, Dst_len: 64, Table: 254, Protocol: RouteProtocol, Type: unix.RTN_UNICAST}}
	cache := make([]byte, 32)
	nl.NativeEndian().PutUint32(cache[8:12], 3025) // 30.25 seconds, not 3025 seconds.
	raw := append([]byte(nil), header.Serialize()...)
	for _, attr := range []*nl.RtAttr{
		nl.NewRtAttr(unix.RTA_DST, net.ParseIP("2001:db8:3::").To16()),
		nl.NewRtAttr(unix.RTA_GATEWAY, net.ParseIP("fe80::2").To16()),
		nl.NewRtAttr(unix.RTA_OIF, nl.Uint32Attr(2)),
		nl.NewRtAttr(unix.RTA_PRIORITY, nl.Uint32Attr(1024)),
		nl.NewRtAttr(unix.RTA_CACHEINFO, cache),
		nl.NewRtAttr(unix.RTA_PREF, []byte{0}),
	} {
		raw = append(raw, attr.Serialize()...)
	}
	want := Route{Destination: "2001:db8:3::/64", Gateway: "fe80::2", Interface: "wan0", ValidUntil: now.Add(30 * time.Second)}
	observed, err := decodeNativeRoute(raw, map[int]string{2: "wan0"}, now)
	if err != nil || !routeMatches(observed, want) || !observed.ValidUntil.Equal(now.Add(30250*time.Millisecond)) {
		t.Fatalf("finite route decoded incorrectly: %+v %v", observed, err)
	}
	static := want
	static.ValidUntil = time.Time{}
	if routeMatches(observed, static) {
		t.Fatal("finite kernel route matched a static journal entry")
	}
	raw = append(raw, nl.NewRtAttr(unix.RTA_MARK, nl.Uint32Attr(7)).Serialize()...)
	observed, err = decodeNativeRoute(raw, map[int]string{2: "wan0"}, now)
	if err != nil || !observed.Advanced || routeMatches(observed, want) {
		t.Fatal("unmodeled route attribute was silently discarded")
	}
	for _, malformed := range [][]byte{nil, raw[:8], append(header.Serialize(), []byte{2, 0, 1, 0}...)} {
		if _, err := decodeNativeRoute(malformed, nil, now); err == nil {
			t.Fatal("accepted truncated route message")
		}
	}
}

func TestFiniteNativeRequestEncodesIPv6MultipathExpiry(t *testing.T) {
	now := time.Now()
	_, dst, _ := net.ParseCIDR("2001:db8:3::/64")
	r := &netlink.Route{Family: unix.AF_INET6, Dst: dst, Table: 1000, Priority: 42, Protocol: RouteProtocol, Type: unix.RTN_UNICAST, MultiPath: []*netlink.NexthopInfo{
		{LinkIndex: 2, Gw: net.ParseIP("fe80::2"), Hops: 255},
		{LinkIndex: 3, Gw: net.ParseIP("fe80::3"), Hops: 2},
	}}
	req, err := expiringRouteRequest(r, now.Add(30500*time.Millisecond), now)
	if err != nil {
		t.Fatal(err)
	}
	raw := req.Serialize()[unix.NLMSG_HDRLEN:]
	observed, err := decodeNativeRoute(raw, map[int]string{2: "wan0", 3: "wan1"}, now)
	want := Route{Destination: dst.String(), Table: 1000, Metric: 42, ValidUntil: now.Add(31 * time.Second), NextHops: []NextHop{{Gateway: "fe80::2", Interface: "wan0", Weight: 256}, {Gateway: "fe80::3", Interface: "wan1", Weight: 3}}}
	if err != nil || !routeMatches(observed, want) || !observed.ValidUntil.Equal(want.ValidUntil) {
		t.Fatalf("native finite ECMP request encoded incorrectly: %+v %v", observed, err)
	}
	if _, err := expiringRouteRequest(r, now, now); err == nil {
		t.Fatal("expired route became an infinite route")
	}
}

func TestDeadlineValidationIndependentOfTimezone(t *testing.T) {
	for _, offset := range []int{-12 * 3600, 0, 9 * 3600, 14 * 3600} {
		zone := time.FixedZone("test", offset)
		deadline := time.Unix(-1, 0).In(zone)
		route := Route{Destination: "2001:db8::/64", Interface: "wan0", ValidUntil: deadline}
		if err := Validate(Desired{Routes: []Route{route}}); err == nil {
			t.Fatalf("route accepted negative Unix timestamp at offset %d", offset)
		}
		iface := Interface{Name: "wan0", Addresses: []string{"2001:db8::1/64"}, AddressOptions: map[string]AddressOptions{"2001:db8::1/64": {PreferredUntil: deadline, ValidUntil: deadline}}}
		if err := Validate(Desired{Interfaces: []Interface{iface}}); err == nil {
			t.Fatalf("address accepted negative Unix timestamp at offset %d", offset)
		}
	}
}
