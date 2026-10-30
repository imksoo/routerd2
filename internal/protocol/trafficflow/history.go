// SPDX-License-Identifier: BSD-3-Clause

package trafficflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/logging"
	"github.com/imksoo/routerd2/internal/protocol/syslog"
	"github.com/imksoo/routerd2/internal/protocol/trafficflowconfig"
)

func openHistory(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("traffic flow history is not a regular file")
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA wal_autocheckpoint = 1000;
PRAGMA journal_size_limit = 33554432;
`+trafficSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func expireHistory(ctx context.Context, cfg trafficflowconfig.Config) (logstore.RetentionResult, error) {
	policy := cfg.Retention
	target := logstore.RetentionTarget{File: cfg.HistoryPath, Retention: time.Duration(policy.MaxAgeSeconds) * time.Second, Signals: []string{logstore.RetentionSignalTrafficFlows}}
	if len(policy.Syslog) > 0 {
		target.BeforeDelete = func(row logstore.RetentionRecord) error {
			// This is the existing SQL export boundary; preserve its original
			// timestamp and columns without a second application representation.
			body, err := json.Marshal(row.Values)
			if err != nil {
				return err
			}
			record := logging.Record{Entry: logging.Entry{Timestamp: time.Now().UTC(), Module: "traffic_flow", Level: "INFO", Message: "trafficFlows " + string(body)}}
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

// Schema and counter updates retain routerd pkg/logstore/traffic.go. This new
// service owns a fresh database, so historical ALTER migrations are unnecessary.
const trafficSchema = `CREATE TABLE IF NOT EXISTS flows (
  id INTEGER PRIMARY KEY,
  flow_key TEXT UNIQUE NOT NULL,
  ts_started INTEGER NOT NULL,
  ts_ended INTEGER,
  client_address TEXT,
  client_port INTEGER,
  peer_address TEXT,
  peer_port INTEGER,
  protocol TEXT NOT NULL,
  nat_translated_address TEXT,
  accounting INTEGER,
  bytes_out INTEGER,
  bytes_in INTEGER,
  packets_out INTEGER,
  packets_in INTEGER,
  app_name TEXT,
  app_category TEXT,
  app_confidence INTEGER,
  detected_protocol TEXT,
  master_protocol TEXT,
  application_protocol TEXT,
  category TEXT,
  risk TEXT,
  confidence INTEGER,
  metadata_json TEXT,
  engine TEXT,
  source TEXT,
  tls_sni TEXT,
  http_host TEXT,
  dns_query TEXT,
  resolved_hostname TEXT
);
CREATE INDEX IF NOT EXISTS flows_client_ts ON flows(client_address, ts_started);
CREATE INDEX IF NOT EXISTS flows_peer_ts ON flows(peer_address, ts_started);
`

// Like routerd SyncActive, the sample and missing-connection updates commit
// together. One JSON array avoids SQLite's per-statement parameter limit;
// splitting samples would incorrectly end connections in another chunk.
func syncActive(ctx context.Context, db *sql.DB, flows []logstore.TrafficFlow, active []string, now time.Time) error {
	keys, err := json.Marshal(active)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO flows(flow_key,ts_started,client_address,client_port,peer_address,peer_port,protocol,nat_translated_address,accounting,bytes_out,bytes_in,packets_out,packets_in)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(flow_key) DO UPDATE SET
 ts_ended = NULL,
 client_address = excluded.client_address,
 client_port = excluded.client_port,
 peer_address = excluded.peer_address,
 peer_port = excluded.peer_port,
 protocol = excluded.protocol,
 nat_translated_address = excluded.nat_translated_address,
 accounting = excluded.accounting,
 bytes_out = excluded.bytes_out,
 bytes_in = excluded.bytes_in,
 packets_out = excluded.packets_out,
 packets_in = excluded.packets_in`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, flow := range flows {
		if _, err := stmt.ExecContext(ctx, flow.FlowKey, flow.StartedAt.UnixNano(), flow.ClientAddress, flow.ClientPort, flow.PeerAddress, flow.PeerPort, flow.Protocol, flow.NATTranslatedAddress, flow.Accounting, flow.BytesOut, flow.BytesIn, flow.PacketsOut, flow.PacketsIn); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE flows SET ts_ended = ? WHERE ts_ended IS NULL AND flow_key NOT IN (SELECT value FROM json_each(?))`, now.UnixNano(), string(keys)); err != nil {
		return err
	}
	return tx.Commit()
}
