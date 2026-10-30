//go:build linux

package main

import (
	"errors"
	"io"
	"os"

	"github.com/imksoo/routerd2/internal/cli"
	"github.com/imksoo/routerd2/internal/config"
	"github.com/imksoo/routerd2/internal/dhcplease"
)

func importDHCPLeases(args []string, output io.Writer) error {
	f := flags("dhcp import-leases")
	path := f.String("config", "", "native configuration for the new router")
	service := f.String("service", "", "DHCP service name")
	stateDir := f.String("state-dir", "", "new router's persistent state directory")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *path == "" || *service == "" || *stateDir == "" {
		return errors.New("usage: routerd dhcp import-leases --config FILE --service NAME --state-dir DIR LEASE_FILE")
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
		input, err := os.Open(f.Arg(0))
		if err != nil {
			return err
		}
		defer input.Close()
		data, err := io.ReadAll(io.LimitReader(input, (8<<20)+1))
		if err != nil {
			return err
		}
		cfg := s.DHCPServerConfig(*stateDir)
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
	return errors.New("configuration has no DHCP server with the selected name")
}
