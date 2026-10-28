//go:build linux

package dhcp6

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/lifetime"
	"github.com/imksoo/routerd2/internal/protocol/dhcp6config"
)

func TestDeclineNoBindingCompletesWithoutLosingDelegation(t *testing.T) {
	clock := clockAt(time.Unix(1700000000, 0))
	prefix := dhcp6config.Prefix{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clock, 20), ValidUntil: lifetime.After(clock, 30)}
	c := client{
		cfg:  dhcp6config.Config{IAID: 7, RequestAddress: true, RequestPrefix: true, PrefixLength: 56},
		duid: []byte{0, 3, 0, 1}, server: []byte{0, 3, 0, 2},
		declined: []assignment{{prefix: netip.MustParsePrefix("2001:db8:42::100/128")}},
		snapshot: dhcp6config.Snapshot{BootID: clock.BootID, Prefixes: []dhcp6config.Prefix{prefix}},
	}
	if err := c.begin(decline, time.Unix(0, clock.BoottimeNanoseconds)); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(message{kind: reply, xid: c.lease.xid, client: c.duid, server: c.server, status: 3}, clock); err != nil {
		t.Fatal(err)
	}
	if c.lease.kind != solicit || len(c.snapshot.Prefixes) != 1 || c.snapshot.Prefixes[0] != prefix {
		t.Fatal("NoBinding left Decline pending or changed unrelated prefix authority", c.snapshot, c.lease)
	}
}

func TestPartialRenewKeepsMissingPrefixExchangeAndOriginalT2(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := clockAt(now)
	prefix := dhcp6config.Prefix{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clock, 20), ValidUntil: lifetime.After(clock, 30)}
	c := client{
		cfg:  dhcp6config.Config{IAID: 7, RequestAddress: true, RequestPrefix: true},
		duid: []byte{0, 3, 0, 1}, server: []byte{0, 3, 0, 2},
		snapshot: dhcp6config.Snapshot{BootID: clock.BootID, Prefixes: []dhcp6config.Prefix{prefix}, RenewAt: lifetime.After(clock, 2), RebindAt: lifetime.After(clock, 4)},
	}
	if err := c.begin(renew, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	xid := c.lease.xid
	m := message{kind: reply, xid: xid, client: c.duid, server: c.server, na: &association{iaid: 7, t1: 5, t2: 8, leases: []assignment{{netip.MustParsePrefix("2001:db8:42::10/128"), 20, 30}}}}
	if err := c.accept(m, clockAt(now.Add(3*time.Second))); err != nil {
		t.Fatal(err)
	}
	if c.lease.kind != renew || c.lease.xid != xid || c.snapshot.RebindAt != lifetime.After(clock, 4) || len(c.snapshot.Addresses) != 1 || len(c.snapshot.Prefixes) != 1 || c.snapshot.Prefixes[0] != prefix {
		t.Fatal("NA-only Reply completed PD renewal or changed its original budget", c.snapshot, c.lease)
	}
	if err := c.advance(clockAt(now.Add(4 * time.Second))); err != nil || c.lease.kind != rebind {
		t.Fatal("partial Reply postponed original T2", err, c.lease)
	}
}

