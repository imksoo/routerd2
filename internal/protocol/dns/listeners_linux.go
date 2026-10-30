//go:build linux

package dns

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/imksoo/routerd/pkg/daemonapi"
	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// inputMu owns listener changes and shutdown. Static listeners, their existing
// TCP connections and upstream pools survive unrelated address changes.
func (d *daemon) updateListeners(addresses []dnsconfig.ListenAddress, clock boottime.Clock) error {
	if d.runCtx == nil {
		return nil
	}
	desired := maps.Clone(d.staticListeners)
	for _, source := range d.listenSources {
		if source.Interface == "" {
			continue
		}
		link, err := netlink.LinkByName(source.Interface)
		if err != nil {
			var missing netlink.LinkNotFoundError
			if errors.As(err, &missing) {
				continue
			}
			return err
		}
		family := netlink.FAMILY_ALL
		if source.Family == 4 {
			family = netlink.FAMILY_V4
		} else if source.Family == 6 {
			family = netlink.FAMILY_V6
		}
		found, err := netlink.AddrList(link, family)
		if err != nil {
			return err
		}
		for _, address := range found {
			if address.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) != 0 {
				continue
			}
			ip, ok := netip.AddrFromSlice(address.IP)
			if !ok {
				return errors.New("kernel returned an invalid DNS interface address")
			}
			ip = ip.Unmap()
			if ip.Is6() && ip.IsLinkLocalUnicast() {
				ip = ip.WithZone(source.Interface)
			}
			endpoint := netip.AddrPortFrom(ip, uint16(d.config.Spec.Listen[source.Listener].Port))
			key := endpoint.String()
			if previous, found := desired[key]; found && previous.listener != source.Listener {
				return fmt.Errorf("DNS listeners compete for %s", key)
			}
			desired[key] = listenBinding{listener: source.Listener, address: endpoint}
		}
	}
	for _, address := range addresses {
		if address.ValidUntil.Live(clock) {
			i := d.listenSources[address.Source].Listener
			endpoint := netip.AddrPortFrom(address.Address, uint16(d.config.Spec.Listen[i].Port))
			key := endpoint.String()
			if previous, found := desired[key]; found && previous.listener != i {
				return fmt.Errorf("DNS listeners compete for %s", key)
			}
			if previous, found := desired[key]; !found || previous.until.Remaining(clock) < address.ValidUntil.Remaining(clock) {
				desired[key] = listenBinding{listener: i, address: endpoint, until: address.ValidUntil}
			}
		}
	}
	additions := make(map[string]listenBinding)
	removed := make(map[string]*boundListener)
	d.stateMu.RLock()
	for address, binding := range desired {
		if d.listeners[address] == nil {
			additions[address] = binding
		}
	}
	d.stateMu.RUnlock()
	added, err := d.openBoundListeners(d.runCtx, additions)
	if err != nil {
		return err
	}
	d.stateMu.Lock()
	for address, listener := range d.listeners {
		if binding, keep := desired[address]; !keep {
			removed[address] = listener
			delete(d.listeners, address)
		} else {
			listener.listenBinding = binding
		}
	}
	for address, listener := range added {
		d.listeners[address] = listener
	}
	d.stateMu.Unlock()
	shutdownBoundListeners(removed)
	if len(added)+len(removed) > 0 {
		return d.reportListening()
	}
	return nil
}

func (d *daemon) refreshListeners() error {
	d.inputMu.Lock()
	defer d.inputMu.Unlock()
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	var addresses []dnsconfig.ListenAddress
	if d.nativeInput != nil {
		addresses = d.nativeInput.Listeners
	}
	return d.updateListeners(addresses, clock)
}

// The private API and at least one actual socket must exist before readiness.
// Missing dynamic addresses remain pending; they do not restart the worker.
func (d *daemon) reportListening() error {
	select {
	case <-d.apiReady:
	default:
		return nil
	}
	if d.listenerCount() == 0 {
		return d.setState(daemonapi.PhaseStarting, daemonapi.HealthUnknown)
	}
	if err := d.setState(daemonapi.PhaseRunning, daemonapi.HealthOK); err != nil {
		return err
	}
	if d.ready != nil {
		d.ready()
		d.ready = nil
	}
	return nil
}

// The reporting loop is the sole reporter. It sends a complete current observation;
// replaying a previous snapshot cannot extend its finite lifetime.
func (d *daemon) publishListeners(ctx context.Context) error {
	select {
	case <-d.apiReady:
	default:
		return nil
	}
	clock, err := boottime.Read()
	if err != nil {
		return err
	}
	out := dnsconfig.Snapshot{Resource: d.config.Resource, BootID: clock.BootID, ObservedNanos: uint64(clock.BoottimeNanoseconds), ExpiresNanos: uint64(clock.BoottimeNanoseconds) + uint64(dnsconfig.ObservationLifetime), QueryOverloads: d.queryOverloads.Load()}
	d.stateMu.RLock()
	for _, listener := range d.listeners {
		if listener.until.Live(clock) {
			out.Listeners = append(out.Listeners, listener.address)
		}
	}
	d.stateMu.RUnlock()
	slices.SortFunc(out.Listeners, func(a, b netip.AddrPort) int { return a.Compare(b) })
	return d.report(ctx, out)
}
