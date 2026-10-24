// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

// routerd-bundle is an offline release publisher. It is intentionally excluded
// from both runtime profiles and has no access to a router's release store.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/imksoo/routerd2/internal/releasepublish"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := releasepublish.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
