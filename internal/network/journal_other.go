//go:build !linux

package network

import (
	"errors"
	"os"
)

func openNetworkJournalParent(string, bool) (*os.File, string, error) {
	return nil, "", errors.New("network journal requires Linux")
}
func openNetworkJournalFile(*os.File, string, bool) (*os.File, error) {
	return nil, errors.New("network journal requires Linux")
}
func writeNetworkJournal(*os.File, string, []byte) (bool, error) {
	return false, errors.New("network journal requires Linux")
}
