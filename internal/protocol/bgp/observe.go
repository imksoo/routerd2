// SPDX-License-Identifier: BSD-3-Clause
package bgp

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	gobgpapiutil "github.com/osrg/gobgp/v4/pkg/apiutil"
	gobgp "github.com/osrg/gobgp/v4/pkg/packet/bgp"
	gobgpserver "github.com/osrg/gobgp/v4/pkg/server"
)

const leaseDuration = 5 * time.Second

type observation struct {
	mu       sync.RWMutex
	latest   Snapshot
	events   []Event
	sequence uint64
	cfg      Config
	epoch    string
	effects  Effects
	fail     func(error)
}

func selectedRoute(cfg Config, prefix string, p *gobgpapiutil.Path) (Route, bool) {
	if p == nil || !p.Best || p.Withdrawal || p.Filtered || p.IsNexthopInvalid || !p.PeerAddress.IsValid() || p.PeerAddress.IsUnspecified() {
		return Route{}, false
	}
	nlri, err := netip.ParsePrefix(prefix)
	if err != nil || nlri.Addr().Is4In6() || nlri.Addr().IsMulticast() {
		return Route{}, false
	}
	var hop netip.Addr
	for _, attr := range p.Attrs {
		switch a := attr.(type) {
		case *gobgp.PathAttributeNextHop:
			hop = a.Value
		case *gobgp.PathAttributeMpReachNLRI:
			hop = a.Nexthop
			if !hop.IsValid() || hop.IsUnspecified() {
				hop = a.LinkLocalNexthop
			}
		}
	}
	if !hop.IsValid() || hop.IsUnspecified() || hop.IsMulticast() || hop.Is4In6() || hop.Is4() != nlri.Addr().Is4() {
		return Route{}, false
	}
	family := "ipv6-unicast"
	if nlri.Addr().Is4() {
		family = "ipv4-unicast"
	}
	iface := cfg.PeerTransports[p.PeerAddress.String()].FIBInterface
	if iface == "" {
		iface = cfg.FIBInterface
	}
	return Route{Prefix: nlri.Masked().String(), Family: family, NextHop: hop.String(), Interface: iface, PeerAddress: p.PeerAddress.String(), Table: cfg.Table, Metric: cfg.RouteMetric, Stale: p.Stale}, true
}

func readSnapshot(ctx context.Context, server *gobgpserver.BgpServer, cfg Config) (Snapshot, error) {
	snapshot := Snapshot{Resource: cfg.Resource, Active: true, Routes: []Route{}, Peers: []Peer{}}
	global, err := server.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		return snapshot, err
	}
	if global.GetGlobal().GetAsn() == 0 {
		return snapshot, errors.New("BGP global instance is stopped")
	}
	seen := make(map[Route]bool)
	for _, family := range cfg.Runtime.Global.Families {
		rf := gobgp.RF_IPv4_UC
		if family == "ipv6-unicast" {
			rf = gobgp.RF_IPv6_UC
		}
		var callbackErr error
		err := server.ListPath(gobgpapiutil.ListPathRequest{TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL, Family: rf}, func(prefix gobgp.NLRI, paths []*gobgpapiutil.Path) {
			if callbackErr != nil {
				return
			}
			if ctx.Err() != nil {
				callbackErr = ctx.Err()
				return
			}
			for _, path := range paths {
				route, ok := selectedRoute(cfg, prefix.String(), path)
				if !ok {
					if path != nil && path.Best && path.PeerAddress.IsValid() && !path.PeerAddress.IsUnspecified() {
						snapshot.RejectedPaths++
					}
					continue
				}
				if seen[route] {
					continue
				}
				seen[route] = true
				if len(snapshot.Routes) >= cfg.MaxRoutes {
					callbackErr = errors.New("selected BGP routes exceed max_routes; refusing a partial FIB snapshot")
					return
				}
				snapshot.Routes = append(snapshot.Routes, route)
			}
		})
		if err != nil {
			return snapshot, err
		}
		if callbackErr != nil {
			return snapshot, callbackErr
		}
	}
	sort.Slice(snapshot.Routes, func(i, j int) bool {
		a, b := snapshot.Routes[i], snapshot.Routes[j]
		if a.Prefix != b.Prefix {
			return a.Prefix < b.Prefix
		}
		if a.NextHop != b.NextHop {
			return a.NextHop < b.NextHop
		}
		return a.PeerAddress < b.PeerAddress
	})
	err = server.ListPeer(ctx, &gobgpapi.ListPeerRequest{}, func(p *gobgpapi.Peer) {
		snapshot.Peers = append(snapshot.Peers, Peer{
			Address: p.GetConf().GetNeighborAddress(), ASN: p.GetConf().GetPeerAsn(),
			State: p.GetState().GetSessionState().String(), AdminState: p.GetState().GetAdminState().String(),
			LocalAddress: p.GetTransport().GetLocalAddress(), RemotePort: p.GetTransport().GetRemotePort(),
		})
	})
	if err != nil {
		return snapshot, err
	}
	sort.Slice(snapshot.Peers, func(i, j int) bool { return snapshot.Peers[i].Address < snapshot.Peers[j].Address })
	return snapshot, ctx.Err()
}

func cloneSnapshot(s Snapshot) Snapshot {
	s.Routes = append([]Route{}, s.Routes...)
	s.Peers = append([]Peer{}, s.Peers...)
	if s.BFD != nil {
		copy := make(map[string]bgpconfig.BFDAcknowledgement, len(s.BFD))
		for address, proof := range s.BFD {
			copy[address] = proof
		}
		s.BFD = copy
	}
	return s
}

func (o *observation) publish(ctx context.Context, s Snapshot) error {
	o.sequence++
	s.Resource, s.Epoch, s.Sequence = o.cfg.Resource, o.epoch, o.sequence
	s.ObservedAt = time.Now().UTC()
	if s.Active {
		s.ValidUntil = s.ObservedAt.Add(leaseDuration)
	} else {
		s.ValidUntil = time.Time{}
		s.Routes = []Route{}
	}
	call, cancel := context.WithTimeout(ctx, time.Duration(o.cfg.EffectTimeoutMilliseconds)*time.Millisecond)
	err := o.effects.Update(call, cloneSnapshot(s))
	if err == nil {
		err = call.Err()
	}
	cancel()
	if err != nil {
		return fmt.Errorf("publish BGP ownership: %w", err)
	}
	if s.Active && !time.Now().Before(s.ValidUntil) {
		return errors.New("BGP ownership acknowledgment arrived after its lease expired")
	}
	if o.cfg.StatusPath != "" {
		if err := atomicJSON(o.cfg.StatusPath, s); err != nil {
			return fmt.Errorf("persist BGP status: %w", err)
		}
	}
	event := Event{Type: "bgp.snapshot", Resource: s.Resource, Epoch: s.Epoch, Sequence: s.Sequence, Time: s.ObservedAt, Active: s.Active, Routes: len(s.Routes), Peers: len(s.Peers), Error: s.Error}
	if err := appendEvent(o.cfg.EventPath, event); err != nil {
		return fmt.Errorf("append BGP event: %w", err)
	}
	o.mu.Lock()
	o.latest = s
	if len(o.events) == 128 {
		copy(o.events, o.events[1:])
		o.events = o.events[:127]
	}
	o.events = append(o.events, event)
	o.mu.Unlock()
	return nil
}
