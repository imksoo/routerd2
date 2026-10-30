// SPDX-License-Identifier: BSD-3-Clause
//go:build linux

package dhcplease

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/imksoo/routerd/pkg/logstore"
	"github.com/imksoo/routerd2/internal/protocol/dhcpserverconfig"
	"golang.org/x/sys/unix"
)

// ImportSticky reads the old routerd database, including its SQLite WAL, and
// installs this service's IPv4 history before its first startup. The worker
// already uses routerd's schema and hold rules, so timestamps need no conversion.
func ImportSticky(ctx context.Context, cfg dhcpserverconfig.Config, source string) (int, error) {
	if err := dhcpserverconfig.Validate(cfg); err != nil {
		return 0, err
	}
	cfg = dhcpserverconfig.Normalize(cfg)
	if cfg.StickyHoldDays == 0 {
		return 0, errors.New("DHCP service does not enable sticky holds")
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}).String())
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `SELECT mac,ip,coalesce(hostname,''),allocated_at,released_at,last_request_at,sticky_until FROM dhcp_sticky_leases`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	dir, lock, err := lockState(cfg.StatePath)
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	defer lock.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(cfg.StatePath)+".lock" && entry.Name() != filepath.Base(cfg.StatePath) {
			return 0, errors.New("sticky import requires a DHCP service that has not started")
		}
	}
	stage, err := os.MkdirTemp(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()), ".sticky-import-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(stage)
	path := filepath.Join(stage, "dhcp-sticky.db")
	log, err := logstore.OpenDHCPStickyLog(path)
	if err != nil {
		return 0, err
	}
	if err := log.Close(); err != nil {
		return 0, err
	}
	target, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer target.Close()
	target.SetMaxOpenConns(1)
	// Publish one complete database file; no uncheckpointed WAL may be left
	// behind in the private staging directory when it is removed.
	if _, err := target.ExecContext(ctx, "PRAGMA journal_mode = DELETE"); err != nil {
		return 0, err
	}
	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count := 0
	for rows.Next() {
		var macText, ipText, hostname string
		var allocated, released, requested, until int64
		if err := rows.Scan(&macText, &ipText, &hostname, &allocated, &released, &requested, &until); err != nil {
			return 0, err
		}
		ip, err := netip.ParseAddr(ipText)
		if err != nil {
			return 0, fmt.Errorf("invalid sticky address: %w", err)
		}
		if !ip.Is4() {
			continue
		}
		inside := netip.MustParsePrefix(cfg.Primary.Prefix).Contains(ip)
		for _, scope := range cfg.Scopes {
			inside = inside || netip.MustParsePrefix(scope.Prefix).Contains(ip)
		}
		// The old log is shared by servers and address families.
		if !inside || ip.String() == cfg.ServerAddress {
			continue
		}
		mac, err := net.ParseMAC(macText)
		if err != nil {
			return 0, err
		}
		if allocated < 0 || released < 0 || requested < 0 || until < 0 || released == 0 && requested == 0 {
			return 0, errors.New("invalid sticky history timestamps")
		}
		if strings.ContainsAny(hostname, "\x00\r\n") {
			hostname = ""
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dhcp_sticky_leases(mac,ip,hostname,family,allocated_at,released_at,last_request_at,sticky_until) VALUES(?,?,?,'ipv4',?,?,?,?)`, mac.String(), ip.String(), hostname, allocated, released, requested, until); err != nil {
			return 0, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := target.Close(); err != nil {
		return 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return 0, err
	}
	if err := unix.Renameat2(int(dir.Fd()), filepath.Join(filepath.Base(stage), "dhcp-sticky.db"), int(dir.Fd()), "dhcp-sticky.db", unix.RENAME_NOREPLACE); err != nil {
		return 0, err
	}
	return count, dir.Sync()
}
