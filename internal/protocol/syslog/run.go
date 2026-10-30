// SPDX-License-Identifier: BSD-3-Clause

// Package syslog forwards the existing local log stream over best-effort UDP.
package syslog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/syslogconfig"
)

const ioTimeout = time.Second

// Run begins at the current log cursor, without replaying retained history.
// A temporary API outage resumes that cursor. A new logger epoch begins at its
// latest record, so recovered local history is not resent. UDP failures drop the
// attempted page; there is no second journal or remote delivery acknowledgement.
func Run(ctx context.Context, cfg syslogconfig.Config, runtimeDir string, ready func()) error {
	cfg = syslogconfig.Normalize(cfg)
	if err := syslogconfig.Validate(cfg); err != nil {
		return err
	}
	client := localapi.NewClient(filepath.Join(runtimeDir, logging.SocketName))
	defer client.HTTP.CloseIdleConnections()
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("syslog hostname: %w", err)
	}
	output := sender{config: cfg, hostname: hostname}
	defer output.close()
	var cursor logging.Cursor
	var queryFailed, deliveryFailed bool
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		page, err := query(ctx, client, cfg.MinLevel, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !queryFailed {
				slog.Warn("syslog local log query failed", "error", err)
				queryFailed = true
			}
			timer.Reset(time.Second)
			continue
		}
		if queryFailed {
			slog.Info("syslog local log queries resumed")
			queryFailed = false
		}
		if cursor.Epoch == "" || page.Reset {
			first := cursor.Epoch == ""
			cursor = logging.Cursor{Epoch: page.Cursor.Epoch, Sequence: page.LatestSequence}
			if first && ready != nil {
				ready()
			}
			timer.Reset(250 * time.Millisecond)
			continue
		}
		// Diagnostics from this forwarding process must not feed its own UDP
		// failure/recovery reporting loop. The cursor still advances over them.
		records := page.Records[:0]
		for _, record := range page.Records {
			if record.PID != os.Getpid() {
				records = append(records, record)
			}
		}
		err = output.send(ctx, records)
		// Advance even when UDP fails. Local retention remains authoritative.
		cursor = page.Cursor
		if ctx.Err() != nil {
			return nil
		}
		delay := 250 * time.Millisecond
		if err != nil {
			output.close()
			if !deliveryFailed {
				slog.Warn("syslog UDP send failed; remote delivery is best effort", "error", err)
				deliveryFailed = true
			}
			delay = time.Second
		} else if len(records) > 0 {
			if deliveryFailed {
				slog.Info("syslog UDP writes resumed")
				deliveryFailed = false
			}
			if page.More {
				delay = 0
			}
		}
		timer.Reset(delay)
	}
}

func query(ctx context.Context, client *localapi.Client, level string, cursor logging.Cursor) (logging.QueryResult, error) {
	level = strings.ToUpper(level)
	if level == "WARNING" {
		level = "WARN"
	}
	values := url.Values{"limit": {strconv.Itoa(logging.MaxQuery)}, "level": {level}}
	if cursor.Epoch == "" {
		values.Set("tail", "true")
		values.Set("limit", "1")
	} else {
		values.Set("epoch", cursor.Epoch)
		values.Set("after", strconv.FormatUint(cursor.Sequence, 10))
	}
	ctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	var result logging.QueryResult
	err := client.Do(ctx, http.MethodGet, "/v1/logs?"+values.Encode(), nil, &result)
	return result, err
}

type sender struct {
	config   syslogconfig.Config
	hostname string
	conn     net.Conn
}

func (s *sender) close() {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func (s *sender) send(ctx context.Context, records []logging.Record) error {
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.conn == nil {
			dialer := net.Dialer{Timeout: ioTimeout}
			conn, err := dialer.DialContext(ctx, "udp", s.config.Address)
			if err != nil {
				return err
			}
			s.conn = conn
		}
		message, err := formatRecord(record)
		if err != nil {
			return err
		}
		deadline := time.Now().Add(ioTimeout)
		if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
			deadline = until
		}
		if err := s.conn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		// Keep log/syslog's network format. Its Writer cannot accept a context
		// or write deadline, and Close waits behind its potentially blocked write.
		_, err = fmt.Fprintf(s.conn, "<%d>%s %s %s[%d]: %s\n",
			s.config.FacilityPriority()|severity(record.Level),
			time.Now().Format(time.RFC3339), s.hostname, s.config.Tag, os.Getpid(), message)
		if err != nil {
			return err
		}
	}
	return nil
}

// Preserve routerd's level/message prefix while retaining the original producer
// and local cursor. Header time/PID describe forwarding, not event production.
func formatRecord(record logging.Record) (string, error) {
	level := strings.ToLower(record.Level)
	if level == "warn" {
		level = "warning"
	}
	message := fmt.Sprintf("%s %s timestamp=%s module=%s pid=%d generation=%d epoch=%s sequence=%d",
		level, record.Message, record.Timestamp.Format(time.RFC3339Nano), record.Module,
		record.PID, record.Generation, record.Epoch, record.Sequence)
	if len(record.Attributes) > 0 {
		attributes, err := json.Marshal(record.Attributes)
		if err != nil {
			return "", fmt.Errorf("encode syslog attributes: %w", err)
		}
		message += " attributes=" + string(attributes)
	}
	return message, nil
}

func severity(level string) int {
	switch level {
	case "DEBUG":
		return 7
	case "WARN":
		return 4
	case "ERROR":
		return 3
	default:
		return 6
	}
}

// Send archives a bounded historical batch using the same best-effort UDP wire
// format. Successful Write is local submission, never a remote acknowledgement.
func Send(ctx context.Context, cfg syslogconfig.Config, records []logging.Record) error {
	cfg = syslogconfig.Normalize(cfg)
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	output := sender{config: cfg, hostname: host}
	defer output.close()
	minimum := severity(strings.ToUpper(cfg.MinLevel))
	if cfg.MinLevel == "warning" {
		minimum = 4
	}
	selected := make([]logging.Record, 0, len(records))
	for _, record := range records {
		if severity(record.Level) <= minimum {
			selected = append(selected, record)
		}
	}
	return output.send(ctx, selected)
}
