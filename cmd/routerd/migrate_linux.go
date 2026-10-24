//go:build linux

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/legacyconfig"
)

// migrate writes a complete native document only after both the input and any
// explicit replacement policy have compiled. Review notes go to diagnostics so
// redirecting stdout always produces one parseable native YAML document.
func migrate(args []string, output, diagnostics io.Writer) error {
	f := flag.NewFlagSet("config migrate", flag.ContinueOnError)
	f.SetOutput(diagnostics)
	policyPath := f.String("policy", "", "explicit legacy resource migration policy")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("usage: routerd config migrate [--policy FILE] FILE")
	}
	file, err := os.Open(f.Arg(0))
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, config.MaxSize+1))
	if err != nil {
		return err
	}
	if len(data) > config.MaxSize {
		return errors.New("configuration exceeds 2 MiB")
	}
	var result legacyconfig.Result
	if *policyPath == "" {
		result, err = legacyconfig.Compile(data)
	} else {
		policy, openErr := os.Open(*policyPath)
		if openErr != nil {
			return openErr
		}
		defer policy.Close()
		options, decodeErr := legacyconfig.DecodeOptions(policy)
		if decodeErr != nil {
			return decodeErr
		}
		result, err = legacyconfig.CompileWithOptions(data, options)
	}
	if err != nil {
		return err
	}
	yaml, err := config.YAML(result.Document)
	if err != nil {
		return err
	}
	var notes bytes.Buffer
	for _, mapping := range result.Mappings {
		for _, note := range mapping.Notes {
			// Quote names and notes: a legacy identifier must not inject terminal
			// controls or forge a second diagnostic line.
			fmt.Fprintf(&notes, "migration %q: %q\n", mapping.Resource, note)
		}
	}
	if _, err := io.Copy(diagnostics, &notes); err != nil {
		return fmt.Errorf("write migration review notes: %w", err)
	}
	_, err = output.Write(yaml)
	return err
}