func TestCombinedAddressAndPrefixExchange(t *testing.T) {
	now := time.Unix(1700000000, 0)
	c := client{cfg: dhcp6config.Config{IAID: 7, RequestAddress: true, RequestPrefix: true, PrefixLength: 56}, duid: []byte{0, 3, 0, 1}, server: []byte{0, 3, 0, 2}, snapshot: dhcp6config.Snapshot{BootID: clockAt(now).BootID, State: "bound"}}
	if err := c.begin(renew, now); err != nil {
		t.Fatal(err)
	}
	m := message{kind: reply, xid: c.lease.xid, client: c.duid, server: c.server, refresh: 600,
		na: &association{iaid: 7, t1: 2, t2: 4, leases: []assignment{{netip.MustParsePrefix("2001:db8:42::10/128"), 6, 10}, {netip.MustParsePrefix("2001:db8:42::11/128"), 8, 12}}},
		pd: &association{iaid: 7, t1: 3, t2: 5, leases: []assignment{{netip.MustParsePrefix("2001:db8:100::/56"), 20, 30}}},
	}
	wrong := m
	wrong.server = []byte{0, 3, 0, 9}
	if err := c.accept(wrong, clockAt(now)); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot.Addresses) != 0 {
		t.Fatal("foreign server extended a Renew")
	}
	if err := c.accept(m, clockAt(now)); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot.Addresses) != 2 || len(c.snapshot.Prefixes) != 1 || c.snapshot.RenewAt != lifetime.After(clockAt(now), 2) || c.snapshot.RebindAt != lifetime.After(clockAt(now), 4) {
		t.Fatal(c.snapshot)
	}
	if err := c.advance(clockAt(now.Add(2 * time.Second))); err != nil {
		t.Fatal(err)
	}
	if c.lease.kind != renew {
		t.Fatal("T1 did not renew")
	}
	packet := c.packet(&c.lease, now.Add(2*time.Second))
	opts, err := options(packet[4:])
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		if o.code == 8 && binary.BigEndian.Uint16(o.value) != 0 {
			t.Fatal("first message elapsed time is not zero")
		}
	}
	c.lease.attempts = 1
	retry := c.packet(&c.lease, now.Add(2250*time.Millisecond))
	opts, err = options(retry[4:])
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		if o.code == 8 && binary.BigEndian.Uint16(o.value) != 25 {
			t.Fatal("retry elapsed time includes the initial random delay")
		}
	}
	decoded, err := decode(packet, c.cfg.IAID)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.na == nil || decoded.pd == nil || decoded.na.iaid != 7 || decoded.pd.iaid != 7 || !bytes.Equal(decoded.client, c.duid) || len(decoded.na.leases) != 2 {
		t.Fatal("IAs did not share transaction/identity", decoded)
	}
	for _, ia := range []*association{decoded.na, decoded.pd} {
		if ia.t1 != 0 || ia.t2 != 0 {
			t.Fatal("client selected server timers")
		}
		for _, a := range ia.leases {
			if a.preferred != 0 || a.valid != 0 {
				t.Fatal("client selected server lifetimes")
			}
		}
	}
	if err := c.advance(clockAt(now.Add(4 * time.Second))); err != nil {
		t.Fatal(err)
	}
	if c.lease.kind != rebind {
		t.Fatal("unanswered Renew did not rebind")
	}
	// NoBinding during Rebind starts Request even after the old T2. That
	// recovery exchange must not be overwritten by the old renewal timer.
	missing := message{kind: reply, xid: c.lease.xid, client: c.duid, server: []byte{0, 3, 0, 9}, na: &association{iaid: 7, status: 3}}
	if err := c.accept(missing, clockAt(now.Add(5*time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := c.advance(clockAt(now.Add(5 * time.Second))); err != nil {
		t.Fatal(err)
	}
	if c.lease.kind != request {
		t.Fatal("old T2 replaced NoBinding recovery Request")
	}
	target, err := decode(c.packet(&c.lease, now.Add(5*time.Second)), c.cfg.IAID)
	if err != nil || !bytes.Equal(target.server, missing.server) {
		t.Fatal("NoBinding Request did not target the responding server", target, err)
	}
	if err := c.advance(clockAt(now.Add(13 * time.Second))); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot.Addresses) != 0 || len(c.snapshot.Prefixes) != 1 {
		t.Fatal("IA_NA expiry removed surviving IA_PD", c.snapshot)
	}
}
func TestRenumberRetainsOriginalOldPrefixDeadline(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := dhcp6config.Prefix{Prefix: "2001:db8:100::/56", PreferredUntil: lifetime.After(clockAt(now), 20), ValidUntil: lifetime.After(clockAt(now), 30)}
	c := client{cfg: dhcp6config.Config{IAID: 7, RequestPrefix: true}, duid: []byte{0, 3, 0, 1}, server: []byte{0, 3, 0, 2}, snapshot: dhcp6config.Snapshot{BootID: clockAt(now).BootID, State: "bound", Prefixes: []dhcp6config.Prefix{old}}}
	if err := c.begin(renew, now); err != nil {
		t.Fatal(err)
	}
	m := message{kind: reply, xid: c.lease.xid, client: c.duid, server: c.server, pd: &association{iaid: 7, t1: 2, t2: 4, leases: []assignment{{netip.MustParsePrefix("2001:db8:200::/56"), 40, 50}}}}
	if err := c.accept(m, clockAt(now)); err != nil {
		t.Fatal(err)
	}
	if len(c.snapshot.Prefixes) != 2 || c.snapshot.Prefixes[0].PreferredUntil != old.PreferredUntil || c.snapshot.Prefixes[0].ValidUntil != old.ValidUntil {
		t.Fatal("renumber refreshed an old prefix", c.snapshot)
	}
	// Late replies from the completed exchange cannot extend either assignment.
	if err := c.accept(m, clockAt(now.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if c.snapshot.Prefixes[0].ValidUntil != old.ValidUntil {
		t.Fatal("late reply extended old lease")
	}
}
func TestDecodeAddressAssociationAndRejectMalformedOptions(t *testing.T) {
	raw := netip.MustParseAddr("2001:db8:42::10").As16()
	address := append([]byte(nil), raw[:]...)
	address = binary.BigEndian.AppendUint32(address, 6)
	address = binary.BigEndian.AppendUint32(address, 10)
	ia := binary.BigEndian.AppendUint32(nil, 7)
	ia = binary.BigEndian.AppendUint32(ia, 2)
	ia = binary.BigEndian.AppendUint32(ia, 4)
	ia = appendOption(ia, 5, address)
	b := appendOption([]byte{7, 1, 2, 3}, 3, ia)
	m, err := decode(b, 7)
	if err != nil || m.na == nil || len(m.na.leases) != 1 || m.na.leases[0].prefix.String() != "2001:db8:42::10/128" {
		t.Fatal(m, err)
	}
	if _, err := decode(append(b, 0, 1, 0), 7); err == nil {
		t.Fatal("truncated option accepted")
	}
	unrelated := append([]byte(nil), ia...)
	binary.BigEndian.PutUint32(unrelated, 99)
	b = appendOption(appendOption([]byte{7, 1, 2, 3}, 3, unrelated), 3, ia)
	b = appendOption(b, 23, netip.MustParseAddr("fe80::53").AsSlice())
	m, err = decode(b, 7)
	if err != nil || m.na == nil || m.na.iaid != 7 || len(m.dns) != 1 || m.dns[0] != "fe80::53" {
		t.Fatal("unrelated IA or link-local DNS discarded the requested assignment", m, err)
	}
	for _, name := range [][]byte{{0xc0, 0}, {4, 'a', 'b'}, {0}} {
		if _, err := domainNames(name); err == nil {
			t.Fatal("invalid/compressed domain accepted", name)
		}
	}
}

func clockAt(t time.Time) boottime.Clock {
	return boottime.Clock{BootID: "11111111-2222-4333-8444-555555555555", BoottimeNanoseconds: t.UnixNano(), WallTime: time.Unix(1700000000, 0)}
}

func TestInformationInfinityDoesNotScheduleRefreshOrSurviveBootChange(t *testing.T) {
	clock := clockAt(time.Unix(1000, 0))
	c := client{duid: []byte{0, 3, 0, 1}, info: exchange{kind: information, xid: 42}, snapshot: dhcp6config.Snapshot{BootID: clock.BootID, State: "information"}}
	m := message{kind: reply, xid: 42, client: c.duid, server: []byte{0, 3, 0, 2}, refresh: ^uint32(0), dns: []string{"2001:db8::53"}}
	if err := c.accept(m, clock); err != nil {
		t.Fatal(err)
	}
	clock.BoottimeNanoseconds += int64(time.Hour)
	clock.WallTime = clock.WallTime.Add(-24 * time.Hour)
	if err := c.advance(clock); err != nil {
		t.Fatal(err)
	}
	if c.info.kind != 0 || !c.info.next.IsZero() || !c.snapshot.Active(clock) {
		t.Fatal("infinite information budget became a refresh timer")
	}
	clock.BootID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if c.snapshot.Active(clock) {
		t.Fatal("infinite information survived another boot")
	}
}

func TestConfirmKeepsOriginalGrantAndSuccessOverridesNotOnLink(t *testing.T) {
	clock := clockAt(time.Unix(1000, 0))
	saved := dhcp6config.Snapshot{BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), Interface: "wan0", IAID: 7, ClientDUID: "00030001", ServerDUID: "00030002", State: "bound", Addresses: []dhcp6config.Address{{Address: "2001:db8:42::100", PreferredUntil: lifetime.After(clock, 20), ValidUntil: lifetime.After(clock, 30)}}, RenewAt: lifetime.After(clock, 15), RebindAt: lifetime.After(clock, 24)}
	c := client{cfg: dhcp6config.Config{Interface: "wan0", IAID: 7, RequestAddress: true}, duid: []byte{0, 3, 0, 1}}
	restored, err := c.restoreLease(saved, clock)
	if err != nil || !restored || c.lease.kind != confirm || c.snapshot.Active(clock) {
		t.Fatal("address recovery skipped link confirmation", restored, err, c.snapshot)
	}
	packet := c.packet(&c.lease, time.Unix(0, clock.BoottimeNanoseconds))
	opts, err := options(packet[4:])
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range opts {
		if o.code == 2 || o.code == 25 {
			t.Fatal("Confirm included server identity or PD", o.code)
		}
	}
	response := message{kind: reply, xid: c.lease.xid, client: c.duid, server: []byte{0, 3, 0, 2}, status: 4}
	if err := c.accept(response, clock); err != nil || c.snapshot.Active(clock) {
		t.Fatal("NotOnLink enabled the saved address", err, c.snapshot)
	}
	response.server = []byte{0, 3, 0, 3}
	response.status = 0
	if err := c.accept(response, clock); err != nil || !c.snapshot.Active(clock) {
		t.Fatal("another server's Success did not complete Confirm", err, c.snapshot)
	}
	if c.snapshot.Addresses[0] != saved.Addresses[0] || c.snapshot.RenewAt != saved.RenewAt || c.snapshot.RebindAt != saved.RebindAt || c.snapshot.ServerDUID != saved.ServerDUID {
		t.Fatal("Confirm changed the original grant or lease server", saved, c.snapshot)
	}
}

func TestConfirmNotOnLinkDiscardsGrantAtEndOfReplyWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := clockAt(now)
	c := client{cfg: dhcp6config.Config{RequestAddress: true}, duid: []byte{0, 3, 0, 1}, snapshot: dhcp6config.Snapshot{BootID: clock.BootID, Addresses: []dhcp6config.Address{{Address: "2001:db8:42::100", ValidUntil: lifetime.After(clock, 30)}}}}
	if err := c.begin(confirm, now); err != nil {
		t.Fatal(err)
	}
	c.lease.verifyUntil = now.Add(10 * time.Second)
	if err := c.accept(message{kind: reply, xid: c.lease.xid, client: c.duid, server: []byte{0, 3, 0, 2}, status: 4}, clock); err != nil {
		t.Fatal(err)
	}
	if err := c.advance(clockAt(now.Add(9 * time.Second))); err != nil || c.lease.kind != confirm {
		t.Fatal("NotOnLink prevented a later Success", err, c.snapshot)
	}
	if err := c.advance(clockAt(now.Add(10 * time.Second))); err != nil || len(c.snapshot.Addresses) != 0 || c.lease.kind != solicit {
		t.Fatal("NotOnLink did not discard the old grant and discover again", err, c.snapshot)
	}
}
