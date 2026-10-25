//go:build !linux

package management

import (
	"context"
	"errors"
	"os"
)

func publishHostIdentity(context.Context, *os.File, string, []byte, func(string) error) error {
	return errors.New("host identity publication requires Linux")
}
