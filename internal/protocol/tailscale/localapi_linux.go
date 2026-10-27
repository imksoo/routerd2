//go:build linux

package tailscale

import (
	"encoding/json"
	"net/netip"
	"sort"

	"github.com/imksoo/routerd2/internal/protocol/tailscaleconfig"
)

// startRequest is the pinned upstream ipn.Options contract. LocalBackend.Start
// replaces owned preferences together while explicitly retaining Persist (node
// identity); do not edit the vendor's state file to change preferences.
type startRequest struct {
	UpdatePrefs preferences
	AuthKey     string
}

type preferences struct {
	ControlURL      string
	Hostname        string
	WantRunning     bool
	RouteAll        bool
	CorpDNS         bool
	ShieldsUp       bool
	AdvertiseRoutes []netip.Prefix
	AdvertiseTags   []string
	NetfilterMode   int
	NoSNAT          bool
	RunSSH          bool
	AutoUpdate      struct {
		Check bool
		Apply bool
	}
}

func startOptions(n tailscaleconfig.Node, key string) startRequest {
	p := preferences{ControlURL: n.ServerURL, Hostname: n.Hostname, WantRunning: true, RouteAll: n.AcceptRoutes, ShieldsUp: n.ShieldsUp, AdvertiseTags: n.AdvertiseTags, NetfilterMode: 0}
	for _, prefix := range n.AdvertiseRoutes {
		p.AdvertiseRoutes = append(p.AdvertiseRoutes, prefix.Masked())
	}
	if n.AdvertiseExitNode {
		p.AdvertiseRoutes = append(p.AdvertiseRoutes, netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0"))
	}
	return startRequest{UpdatePrefs: p, AuthKey: key}
}

type peerStatus struct {
	ID           string
	HostName     string
	Online       bool
	TailscaleIPs []netip.Addr
	AllowedIPs   []netip.Prefix
	Relay        string
	CurAddr      string
	RxBytes      int64
	TxBytes      int64
}
type status struct {
	BackendState string
	Self         *peerStatus
	Peer         map[string]peerStatus
}

func parseStatus(data []byte) (tailscaleconfig.Snapshot, error) {
	var state status
	if err := json.Unmarshal(data, &state); err != nil {
		return tailscaleconfig.Snapshot{}, err
	}
	out := tailscaleconfig.Snapshot{BackendState: state.BackendState}
	if state.Self != nil {
		out.NodeID = state.Self.ID
		out.Online = state.Self.Online
		out.Addresses = state.Self.TailscaleIPs
	}
	for _, peer := range state.Peer {
		out.Peers = append(out.Peers, tailscaleconfig.Peer{ID: peer.ID, Hostname: peer.HostName, Online: peer.Online, Addresses: peer.TailscaleIPs, AllowedPrefixes: peer.AllowedIPs, HomeDERP: peer.Relay, DirectAddress: peer.CurAddr, RxBytes: peer.RxBytes, TxBytes: peer.TxBytes})
	}
	sort.Slice(out.Peers, func(i, j int) bool { return out.Peers[i].ID < out.Peers[j].ID })
	return out, nil
}
