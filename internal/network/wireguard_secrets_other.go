//go:build !linux

package network

import (
	"errors"
	"os"
)

func privateFileOwner(os.FileInfo) bool { return false }

func openWireGuardSecretFile(string) (*os.File, error) {
	return nil, errors.New("WireGuard secrets require Linux")
}
func openWireGuardSecretDirectory(string) (*os.File, error) {
	return nil, errors.New("WireGuard secrets require Linux")
}
func prepareWireGuardSecretDirectory(string) error {
	return errors.New("WireGuard secrets require Linux")
}
func publishWireGuardSecretFile(string, []byte, func(string) error) error {
	return errors.New("WireGuard secrets require Linux")
}
