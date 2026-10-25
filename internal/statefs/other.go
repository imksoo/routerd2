//go:build !linux

package statefs

import (
	"context"
	"errors"
	"os"
)

func OpenDirectory(string, bool) (*os.File, error) {
	return nil, errors.New("protected state requires Linux")
}
func OpenParent(string, bool) (*os.File, string, error) {
	return nil, "", errors.New("protected state requires Linux")
}
func ReadPrivate(context.Context, *os.File, string, int64) ([]byte, error) {
	return nil, errors.New("protected state requires Linux")
}
