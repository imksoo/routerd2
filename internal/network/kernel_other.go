//go:build !linux

package network

import (
	"errors"
	"os"
)

func newKernel() (kernel, error) { return nil, errors.New("routerd2 networking requires Linux") }
func lockJournal(*os.File) error { return errors.New("routerd2 networking requires Linux") }
