package network

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"
)

type wireGuardDNSPeer struct {
	Interface string
	PublicKey wireGuardKey
	Endpoint  string
	Host      string
	Port      uint16
}

func (e *Engine) prepareWireGuardDNS(devices []WireGuard) {
	var next []wireGuardDNSPeer
	for _, device := range devices {
		for _, peer := range device.Peers {
			if peer.Endpoint == "" {
				continue
			}
			host, port, _ := parseWireGuardPeerEndpoint(peer.Endpoint)
			if _, err := netip.ParseAddr(host); err == nil {
				continue
			}
			key, _ := parseWireGuardKey(peer.PublicKey)
			next = append(next, wireGuardDNSPeer{device.Interface, key, peer.Endpoint, host, port})
		}
	}
	if slices.Equal(next, e.wireGuardDNS) {
		return
	}
	e.wireGuardDNS = next
	select {
	case e.wireGuardDNSWake <- struct{}{}:
	default:
	}
}

// wireguard-tools' reresolve-dns uses the last authenticated handshake to give
// roaming precedence for 135 seconds. Keep the same rule at both reads around
// DNS I/O. No learned address is written back into operator intent or journal.
// https://git.zx2c4.com/wireguard-tools/tree/contrib/reresolve-dns/reresolve-dns.sh
func wireGuardNeedsDNS(peer WireGuardPeerState) bool {
	return peer.LastHandshake.IsZero() || time.Since(peer.LastHandshake) > 135*time.Second
}

// Called with the engine lock. Recheck ownership and configuration after a DNS
// wait, including a handshake that may have established a new roaming endpoint.
func (e *Engine) wireGuardDNSObservation(target wireGuardDNSPeer) (WireGuardPeerState, int, error) {
	record := e.wireGuardRecord(target.Interface)
	if record == nil || record.Pending != nil {
		return WireGuardPeerState{}, 0, nil
	}
	wanted := false
	for _, peer := range record.Last.Peers {
		if peer.PublicKey == target.PublicKey.encoded() && peer.Endpoint == target.Endpoint {
			wanted = true
			break
		}
	}
	if !wanted {
		return WireGuardPeerState{}, 0, nil
	}
	link, err := e.kernel.Interface(target.Interface)
	if err != nil {
		return WireGuardPeerState{}, 0, err
	}
	if link.Index != record.Index || !e.ownsLink(link) {
		return WireGuardPeerState{}, 0, errors.New("WireGuard endpoint link ownership changed")
	}
	live, err := e.kernel.(wireGuardKernel).ReadWireGuard(link.Index)
	defer clearWireGuardDevice(&live)
	if err != nil {
		return WireGuardPeerState{}, 0, err
	}
	if !wireGuardOwned(live, *record) {
		return WireGuardPeerState{}, 0, errors.New("WireGuard endpoint peer ownership changed")
	}
	for _, peer := range live.Peers {
		if peer.PublicKey == target.PublicKey {
			return WireGuardPeerState{Endpoint: peer.Endpoint, LastHandshake: peer.LastHandshake}, link.Index, nil
		}
	}
	return WireGuardPeerState{}, 0, nil
}

func (e *Engine) refreshWireGuardEndpoint(ctx context.Context, target wireGuardDNSPeer) error {
	e.mu.Lock()
	peer, index, err := e.wireGuardDNSObservation(target)
	e.mu.Unlock()
	if err != nil || index == 0 || !wireGuardNeedsDNS(peer) {
		return err
	}
	lookup, cancel := context.WithTimeout(ctx, 2*time.Second)
	addresses, err := net.DefaultResolver.LookupNetIP(lookup, "ip", target.Host)
	cancel()
	if err != nil {
		return err
	}
	addresses = slices.DeleteFunc(addresses, func(a netip.Addr) bool {
		return !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a.Is6() && a.IsLinkLocalUnicast()
	})
	if len(addresses) == 0 {
		return errors.New("WireGuard endpoint DNS returned no usable address")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	peer, index, err = e.wireGuardDNSObservation(target)
	if err != nil || index == 0 || !wireGuardNeedsDNS(peer) {
		return err
	}
	endpoint := netip.AddrPortFrom(addresses[0].Unmap(), target.Port).String()
	for _, address := range addresses {
		candidate := netip.AddrPortFrom(address.Unmap(), target.Port).String()
		if candidate == peer.Endpoint {
			return nil
		}
	}
	return e.kernel.(wireGuardKernel).SetWireGuardEndpoint(index, target.PublicKey, endpoint)
}

// DNS waits never hold the writer lock. The network worker cancels and joins
// this loop before closing its engine; failed lookups leave working peers alone.
func (e *Engine) MaintainWireGuardEndpoints(ctx context.Context) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-e.wireGuardDNSWake:
		case <-ticker.C:
		}
		e.mu.Lock()
		peers := slices.Clone(e.wireGuardDNS)
		e.mu.Unlock()
		for _, peer := range peers {
			if err := e.refreshWireGuardEndpoint(ctx, peer); err != nil && ctx.Err() == nil {
				slog.Warn("WireGuard endpoint resolution failed", "interface", peer.Interface, "host", peer.Host, "error", err)
			}
			if ctx.Err() != nil {
				return nil
			}
		}
	}
}
