// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package dns

import (
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func dnsListenConfig(iface string) net.ListenConfig {
	return net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		if err := enableFreebind(network, address, c); err != nil {
			return err
		}
		if iface == "" {
			return nil
		}
		var sockErr error
		if err := c.Control(func(fd uintptr) {
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
		}); err != nil {
			return err
		}
		return sockErr
	}}
}

func enableFreebind(_, _ string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		ipv4Err := unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
		ipv6Err := unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_FREEBIND, 1)
		sockErr = firstRealSocketError(ipv4Err, ipv6Err)
	}); err != nil {
		return err
	}
	return sockErr
}

func firstRealSocketError(errs ...error) error {
	for _, err := range errs {
		if err == nil || errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EAFNOSUPPORT) {
			continue
		}
		return err
	}
	return nil
}
