//go:build linux

package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/imksoo/routerd2/internal/boottime"
	"github.com/imksoo/routerd2/internal/statefs"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The fixed upstream router owns table 52 and rule priorities 5210..5270.
// A durable boot marker records that exclusive scope before starting it. It
// permits cleanup after SIGKILL, without treating observed foreign rules as ours.
type kernelOwner struct {
	BootID    string `json:"boot_id"`
	Namespace uint64 `json:"network_namespace_cookie"`
}

func kernelScope(remove bool) (bool, error) {
	found := false
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		rules, err := netlink.RuleList(family)
		if err != nil {
			return false, err
		}
		for _, rule := range rules {
			if rule.Table == 52 || rule.Priority >= 5210 && rule.Priority <= 5270 {
				found = true
				if remove {
					if err = netlink.RuleDel(&rule); err != nil {
						return false, err
					}
				}
			}
		}
		routes, err := netlink.RouteListFiltered(family, &netlink.Route{Table: 52}, netlink.RT_FILTER_TABLE)
		if err != nil {
			return false, err
		}
		for _, route := range routes {
			found = true
			if remove {
				if err = netlink.RouteDel(&route); err != nil {
					return false, err
				}
			}
		}
	}
	return found, nil
}

func claimKernel(ctx context.Context, dir *os.File, iface string, namespace uint64) (func() error, error) {
	clock, err := boottime.Read()
	if err != nil {
		return nil, err
	}
	raw, err := statefs.ReadPrivate(ctx, dir, "kernel-owner.json", 4096)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	var owner kernelOwner
	if err == nil {
		if err = json.Unmarshal(raw, &owner); err != nil {
			return nil, err
		}
	}
	owned := owner.BootID == clock.BootID && owner.Namespace == namespace
	_, err = netlink.LinkByName(iface)
	if err == nil {
		return nil, errors.New("Tailscale TUN name is already in use")
	}
	var absent netlink.LinkNotFoundError
	if !errors.As(err, &absent) {
		return nil, err
	}
	occupied, err := kernelScope(false)
	if err != nil {
		return nil, err
	}
	if occupied && !owned {
		return nil, errors.New("Tailscale table52 or rule priorities5210..5270 are already in use")
	}
	if owned {
		if _, err = kernelScope(true); err != nil {
			return nil, err
		}
	}
	owner = kernelOwner{BootID: clock.BootID, Namespace: namespace}
	raw, err = json.Marshal(owner)
	if err != nil {
		return nil, err
	}
	// The directory is private and locked by the worker. Sync the ownership
	// record before any engine netlink effects, including partial startup.
	fd, err := unix.Openat(int(dir.Fd()), "kernel-owner.pending", unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "tailscale-owner")
	defer unix.Unlinkat(int(dir.Fd()), "kernel-owner.pending", 0)
	_, err = f.Write(raw)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	if err = unix.Renameat(int(dir.Fd()), "kernel-owner.pending", int(dir.Fd()), "kernel-owner.json"); err != nil {
		return nil, err
	}
	if err = dir.Sync(); err != nil {
		return nil, err
	}
	return func() error {
		if _, err := kernelScope(true); err != nil {
			return err
		}
		return errors.Join(unix.Unlinkat(int(dir.Fd()), "kernel-owner.json", 0), dir.Sync())
	}, nil
}
