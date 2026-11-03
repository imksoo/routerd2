//go:build !linux

package management

import "net"

func sshListenConfig() net.ListenConfig { return net.ListenConfig{} }
