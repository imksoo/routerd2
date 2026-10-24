//go:build linux

package main

import (
	"errors"
	"io"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
)

// planConfig reads only the named source document. Compilation is pure: key
// files, kernel state, journals, and running workers do not participate. A plan
// is emitted only after the complete program and cross-worker constraints pass.
func planConfig(args []string, output io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: routerd config plan FILE")
	}
	source, err := config.Read(args[0])
	if err != nil {
		return err
	}
	plan, err := config.Compile(source)
	if err != nil {
		return err
	}
	return cli.WriteJSON(output, plan)
}
