//go:build linux

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/dhcplease"
)

func importDHCPState(operation string, args []string, output io.Writer) error {
	f := flags("dhcp " + operation)
	path := f.String("config", "", "native configuration for the new router")
	service := f.String("service", "", "DHCP service name")
	stateDir := f.String("state-dir", "", "new router's persistent state directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *path == "" || *service == "" || *stateDir == "" {
		return fmt.Errorf("usage: routerd dhcp %s --config FILE --service NAME --state-dir DIR SOURCE_FILE", operation)
	}
	doc, err := config.Read(*path)
	if err != nil {
		return err
	}
	plan, err := config.Compile(doc)
	if err != nil {
		return err
	}
	for _, s := range plan.Native.Services {
		if s.Name != *service || s.DHCPServer == nil {
			continue
		}
		cfg := s.DHCPServerConfig(*stateDir)
		if operation == "import-sticky" {
			count, err := dhcplease.ImportSticky(context.Background(), cfg, f.Arg(0))
			if err != nil {
				return err
			}
			return cli.WriteJSON(output, struct {
				Service   string `json:"service"`
				Records   int    `json:"records"`
				StatePath string `json:"state_path"`
			}{s.Name, count, filepath.Join(filepath.Dir(cfg.StatePath), "dhcp-sticky.db")})
		}
		input, err := os.Open(f.Arg(0))
		if err != nil {
			return err
		}
		defer input.Close()
		data, err := io.ReadAll(io.LimitReader(input, (8<<20)+1))
		if err != nil {
			return err
		}
		count, err := dhcplease.Import(cfg, data)
		if err != nil {
			return err
		}
		return cli.WriteJSON(output, struct {
			Service   string `json:"service"`
			Leases    int    `json:"leases"`
			StatePath string `json:"state_path"`
		}{s.Name, count, cfg.StatePath})
	}
	return fmt.Errorf("configuration has no DHCP server named %q", *service)
}
