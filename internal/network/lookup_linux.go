//go:build linux

package network

import (
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// RouteLookupRequest scopes a read-only FIB lookup to the exact probe path.
// It does not create routes, change ownership, or execute an external utility.
type RouteLookupRequest struct {
	Target    string `json:"target"`
	Interface string `json:"interface"`
	Source    string `json:"source,omitempty"`
	Mark      uint32 `json:"mark,omitempty"`
}
type RouteLookupResult struct {
	Interface string `json:"interface"`
	Source    string `json:"source,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Table     int    `json:"table"`
}

func lookupInputs(r RouteLookupRequest) (net.IP, *netlink.RouteGetOptions, error) {
	target, err := netip.ParseAddr(r.Target)
	if err != nil || target.Zone() != "" || target.Is4In6() || target.IsUnspecified() || target.IsMulticast() || !interfaceName.MatchString(r.Interface) {
		return nil, nil, errors.New("route lookup requires a unicast target and interface")
	}
	options := &netlink.RouteGetOptions{Oif: r.Interface, Mark: r.Mark}
	if r.Source != "" {
		source, err := netip.ParseAddr(r.Source)
		if err != nil || source.Zone() != "" || source.Is4In6() || source.IsUnspecified() || source.IsMulticast() || source.Is4() != target.Is4() {
			return nil, nil, errors.New("route lookup source family does not match target")
		}
		options.SrcAddr = net.IP(source.AsSlice())
	}
	return net.IP(target.AsSlice()), options, nil
}
func LookupRoute(r RouteLookupRequest) (RouteLookupResult, error) {
	target, options, err := lookupInputs(r)
	if err != nil {
		return RouteLookupResult{}, err
	}
	// Only routing is needed. Opening every supported Netlink family also
	// requests XFRM, which may be absent in minimal appliance kernels.
	handle, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return RouteLookupResult{}, err
	}
	defer handle.Close()
	if err := handle.SetSocketTimeout(100 * time.Millisecond); err != nil {
		return RouteLookupResult{}, err
	}
	routes, err := handle.RouteGetWithOptions(target, options)
	if err != nil {
		return RouteLookupResult{}, err
	}
	if len(routes) == 0 {
		return RouteLookupResult{}, errors.New("no matching route")
	}
	route := routes[0]
	link, err := handle.LinkByIndex(route.LinkIndex)
	if err != nil {
		return RouteLookupResult{}, err
	}
	result := RouteLookupResult{Interface: link.Attrs().Name, Table: route.Table}
	if len(route.Src) > 0 {
		result.Source = route.Src.String()
	}
	if len(route.Gw) > 0 {
		result.Gateway = route.Gw.String()
	}
	return result, nil
}
