//go:build linux

package main

import (
	"strings"
	"testing"
)

func TestLoggingCommandRejectsOtherRolesAndUnmanagedExecution(t *testing.T) {
	for _, args := range [][]string{{}, {"worker", "--role=network", "--name=network"}, {"worker", "--role=logging", "--name=logging", "--instance-config=/arbitrary"}} {
		if err := run(args); err == nil {
			t.Fatal("unscoped logging command accepted", args)
		}
	}
	t.Setenv("ROUTERD_RUNTIME_DIR", "")
	if err := run([]string{"worker", "--role=logging", "--name=logging"}); err == nil || !strings.Contains(err.Error(), "supervisor") {
		t.Fatal("unmanaged logger started", err)
	}
}
