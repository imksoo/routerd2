// SPDX-License-Identifier: BSD-3-Clause
package dns

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/dnsconfig"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
)

// Keep the old expiry/archive/vacuum semantics with the existing DNS owner.
// Maintenance uses its own SQLite connection; it never holds the query writer
// lock or blocks DNS listeners on an archive destination.
func (d *daemon) retainQueries(ctx context.Context, policy dnsconfig.QueryRetention) {
	target := logstore.RetentionTarget{File: d.config.Spec.QueryLog.Path, Retention: time.Duration(policy.MaxAgeSeconds) * time.Second, Signals: []string{logstore.RetentionSignalDNSQueries}}
	if len(policy.Syslog) > 0 {
		target.BeforeDelete = func(row logstore.RetentionRecord) error {
			// The old logstore callback is the dynamic SQL boundary. Keep its
			// column values in the archive body, including the original ts.
			body, err := json.Marshal(row.Values)
			if err != nil {
				return err
			}
			record := logging.Record{Entry: logging.Entry{Timestamp: time.Now().UTC(), Module: d.opts.resource, Level: "INFO", Message: "dnsQueries " + string(body)}}
			for _, destination := range policy.Syslog {
				if err := syslog.Send(ctx, destination, []logging.Record{record}); err != nil {
					return err
				}
			}
			return nil
		}
	}
	interval := 24 * time.Hour
	if policy.IntervalSeconds != 0 {
		interval = time.Duration(policy.IntervalSeconds) * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		result, err := logstore.ApplyRetention(ctx, target, policy.Vacuum)
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		d.retentionDeleted += result.Deleted
		d.retentionError = ""
		if err != nil {
			d.retentionError = err.Error()
		}
		d.mu.Unlock()
		if err != nil {
			slog.Warn("DNS query retention failed", "resource", d.opts.resource, "error", err)
		}
		timer.Reset(interval)
	}
}
