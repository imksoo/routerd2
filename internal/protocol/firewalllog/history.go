// SPDX-License-Identifier: BSD-3-Clause

package firewalllog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/firewalllogconfig"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
)

func openHistory(path string) (*logstore.FirewallLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("firewall history is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	return logstore.OpenFirewallLog(path)
}

// This existing collector owns both insertion and expiry. No new rows can land
// between exportExpired and DELETE, so every deleted row passed the archive
// callback. The bounded queue keeps disk/receiver waits off the NFLOG reader.
func retainHistory(ctx context.Context, cfg firewalllogconfig.Config, db *logstore.FirewallLog, entries <-chan logstore.FirewallLogEntry, dropped *atomic.Uint64) {
	batch := make([]logstore.FirewallLogEntry, 0, 128)
	flush := func(ctx context.Context) {
		if count := dropped.Swap(0); count != 0 {
			slog.Warn("firewall history queue overflow", "dropped", count)
		}
		if len(batch) == 0 {
			return
		}
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := db.RecordBatch(call, batch)
		cancel()
		if err != nil {
			slog.Warn("firewall history batch failed", "records", len(batch), "error", err)
		}
		batch = batch[:0]
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var expiry <-chan time.Time
	var timer *time.Timer
	if cfg.Retention != nil {
		timer = time.NewTimer(0)
		defer timer.Stop()
		expiry = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for {
				select {
				case entry := <-entries:
					batch = append(batch, entry)
					if len(batch) == cap(batch) {
						flush(stop)
					}
				default:
					flush(stop)
					return
				}
			}
		case entry := <-entries:
			batch = append(batch, entry)
			if len(batch) == cap(batch) {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-expiry:
			flush(ctx)
			result, err := expireHistory(ctx, cfg)
			if ctx.Err() != nil {
				continue
			}
			if err != nil {
				slog.Warn("firewall history retention failed", "error", err)
			} else if result.Deleted > 0 {
				slog.Info("firewall history expired", "deleted", result.Deleted, "vacuumed", result.Vacuumed)
			}
			timer.Reset(cfg.Retention.Interval())
		}
	}
}

func expireHistory(ctx context.Context, cfg firewalllogconfig.Config) (logstore.RetentionResult, error) {
	policy := cfg.Retention
	target := logstore.RetentionTarget{File: cfg.HistoryPath, Retention: time.Duration(policy.MaxAgeSeconds) * time.Second, Signals: []string{logstore.RetentionSignalFirewallEvents}}
	if len(policy.Syslog) > 0 {
		target.BeforeDelete = func(row logstore.RetentionRecord) error {
			// This is the existing SQL export boundary; preserve its original
			// timestamp and columns without a second application representation.
			body, err := json.Marshal(row.Values)
			if err != nil {
				return err
			}
			record := logging.Record{Entry: logging.Entry{Timestamp: time.Now().UTC(), Module: "firewall_log", Level: "INFO", Message: "firewallEvents " + string(body)}}
			for _, destination := range policy.Syslog {
				if err := syslog.Send(ctx, destination, []logging.Record{record}); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return logstore.ApplyRetention(ctx, target, policy.Vacuum)
}
