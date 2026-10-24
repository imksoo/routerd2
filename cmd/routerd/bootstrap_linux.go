//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/imksoo/routerd2/internal/boot"
	"github.com/imksoo/routerd2/internal/kmod"
	"github.com/imksoo/routerd2/internal/runtimeconfig"
	"github.com/imksoo/routerd2/internal/storage"
)

// bootstrapRuntime prepares the same prerequisites for a factory boot, a
// selected signed image, and a runtime without release selection. Each step
// recognizes its existing state, so exec of a selected image preserves the
// mounted volume and repeated entry never stacks another mount.
func bootstrapRuntime(ctx context.Context, b runtimeconfig.Boot) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if err := boot.Bootstrap(boot.Options{Platform: b.Platform, RuntimeDir: b.RuntimeDir, StateDir: b.StateDir}); err != nil {
		return err
	}
	// Block and filesystem drivers must be present before Mount waits for its
	// explicit device. No supervisor, release store or config writer starts
	// until the persistent state directory has been verified.
	if err := kmod.Load(b.KernelModules); err != nil {
		return err
	}
	if b.StateVolume == nil {
		return nil
	}
	status, err := storage.Mount(ctx, storage.Options{
		Platform: b.Platform, Device: b.StateVolume.Device, Target: b.StateDir,
		WaitTimeout: time.Duration(b.StateVolume.WaitTimeoutSeconds) * time.Second,
	})
	if err != nil {
		// A post-mount verification or fsync failure must stop startup too.
		// Do not fall back to volatile state or try to unmount a changed path.
		return fmt.Errorf("prepare persistent state volume (mounted=%t): %w", status.Mounted, err)
	}
	slog.Info("persistent state volume ready", "device", status.Device, "target", status.Target,
		"filesystem", status.FileSystem, "uuid", status.UUID, "already_mounted", status.AlreadyMounted)
	return nil
}
