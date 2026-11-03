//go:build linux

package management

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func sshListenConfig() net.ListenConfig {
	// Management starts alongside the network worker. Bind the configured
	// address before it is installed, without exposing a wildcard listener.
	return net.ListenConfig{Control: func(network, _ string, c syscall.RawConn) error {
		level, option := unix.IPPROTO_IP, unix.IP_FREEBIND
		if network == "tcp6" {
			level, option = unix.IPPROTO_IPV6, unix.IPV6_FREEBIND
		}
		var socketErr error
		if err := c.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptInt(int(fd), level, option, 1)
		}); err != nil {
			return err
		}
		return socketErr
	}}
}
