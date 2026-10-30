// SPDX-License-Identifier: BSD-3-Clause

// Package bgp runs the GoBGP library as a supervised routerd module. It owns
// protocol sessions and the local RIB, never Linux links, routes or commands.
package bgp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/imksoo/routerd/pkg/bgpdaemon"
	"github.com/imksoo/routerd2/internal/protocol/bgpconfig"
	gobgpapi "github.com/osrg/gobgp/v4/api"
)

type Config = bgpconfig.Config
type PeerTransport = bgpconfig.PeerTransport

func Normalize(c Config) Config { return bgpconfig.Normalize(c) }
func Validate(c Config) error   { return bgpconfig.Validate(c) }

type Route = bgpconfig.Route
type Peer = bgpconfig.Peer
type Snapshot = bgpconfig.Snapshot
type Event = bgpconfig.Event

type Effects interface {
	Update(context.Context, Snapshot) error
}
type EffectsFunc func(context.Context, Snapshot) error

func (f EffectsFunc) Update(ctx context.Context, s Snapshot) error { return f(ctx, s) }

func configuredPeer(peer bgpdaemon.AppliedPeer, global bgpdaemon.AppliedGlobal, tr PeerTransport) *gobgpapi.Peer {
	p := appliedPeer(peer, global)
	if tr.LocalAddress != "" || tr.RemotePort != 0 || tr.BindInterface != "" {
		if p.Transport == nil {
			p.Transport = &gobgpapi.Transport{}
		}
		p.Transport.LocalAddress = tr.LocalAddress
		p.Transport.RemotePort = uint32(tr.RemotePort)
		p.Transport.BindInterface = tr.BindInterface
	}
	return p
}

func uniqueTrimmedSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, s := range values {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	sort.Strings(out)
	return out
}

func policyPeerName(value string) string {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			out.WriteRune(r)
			dash = false
		} else if !dash {
			out.WriteByte('-')
			dash = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		name = "peer"
	}
	// Compressed IPv6 colon runs can collapse to the same printable name.
	// Keep the original IPv4 policy names while fencing IPv6 scopes uniquely.
	if strings.Contains(value, ":") {
		digest := sha256.Sum256([]byte(value))
		name += fmt.Sprintf("-%x", digest[:4])
	}
	return name
}
