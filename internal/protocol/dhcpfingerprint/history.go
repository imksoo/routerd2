// SPDX-License-Identifier: BSD-3-Clause

// Package dhcpfingerprint retains observations from the owning DHCP engine.
package dhcpfingerprint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	old "github.com/imksoo/routerd/pkg/dhcpfingerprint"
	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/dhcpevent"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
	"github.com/imksoo/routerd2/internal/retention"
)

// Run is the sole writer and expiry owner. The DHCP notification path only
// enqueues; storage and archive waits cannot delay a lease notification reply.
func Run(ctx context.Context, path string, policy retention.Config, input <-chan dhcpevent.Event, dropped *atomic.Uint64) {
	var db *logstore.DHCPFingerprintLog
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	lastError := ""
	report := func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message == lastError {
			return
		}
		lastError = message
		if err != nil {
			slog.Warn("DHCP fingerprint history failed", "error", err)
		} else {
			slog.Info("DHCP fingerprint history recovered")
		}
	}
	var err error
	db, err = openHistory(path)
	report(err)
	initialSweep := time.Duration(0)
	if err != nil {
		initialSweep = time.Second
	}
	write := func(ctx context.Context, event dhcpevent.Event) {
		if db == nil {
			dropped.Add(1)
			return
		}
		if count := dropped.Swap(0); count != 0 {
			slog.Warn("DHCP fingerprint observations dropped", "records", count)
		}
		match := old.Infer(old.Fingerprint{MAC: event.MAC, Hostname: event.Hostname, VendorClass: event.VendorClass, RequestedOptions: event.RequestedOptions})
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := db.Upsert(call, logstore.DHCPFingerprint{MAC: event.MAC, Hostname: event.Hostname, VendorClass: event.VendorClass, RequestedOptions: event.RequestedOptions, OSFamily: match.OSFamily, DeviceClass: match.DeviceClass, DeviceName: match.DeviceName, Confidence: match.Confidence, Signal: match.Signal, ObservedAt: event.At, Source: "dnsmasq-dhcp-event"})
		cancel()
		report(err)
	}
	timer := time.NewTimer(initialSweep)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			for stop.Err() == nil {
				select {
				case event := <-input:
					write(stop, event)
				default:
					return
				}
			}
			return
		case event := <-input:
			write(ctx, event)
		case <-timer.C:
			if count := dropped.Swap(0); count != 0 {
				slog.Warn("DHCP fingerprint observations dropped", "records", count)
			}
			if db == nil {
				var err error
				db, err = openHistory(path)
				report(err)
				if err != nil {
					timer.Reset(time.Second)
					continue
				}
			}
			call, cancel := context.WithTimeout(ctx, 2*time.Second)
			result, err := expire(call, path, policy)
			cancel()
			report(err)
			if err == nil && result.Deleted > 0 {
				slog.Info("DHCP fingerprint history expired", "deleted", result.Deleted)
			}
			timer.Reset(policy.Interval())
		}
	}
}

func openHistory(path string) (*logstore.DHCPFingerprintLog, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("DHCP fingerprint history is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return logstore.OpenDHCPFingerprintLog(path)
}

func expire(ctx context.Context, path string, policy retention.Config) (logstore.RetentionResult, error) {
	target := logstore.RetentionTarget{File: path, Retention: time.Duration(policy.MaxAgeSeconds) * time.Second, Signals: []string{logstore.RetentionSignalDHCPFingerprints}}
	if len(policy.Syslog) > 0 {
		target.BeforeDelete = func(row logstore.RetentionRecord) error {
			body, err := json.Marshal(row.Values)
			if err != nil {
				return err
			}
			record := logging.Record{Entry: logging.Entry{Timestamp: time.Now().UTC(), Module: "dhcp_fingerprint", Level: "INFO", Message: "dhcpFingerprints " + string(body)}}
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
