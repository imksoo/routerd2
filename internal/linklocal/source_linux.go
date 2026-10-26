// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// Package linklocal selects a usable kernel IPv6 link-local source.
package linklocal

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Read returns an invalid source while automatic addressing or DAD is pending.
func Read(name string, wanted netip.Addr) (*net.Interface, netip.Addr, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	if ifi.Flags&net.FlagUp == 0 {
		return nil, netip.Addr{}, errors.New("IPv6 source interface is not administratively up")
	}
	link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Index: ifi.Index}}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, netip.Addr{}, err
	}
	for _, a := range addresses {
		source, ok := netip.AddrFromSlice(a.IP)
		if ok && source.IsLinkLocalUnicast() && a.Flags&(unix.IFA_F_TENTATIVE|unix.IFA_F_DADFAILED) == 0 && (!wanted.IsValid() || source == wanted) {
			return ifi, source, nil
		}
	}
	return ifi, netip.Addr{}, nil
}

// Wait preserves routerd's link-local readiness prerequisite without installing
// an address or disabling DAD. Cancellation and the supervisor's startup bound
// govern the wait; normal kernel DAD must not require worker replacement.
func Wait(ctx context.Context, name string, wanted netip.Addr) (*net.Interface, netip.Addr, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, netip.Addr{}, err
		}
		ifi, source, err := Read(name, wanted)
		if err != nil || source.IsValid() {
			return ifi, source, err
		}
		select {
		case <-ctx.Done():
			return nil, netip.Addr{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
